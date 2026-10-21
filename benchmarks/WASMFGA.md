# atomicfga under wasmfga's native benchmark

The sibling project wasmfga (`../../../wasmfga.git/sts-0`) also runs OpenFGA
inside an application-owned transaction. Its Go port evaluates each Check over
a **snapshot**: the store's complete tuple set, loaded into upstream's
in-memory backend for every Check. Upstream's `MemoryBackend.Write` is
quadratic, so the cost grows with store size (wasmfga caps snapshots at 10,000
tuples). wasmfga also measures upstream's checker over a direct transaction
reader of its own (`engine/upstream`), which avoids hydration.

`TestWasmfgaWorkloads` (`bench/wasmfga_test.go`) runs wasmfga's ten native
workloads through atomicfga under the same conditions:

- the same inputs: generated as in wasmfga's `benchmarks/workloads.mjs` from
  a copy of its model and cases (`bench/testdata/wasmfga.json`), and checked
  against wasmfga's built fixtures, which they match exactly;
- every Check inside one open, uncommitted transaction that wrote the
  workload's store, model and tuples, with an explicit model ID; SQLite in
  memory on one connection, as in wasmfga;
- wasmfga's warmups and iterations (500/5,000, 100/500 from 1,000 tuples,
  10/50 from 10,000), median of per-check times, every decision verified;
- plus a 100,000-tuple store, beyond wasmfga's limit.

wasmfga's three Go engines were rerun from its built binary
(`dist/native/go-bench`, change 03225a7b) on the same machine, immediately
before. The "OpenFGA server" columns run upstream's server on its pooled
datastore with committed data.

## Results

Median per Check (Apple M2; PostgreSQL 17 in a local podman VM).

| workload | wasmfga Go snapshot port | wasmfga upstream reader, model per check | wasmfga upstream reader, model reused | OpenFGA server, SQLite | atomicfga, SQLite | atomicfga, Turso | OpenFGA server, PostgreSQL | atomicfga, PostgreSQL database/sql | atomicfga, PostgreSQL pgx |
|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| direct-grant | 268 µs | 393 µs | 250 µs | 597 µs | 279 µs | 364 µs | 443 µs | 637 µs | 388 µs |
| empty-deny | 273 µs | 409 µs | 256 µs | 648 µs | 481 µs | 637 µs | 481 µs | 1,151 µs | 388 µs |
| userset-chain-12 | 449 µs | 789 µs | 624 µs | 1,562 µs | 1,454 µs | 2,052 µs | 2,421 µs | 3,217 µs | 2,423 µs |
| parent-chain-12 | 998 µs | 1,843 µs | 1,656 µs | 3,251 µs | 2,884 µs | 4,339 µs | 3,579 µs | 7,431 µs | 4,164 µs |
| intersection | 274 µs | 416 µs | 271 µs | 599 µs | 545 µs | 731 µs | 485 µs | 1,255 µs | 394 µs |
| exclusion | 169 µs | 195 µs | 52 µs | 627 µs | 155 µs | 202 µs | 406 µs | 343 µs | 384 µs |
| cel-int | 286 µs | 404 µs | 256 µs | 662 µs | 473 µs | 636 µs | 460 µs | 1,105 µs | 392 µs |
| snapshot-100 | 454 µs | 397 µs | 251 µs | 610 µs | 287 µs | 360 µs | 450 µs | 643 µs | 406 µs |
| snapshot-1000 | 5,807 µs | 395 µs | 253 µs | 560 µs | 288 µs | 374 µs | 475 µs | 641 µs | 404 µs |
| snapshot-10000 | 436.20 ms | 406 µs | 258 µs | 619 µs | 294 µs | 366 µs | 684 µs | 736 µs | 475 µs |
| snapshot-100000 | — | — | — | 572 µs | 293 µs | 374 µs | 450 µs | 718 µs | 477 µs |

Raw results: [results/wasmfga](results/wasmfga/). Reproduce with
`ATOMICFGA_WASMFGA=1 go test -run TestWasmfgaWorkloads -v ./bench/`.

## Findings

- **No snapshot cost.** atomicfga issues the same few indexed queries
  whatever the store's size: a direct grant takes 279 µs with one tuple and
  293 µs with 100,000 on SQLite (4 statements either way). The snapshot port
  takes 5.8 ms at 1,000 tuples and 436 ms at 10,000: 1,500× slower than
  atomicfga there.
- **Faster than the OpenFGA server it embeds.** On SQLite, atomicfga beats
  the original server on every workload (0.25–0.93×). It issues about half the
  statements (4.2 against 8.8 for a direct grant): queued queries of
  resolution branches already decided are dropped. On PostgreSQL, pgx
  transactions are within 0.69–1.16× of the server; `database/sql` is up to
  2.6× slower (0.84× on exclusion), since it cannot pipeline.
- **Slower than wasmfga's lean upstream reader on small, deep checks.** That
  baseline (model reused) is 1.1× faster on a direct grant, 1.7–2.3× on
  chains, intersection and conditions, and 3× on exclusion. It calls upstream's `CheckCommand`
  directly with a compiled model, resolution breadth 1, and wasmfga's own
  two-table schema, bypassing the server's request handling, model
  resolution and planner. It supports Check only, on SQLite.
- Resolution breadth 1 does **not** help atomicfga: a direct grant slows to
  590 µs, because sequential resolution forgoes dropping decided branches'
  queries (7 statements instead of 4). A profile of the deep chain shows
  ~90% of CPU in goroutine wakeups around the serialized transaction; running
  queued statements on one leader goroutine (as pgx pipelining does) was
  measured and was slower for the same reason.
- Turso is ~1.3–1.5× SQLite here; its engine is slower per statement (see
  [RESULTS.md](RESULTS.md#engine-reference)).
