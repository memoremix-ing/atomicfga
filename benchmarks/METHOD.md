# Benchmark method

`bench/` runs the same operations against two implementations, per dialect:

- **OpenFGA**: upstream's server with upstream's pooled SQL datastore
  (`postgres.New`, `mysql.New`, `sqlite.New`), on its own database, opening
  its own connections and committing each write in its own transaction.
- **atomicfga**: the same server code inside application transactions, on a
  second database of the same dialect.

Both databases hold the same dataset and authorization model
(`bench/workloads_test.go`). Before any timing, each benchmark runs its
operation on both and fails unless the results are identical. Both
implementations have the same default server configuration (tuple caches
off, model and type system caches on) and pools of at most 16 connections.

Modes:

| mode | meaning |
|---|---|
| OpenFGA | `server.Check(…)` etc. on the original |
| atomicfga, in tx | every iteration in one open transaction: the marginal cost of an operation inside a request's existing transaction |
| atomicfga, tx per op | `BEGIN`; operation; `COMMIT`/`ROLLBACK` per iteration: a request that opens a transaction only for authorization |
| pgx, … | as above with a native `pgx.Tx` (pipelining on); otherwise `database/sql` (`pgx/stdlib` for PostgreSQL) |

Metrics: `ns/op` (median of runs), `q` = SQL statements per operation
(counted at the datastore for OpenFGA, at the transaction for atomicfga),
`rt` = round trips when pgx pipelining merged statements.

Environment: Apple M2 (8 cores), Go 1.27.1, PostgreSQL 17 and MySQL 8.4 in a
podman VM (libkrun) on the same machine, with default (durable) settings, and
SQLite through modernc.org/sqlite in WAL mode. Round trips through the VM's
port forwarding cost roughly 50–100 µs, which is comparable to a same-zone
network. These are microbenchmarks of single operations on one machine; they
compare the two implementations, not the databases.

Reproduce: `make databases && make bench`, then
`scripts/report.py dist/bench.txt`.
