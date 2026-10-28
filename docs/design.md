# Design

## Goal

Run unmodified OpenFGA server code with every datastore access going through a
transaction that the embedding application began and will commit or roll back.
OpenFGA must not open a connection or transaction of its own, and must not
leak state between transactions.

## Routing statements into the caller's transaction

OpenFGA's SQLite datastore (and its shared `sqlcommon` helpers) are written
against `*sql.DB`. They query through pooled connections, iterate
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
  A transaction is one connection. PostgreSQL cannot start a statement while
  another's rows are still streaming. OpenFGA iterators are
  consumed concurrently from many goroutines, so streaming would deadlock or
  fail with "conn busy".
- **`BeginTx`** becomes `SAVEPOINT atomicfga_N`; `Commit` becomes `RELEASE
  SAVEPOINT`; `Rollback` becomes `ROLLBACK TO SAVEPOINT` followed by `RELEASE
  SAVEPOINT`. The session is held exclusively for the savepoint's lifetime, so
  concurrent readers in the same session neither observe nor get rolled back
  with a partially applied write. Savepoints nest correctly inside the
  application's own savepoints.
- **Cancellation** is stripped before statements reach the application's
  driver (`context.WithoutCancel`). pgx closes the connection when a running
  statement's context is canceled. That would destroy the application's transaction whenever OpenFGA
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

## One state per transaction

Applications naturally wrap the same transaction in several places (a
repository layer, helpers, goroutines). Everything a session knows about its
transaction therefore lives in one shared `txState`: the statement queue and
semaphore, the aborted flag, and savepoint numbering. With separate state per
wrapper, two wrappers would send statements onto one connection
concurrently, and one would keep running statements after another saw the
transaction abort.

The engine keeps a registry from transaction to state. Its keys are weak
pointers to the transaction object (`weak.Pointer`), and the state holds no
reference to the transaction: each API call carries it. A
`runtime.AddCleanup` on the transaction removes the entry once the
transaction is unreachable. Weak pointers identify objects rather than
addresses, so a later transaction at a reused address gets fresh state.

Identity must belong to one transaction, so `SQLTx` and `PgxTx` refuse a
`*sql.DB` (statements would spread over pooled connections) and a `*sql.Conn`
or `*pgx.Conn` (a connection outlives its transactions: state, such as a
rolled-back model's visibility or an aborted flag, would carry over). For
SQLite and Turso writers, which previously needed a connection for `BEGIN
IMMEDIATE`, `BeginImmediate` returns a transaction object. `Session` panics
on a custom `Tx` without pointer identity. `TestSessionsOfOneTransaction*`
fail when the registry is disabled.

## Aborted transactions

When the database reports that the whole transaction is lost (SQLSTATE
`40001`, `40P01` or `25P02` on PostgreSQL; a busy error inside a SQLite or
Turso transaction), the session records it and refuses every further
statement. The call returns `ErrTransactionAborted` (gRPC `Aborted`). A
database that has rolled the transaction back would otherwise run the
statements that follow outside it, committing them one by one.

## Turso

The Turso engine runs OpenFGA's SQLite datastore. Three differences from
SQLite needed handling, each found by the upstream conformance suite or the
benchmarks:

- **Error types.** OpenFGA recognizes constraint violations only as
  `modernc.org/sqlite` errors. `bind.TursoErrors` wraps Turso's constraint
  errors so that `errors.As` also finds a genuine modernc constraint error
  (produced once from an in-memory database, since its fields are
  unexported), and treats Turso's busy errors as aborts. Errors are matched
  by message, so atomicfga does not link Turso's native library for others.
- **Expression depth.** Turso's parser limits expressions to depth 100, and
  OpenFGA deletes a write's tuples with one `OR` term per tuple. Deletes of
  more than 91 tuples fail, so Turso engines default to 64 tuples per write
  and refuse more than 90.
- **Planner statistics.** Without `sqlite_stat1`, Turso answers OpenFGA's
  point lookups through the `(store, ulid)` index, reading every tuple of the
  store: 7–12× slower than SQLite. Statistics of empty tables do not help.
  `Migrate` seeds fixed statistics of a large multi-store database (unless
  statistics exist), which Turso connections load when they open.

## Shared caches

The embedded server is shared by every transaction, so its caches are too.

- **Tuple-derived caches** (check query cache, check/ListObjects iterator
  caches, shared iterators, cache controller) would serve one transaction's
  uncommitted or rolled-back tuples to another. The engine forces them off.
- **Authorization models** are cached by store and model ID (the model cache,
  the type system cache and the weighted-graph cache), and concurrent lookups
  are coalesced with `singleflight`. A model written by an uncommitted
  transaction, or one that was rolled back, could therefore be served from
  cache to a transaction that cannot see it.

  Models are immutable and their IDs are unique ULIDs. A cached model is
  correct for any transaction in which its ID is *visible*. Before delegating,
  `Session` requires the request to name its model, and confirms the ID
  exists in the transaction (`SELECT 1 … LIMIT 1`), on every request. No
  answer is remembered: a model another transaction committed may postdate
  this one's snapshot, and the caller may roll back to a savepoint, without
  the session seeing it, after the session saw a model this transaction
  wrote. The server's "latest model" path (`FindLatestAuthorizationModel`
  through `singleflight`) is never used.

  A request naming no model fails with `ErrModelIDRequired`
  (InvalidArgument), where upstream would use the store's latest model: the
  greatest ID, an ordering of clock readings. A missing model is upstream's
  `authorization_model_not_found`. Request validation runs first, so invalid
  requests still get upstream's validation errors.

- **Model IDs.** Upstream mints a model's ID from the clock. atomicfga reads
  the store's greatest model ID in the write's transaction (on PostgreSQL
  after taking a per-store `pg_advisory_xact_lock`), and the datastore
  wrapper replaces a minted ID that is not greater with the greatest's
  millisecond plus one; upstream answers with the ID it handed the
  datastore. So the model written last always has the greatest ID, whatever
  the clocks.

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
