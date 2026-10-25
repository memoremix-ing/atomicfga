# atomicfga

[OpenFGA](https://openfga.dev) embedded in a Go application that **owns the
database transaction**.

Upstream OpenFGA opens its own connections and commits every write in a
transaction of its own. atomicfga inverts that: the application begins a
transaction, binds it to a `Session`, and every OpenFGA operation made through
the session reads and writes inside that transaction. Relationship tuples,
authorization models and the application's own rows commit or roll back
together. Checks see the transaction's own uncommitted writes. A permission
check and the write that depends on it can be serialized by the database.

```go
engine, err := atomicfga.New(atomicfga.Postgres) // once per process
defer engine.Close()

tx, err := pool.Begin(ctx) // the application's pgx transaction
defer tx.Rollback(ctx)
fga := engine.Session(atomicfga.PgxTx(tx))

// Create a document and its owner atomically.
_, err = tx.Exec(ctx, "INSERT INTO documents (id, title) VALUES ($1, $2)", id, title)
_, err = fga.Write(ctx, &openfgav1.WriteRequest{StoreId: storeID,
	Writes: &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{
		{Object: "document:" + id, Relation: "owner", User: "user:" + uid}}}})

// Visible inside the transaction before commit.
resp, err := fga.Check(ctx, &openfgav1.CheckRequest{StoreId: storeID,
	TupleKey: &openfgav1.CheckRequestTupleKey{Object: "document:" + id, Relation: "can_edit", User: "user:" + uid}})

err = tx.Commit(ctx)
```

`Session` implements `openfgav1.OpenFGAServiceServer`: Check, BatchCheck,
ListObjects, StreamedListObjects, ListUsers, Expand, Read, Write, ReadChanges,
the store, authorization model and assertion APIs. Requests and responses are
OpenFGA's own protobuf messages, and errors are OpenFGA's gRPC status errors.

## What runs

atomicfga is a separate module that builds the sibling OpenFGA checkout
(`../../../openfga/openfga.git/lts-1`, see `upstream.json`) through a `replace`
directive. It does not modify that checkout. Everything above the SQL layer is
upstream code: the server, Check/ListObjects/ListUsers resolution, model and
tuple validation, conditions (CEL), and the SQLite datastore itself, which
also serves the Turso database engine. Supported databases are PostgreSQL,
SQLite and Turso.

- **`internal/bind`** is a `database/sql` driver whose connections hold no
  database state. Each statement is routed to the session found in its
  context and executed on the application's transaction. A datastore's
  `BeginTx` becomes a `SAVEPOINT`. OpenFGA's datastores therefore run
  unmodified, and issue exactly the SQL they normally issue.
- **`internal/pgstore`** is upstream's PostgreSQL datastore expressed over
  `database/sql`. Upstream's is hard-wired to `pgxpool.Pool`. The statements,
  locking and batching are the same; tuple writes use upstream's shared
  `sqlcommon.Write`.
- **`Session`** pins every request to an authorization model that is visible
  in the transaction before OpenFGA's process-wide model caches are consulted
  (see [design](docs/design.md#shared-caches)), detects transactions the
  database has aborted, and makes each call finish only when none of its
  statements is still running on the transaction.

## Transaction contract

1. Begin a transaction on the database that holds OpenFGA's schema and the
   application's tables: a `*sql.Tx`, a `pgx.Tx`, or (SQLite and Turso
   writers) an `*atomicfga.ImmediateTx` from `atomicfga.BeginImmediate`.
2. Wrap it whenever you need OpenFGA: `engine.Session(atomicfga.SQLTx(tx))` or
   `engine.Session(atomicfga.PgxTx(tx))`. Every session of one transaction
   shares the transaction's state, so wrap it in as many places as is
   convenient, and use sessions concurrently if you like: their statements
   are serialized onto the transaction. Do not use the transaction yourself
   *while* a session call is in progress. A call does not return until none
   of its statements is still running.
3. Commit or roll back the transaction yourself. atomicfga never begins,
   commits or rolls back it, and never uses another connection.

Sessions recognize a transaction by the identity of the transaction object,
and keep its state only while that object is reachable. That is why
`SQLTx`/`PgxTx` refuse a `*sql.DB`, `*sql.Conn` or `*pgx.Conn`: a pool
spreads statements over connections, and a connection outlives its
transactions, so neither identifies one. A custom `Tx` must be a pointer
type for the same reason.

Errors:

- A tuple write runs inside a savepoint. If it fails (for example a duplicate
  tuple, error code `write_failed_due_to_invalid_input`), its partial effects
  are rolled back and the transaction remains usable, on PostgreSQL too.
- If the database aborts the transaction (deadlock, serialization failure, or
  any PostgreSQL error outside a savepoint), the call returns an error matching
  `errors.Is(err, atomicfga.ErrTransactionAborted)` with gRPC code `Aborted`.
  On SQLite and Turso, a busy error inside a transaction counts too: a
  deferred transaction could not become a write transaction. The session
  refuses all further statements. Roll back and retry the whole transaction.
- Concurrent transactions inserting the same tuple fail as upstream does,
  with OpenFGA's own `Aborted` ("transactional write failed due to conflict").
  The failure is confined to the write's savepoint, but under `REPEATABLE
  READ`/`SERIALIZABLE` the simplest correct response is again to retry the
  transaction. In short: retry on gRPC code `Aborted`.
- Other errors are OpenFGA's own (validation, not found, …).

Isolation and locking:

- Writes take the same row locks as upstream (`SELECT … FOR UPDATE` on the
  affected tuples), and hold them until **your** transaction ends.
- To make a decision safe against concurrent revocation (check, then act on
  the result), use `SERIALIZABLE` on PostgreSQL, and write transactions
  (`BEGIN IMMEDIATE`) on SQLite and Turso. Retry aborted transactions.
  `TestHegelCheckThenAct` verifies this property.
- `REPEATABLE READ` is not enough. It gives each transaction a stable
  snapshot, but an edit can still commit after a revocation that did not see
  it, unless the two also write a common row.
  `TestCheckThenActNeedsSerializable` runs such a history at both levels.
- `SERIALIZABLE` guarantees that some serial order explains every committed
  transaction, not that it is commit order: an edit that overlaps a
  revocation may still be ordered before it.
- **SQLite and Turso:** begin transactions that write with
  `atomicfga.BeginImmediate(ctx, db)`, which takes the write lock up front as
  OpenFGA does (`BEGIN IMMEDIATE`) and behaves like `*sql.Tx`. tursogo's
  `BeginTx` is always deferred; for modernc SQLite, `_txlock=immediate` in the
  DSN works too. Read-only transactions may be deferred.

Drivers: PostgreSQL through pgx v5, either native (`pgx.Tx`) or
`database/sql` with `pgx/stdlib`. SQLite through
`modernc.org/sqlite`, OpenFGA's own driver, whose error types its datastore
relies on. Turso through `turso.tech/database/tursogo` with
`atomicfga.Turso`: atomicfga translates its constraint and busy errors (by
message, so applications that do not use Turso do not link its native
library).

Turso specifics, all covered by the test suites:

- Turso's parser limits expression depth to 100, and OpenFGA deletes a
  write's tuples with one `OR` per tuple. The Turso engine therefore defaults
  to 64 tuples per write (upstream: 100) and rejects more than 90.
- Without statistics, Turso's planner answers OpenFGA's point lookups by
  scanning every tuple in the store (7–12× slower). `Migrate` seeds
  `sqlite_stat1` with statistics shaped like a large multi-store database,
  unless statistics already exist; `ANALYZE` replaces them. Turso connections
  read statistics only when they open, so the first time, migrate through a
  separate `*sql.DB` (or before the application's connections open).

Schema: `atomicfga.Migrate(ctx, db, dialect)` applies OpenFGA's own embedded
migrations through goose, recording them in `goose_db_version` exactly as
`openfga migrate` does. Turso uses the SQLite migrations.

## Differences from an OpenFGA server

- **Caches that would share data between transactions are disabled:** the
  check query cache, the check and ListObjects iterator caches, shared
  iterators and the cache controller. These are also off by default upstream.
  The authorization model and type system caches stay on. They are safe
  because models are immutable and every request is first pinned to a model
  visible in its transaction.
- **Consistency preferences have no effect.** Every read sees the
  transaction's view. `HIGHER_CONSISTENCY` is accepted and ignored.
- **Timestamps and the changelog.** On PostgreSQL, `NOW()` is the
  transaction's start time. Tuples and changelog entries written in one
  transaction therefore share a timestamp, and `ReadChanges` inside a
  transaction does not return that transaction's own changes. Changelog ULIDs
  are assigned when written, not when committed, which is also true upstream
  but matters more for long transactions. A consumer of `ReadChanges` should
  use a horizon offset (`server.WithChangelogHorizonOffset`) longer than your
  longest transaction.
- No gRPC/HTTP listener, authentication, access control store, AuthZEN, or
  shadow resolvers. `Session` can be registered on your own gRPC server if
  you want one.

## Performance

`make bench` runs the original OpenFGA (upstream pooled datastore, its own
transactions) and atomicfga side by side on each dialect with the same
dataset, after checking that both return the same results. See
[results](benchmarks/RESULTS.md) and [method](benchmarks/METHOD.md). Inside an
existing transaction:

| | PostgreSQL, pgx | PostgreSQL, `database/sql` | SQLite | Turso¹ |
|---|---:|---:|---:|---:|
| single-query check | 1.01× | 1.07× | 1.13× | 1.48× |
| 8-way union | 0.84× | 2.51× | 0.96× | 1.30× |
| BatchCheck of 25 | 0.38× | 3.31× | 0.44× | 0.65× |
| 10-level recursion | 1.28× | 1.84× | 1.03× | 1.69× |
| ListObjects (1000) | 1.16× | 1.36× | 1.39× | 2.80× |
| tuple write + application row, committed | 0.89× | 0.91× | 0.82× | 2.28× |

(Time relative to upstream; lower is better. The last row includes the
transaction; upstream needs two non-atomic transactions for it. ¹Upstream has
no Turso datastore: Turso is compared with upstream on SQLite, so its column
includes the engine's own cost. A raw indexed lookup takes 82 µs on Turso
against 48 µs on SQLite.)

- **Pitfall: one connection.** Upstream fans a check out over pooled
  connections; inside a transaction those reads are serialized. With pgx,
  reads that queue during a round trip are **pipelined** into the next one,
  which more than recovers the loss. `database/sql` cannot pipeline, so
  fan-out-heavy checks are 2–3.5× slower there. Use pgx on PostgreSQL.
- Queued reads of branches OpenFGA has already abandoned are dropped, so
  exclusions and intersections often issue fewer queries than upstream.
- Model resolution adds no queries once a model is known to be committed. A
  request without a model ID costs one indexed `LIMIT 1` query; upstream's
  PostgreSQL datastore reads every model row of the store for it.
- Results are buffered (required to share one connection), so ListObjects
  and ListUsers allocate per row and run 1.2–1.4× slower.
- A transaction opened *only* for a check pays `BEGIN`/`ROLLBACK` round trips
  (~2.5× for a single-query check on PostgreSQL).
- **No snapshot cost.** Under the sibling wasmfga project's native benchmark,
  a check costs the same with 1 or 100,000 tuples in the store; wasmfga's
  snapshot port takes 436 ms at 10,000. See [wasmfga comparison](benchmarks/WASMFGA.md).
- **Pitfall: Turso's planner.** Without statistics it scanned the whole store
  for every point lookup (4.5× slower on the benchmark dataset, and growing
  with the store). `Migrate` seeds statistics; see Turso specifics above.

## Tests

```sh
make databases     # a PostgreSQL 17 container (docker or podman)
make test-unit     # transaction semantics, upstream datastore suite, driver
make test-upstream # OpenFGA's own Check/ListObjects/ListUsers suites
make test-hegel    # Hegel model-based tests against the original OpenFGA
make bench
```

Every suite runs against SQLite, Turso and PostgreSQL (`database/sql` and
native pgx). Set `ATOMICFGA_DIALECTS` (`sqlite,turso,postgres`) to restrict
them. Upstream has no Turso datastore, so for Turso the original OpenFGA
reference runs upstream's SQLite datastore.

- **Upstream datastore conformance**: OpenFGA's `pkg/storage/test` suite, run
  with each call in its own application transaction.
- **Upstream assertion suites and matrices**: OpenFGA's
  `consolidated_1_1_tests.yaml`, `abac_tests.yaml` and the generated Check and
  ListObjects matrices, with each resolution strategy upstream tests (44,330
  passing subtests across the four variants).
- **Hegel model-based tests** (`tests/hegel_test.go`, [hegel.dev](https://hegel.dev)):
  a state machine drives tuple writes and deletes, newly generated model
  versions, assertions, application rows, commits, rollbacks and
  application-savepoint rollbacks inside a real transaction. A reference model
  tracks committed and working state. After every step, Check, BatchCheck,
  ListObjects, ListUsers, Expand and Read through atomicfga are compared with
  the original OpenFGA serving a mirror of the working state. An isolation
  invariant checks that a concurrent transaction sees exactly the committed
  state: no uncommitted tuple, application row or model, even through the
  shared model caches. A second run lets read rules run concurrently on one
  session. Hegel shrinks failures to a minimal history.
- **Check-then-act** (`tests/guarded_test.go`): concurrent serializable
  transactions grant, revoke and edit. The invariant replays the committed
  history in serialization order and requires every edit to have been
  authorized when it committed.
  `ATOMICFGA_DEMONSTRATE_RACE=1` runs the same workload with the check made
  before the transaction, as with a separately deployed OpenFGA. Hegel finds
  a violation within seconds, for example: Bob leaves the team (clock 6101),
  his direct grant is revoked (6102), and his edit, authorized by a check
  made earlier, still commits (6103).
- **Driver unit tests** (`internal/bind`): savepoint exclusivity, call
  barriers, cancellation, abort detection and pipelining, against a fake
  backend.

## Rust port of Check (prototype)

`rust/` ports OpenFGA's Check resolution to Rust as a sans-IO evaluator
(the host answers its reads, here from inside the caller's transaction)
and compiles to a small WebAssembly module. It is fail-closed: when it cannot
be certain of OpenFGA's answer it refuses, and `Session.RustCheck` falls back
to OpenFGA's Check. Build with `make rust` and `-tags rustcheck`. Its gates
(`make test-rust`) run every check through both evaluators in the same
transaction; see [docs/rustcheck.md](docs/rustcheck.md) for the design,
the results, and the upstream nondeterminism they surfaced. The evaluator
also has a canonical (least-fixed-point) mode, checked against a Quint
specification (`spec/check.qnt`, `make test-spec`). Storage sits behind a
small interface: a redb implementation serves models, writes and checks in
the caller's redb transactions, and in browsers a separate redb module
(persisted to OPFS) or SQLite-WASM plugs into the storage-free evaluator
module (`web/`, `make test-web`).

## Layout

```text
track/github.com/openfga/openfga.git/lts-1/       upstream (built via replace)
track/github.com/memoremix-ing/atomicfga.git/sts-0/
  atomicfga.go session.go migrate.go              public API
  internal/bind/                                  loopback driver, sessions
  internal/pgstore/                               PostgreSQL datastore port
  internal/testdb/                                test databases
  tests/                                          upstream suites, Hegel
  bench/                                          benchmarks vs. original
  rust/ rustcheck.go rustredb.go                  Rust port of Check (prototype)
  web/                                            browser glue, store adapters, tests
  spec/                                           Quint spec of canonical Check
```
