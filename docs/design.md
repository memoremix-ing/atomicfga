# Design

## Goal

Run unmodified OpenFGA server code with every datastore access going through a
transaction that the embedding application began and will commit or roll back.
OpenFGA must not open a connection or transaction of its own, and must not
leak state between transactions.

## Routing statements into the caller's transaction

OpenFGA's SQLite and MySQL datastores (and its shared `sqlcommon` helpers) are
written against `*sql.DB`. They query through pooled connections, iterate
results lazily, and call `BeginTx` for writes.

`internal/bind` provides a `*sql.DB` backed by a loopback driver. Its
connections are stateless forwarders. `QueryContext` and `ExecContext` look up
the `Call` in the statement's context and execute on that call's session, which
wraps the application's transaction (`*sql.Tx` or `pgx.Tx`). The datastores are
constructed once per engine on this `*sql.DB`, and the transaction is chosen
per statement by context. That is why one `Engine` can serve any number of
concurrent transactions.

- **Arguments** pass through unchanged (`NamedValueChecker` accepts
  everything). The application's driver performs the only conversion, exactly
  as it would for OpenFGA.
- **Results** are read completely before the statement returns (`Result`).
  A transaction is one connection. PostgreSQL and MySQL cannot start a
  statement while another's rows are still streaming. OpenFGA iterators are
  consumed concurrently from many goroutines, so streaming would deadlock or
  fail with "conn busy".
- **`BeginTx`** becomes `SAVEPOINT atomicfga_N`; `Commit` becomes `RELEASE
  SAVEPOINT`; `Rollback` becomes `ROLLBACK TO SAVEPOINT` followed by `RELEASE
  SAVEPOINT`. The session is held exclusively for the savepoint's lifetime, so
  concurrent readers in the same session neither observe nor get rolled back
  with a partially applied write. Savepoints nest correctly inside the
  application's own savepoints.
- **Cancellation** is stripped before statements reach the application's
  driver (`context.WithoutCancel`). pgx closes the connection, and
  go-sql-driver/mysql kills it, when a running statement's context is
  canceled. That would destroy the application's transaction whenever OpenFGA
  cancels a losing branch of a union.

PostgreSQL is the exception: upstream's datastore uses `*pgxpool.Pool`
directly. `internal/pgstore` is that datastore re-expressed over `database/sql`.
Every query is built by the same squirrel builders as upstream. Writes call
`sqlcommon.Write`, whose statements equal upstream `postgres.write`: sorted,
batched `SELECT … FOR UPDATE` with a row-constructor `IN`, then batched
`DELETE`, tuple `INSERT` and changelog `INSERT`. The port is verified by
upstream's datastore conformance suite and by every differential test.

## Serializing a concurrent resolver onto one connection

OpenFGA resolves a check with many goroutines. Upstream they query in parallel
over pooled connections. Here, statements of one session are serialized by a
one-slot semaphore. Two mechanisms keep this from costing more than necessary:

- **Dropping abandoned work.** When a union finds a match, OpenFGA cancels
  the other branches but lets their goroutines run on. `sqlcommon` detaches
  query contexts from cancellation before they reach the driver. So
  `bind.PropagateCancellation` wraps the datastore and records each read's
  cancellation as a context *value*, which survives `WithoutCancel`. Queued
  statements of canceled branches are dropped before reaching the database.
- **Pipelining (pgx).** Queries that queue up while a round trip is in flight
  are sent together as one pgx batch in the next round trip. The first caller
  to find the session idle becomes the leader and drains the queue. Nothing
  waits for a batch to fill, so an isolated query is sent immediately.

## Ending a call

OpenFGA returns from `Check` while goroutines of abandoned branches may still
be about to query. Each session API call is a `bind.Call`. When it returns,
`Call.Finish` marks it done and acquires the session once. That waits for a
statement in flight and makes later statements of the call fail with
`ErrCallFinished` without reaching the transaction. Without this barrier, a
straggler could run after the application has resumed using its transaction,
or after commit. With pgx this failed with "conn busy".

## Aborted transactions

When the database reports that the whole transaction is lost (SQLSTATE
`40001`, `40P01`, `25P02`, or MySQL error 1213), the session records it and
refuses every further statement. The call returns `ErrTransactionAborted`
(gRPC `Aborted`). On MySQL a deadlock victim's transaction is rolled back and
the connection returns to autocommit. Without the refusal, the statements that
followed would silently commit one by one.

## Shared caches

The embedded server is shared by every transaction, so its caches are too.

- **Tuple-derived caches** (check query cache, check/ListObjects iterator
  caches, shared iterators, cache controller) would serve one transaction's
  uncommitted or rolled-back tuples to another. The engine forces them off.
- **Authorization models** are cached by store and model ID (the model cache,
  the type system cache and the weighted-graph cache), and concurrent lookups
  are coalesced with `singleflight`. A model written by an uncommitted
  transaction, or one that was rolled back, could therefore be served from
  cache to a transaction that cannot see it. Coalescing could even run one
  transaction's "latest model" lookup on another transaction's connection.

  Models are immutable and their IDs are unique ULIDs. A cached model is
  correct for any transaction in which its ID is *visible*. Before delegating,
  `Session` pins the request to a model ID that is visible in its transaction:
  - Without a model ID, it queries the latest model ID in the transaction
    (`ORDER BY authorization_model_id DESC LIMIT 1`) and sets it on a shallow
    copy of the request. The server's "latest model" path, the only one that
    reaches `FindLatestAuthorizationModel` through `singleflight`, is never
    used.
  - With a model ID, it confirms the ID exists in the transaction (`SELECT 1
    … LIMIT 1`), unless the engine already knows the model is committed. A
    model is known to be committed once a transaction that did not write it has
    seen it. Models are never deleted, so a committed model stays visible.

  Errors match upstream: `latest_authorization_model_not_found` and
  `authorization_model_not_found`. Request validation runs first, so invalid
  requests still get upstream's validation errors.

## Isolation of results from the original

The original server and atomicfga share all resolution code. The differences
that remain are deliberate:

| | OpenFGA server | atomicfga |
|---|---|---|
| Connections | pool per server | the caller's transaction |
| Write transaction | its own, `READ COMMITTED` | savepoint in the caller's transaction, caller's isolation |
| Tuple-derived caches | configurable | always off |
| Consistency preference | selects primary/replica, bypasses caches | no effect |
| `NOW()` (PostgreSQL) | statement's own transaction | caller's transaction start |
