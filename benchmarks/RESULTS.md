# Benchmark results

atomicfga against the original OpenFGA, on SQLite, Turso and PostgreSQL 17.
See [METHOD.md](METHOD.md) for the setup. Both implementations returned
identical results for every benchmarked operation before it was timed.
Sequential tables are medians of three independent runs, parallel tables of
two. Raw `go test -bench` output is in [results/](results/).

Upstream OpenFGA has no Turso datastore, so the Turso tables compare with
the original on SQLite: they combine atomicfga's cost with the Turso
engine's.

## Findings

**The cost of one connection.** Upstream fans a check out into concurrent
reads on pooled connections. Inside a transaction those reads share one
connection and are serialized. This is the main performance pitfall of
inverting transaction ownership, and it scales with fan-out, not data size:

| PostgreSQL, in tx | queries | `database/sql` | pgx (pipelined) |
|---|---:|---:|---:|
| direct check | 1 | 1.07× | 1.01× |
| 8-way union | 8 | 2.51× | **0.84×** (2 round trips) |
| intersection | 10 | 2.58× | **0.83×** (2 round trips) |
| BatchCheck of 25 | 175 | 3.31× | **0.38×** (4 round trips) |
| 10-level tuple-to-userset recursion | 37 | 1.84× | 1.28× (22 round trips) |

**Mitigations that are in place:**

1. *Pipelining* (pgx). Queries that queue up while a round trip is in flight
   are sent together in the next one. Wide unions and BatchCheck then take
   2–4 round trips, and become faster than upstream's parallel queries,
   which each pay a round trip and contend for the pool. Recursion stays
   ~1.3× because each level depends on the previous one. Yielding to let more
   siblings enqueue before the first batch was measured and made no
   difference, so it was removed.
2. *Dropping abandoned branches.* When a branch decides a union, intersection
   or exclusion, queued queries of the cancelled branches never reach the
   database. Exclusion issues 2 queries instead of upstream's 7–8 (0.27× on
   SQLite, 0.35× on Turso, 0.90× on PostgreSQL `database/sql`). Pipelined pgx
   batches are already sent, so they do not benefit.
3. *Model resolution in one indexed query.* A request is pinned to a model
   visible in its transaction with one indexed `LIMIT 1` query; upstream's
   PostgreSQL datastore reads every model row of the store to find the
   latest.

**The cost of model visibility (2026-10-25).** The tables here were measured
when sessions skipped the visibility query for a model ID known to be
committed. That shortcut was unsound (a transaction's snapshot may predate
the commit, and a savepoint rollback can remove a model the transaction
used), so every request with a model ID now runs the query. Every benchmark
here passes one, so each pays one more statement and, on PostgreSQL, one
more round trip that cannot overlap the check. Medians of three runs
([raw](results/visibility-query.txt)), relative to upstream:

| in tx | before | after |
|---|---:|---:|
| PostgreSQL pgx, direct check | 1.01× | 1.88× |
| PostgreSQL pgx, 8-way union | 0.86× | 1.16× |
| PostgreSQL pgx, 10-level recursion | 1.21× | 1.23× |
| PostgreSQL `database/sql`, direct check | 1.06× | 1.90× |
| SQLite, direct check | 1.10× | 1.24× |
| Turso, direct check | 1.46× | 1.84× |

The other tables have not been rerun since.

**What remains slower, and when it matters:**

- `database/sql` on PostgreSQL cannot pipeline. Fan-out checks run 2.3–3.3×
  slower than upstream there. Use pgx transactions.
- A transaction opened *only* to check costs the database two extra round
  trips (`BEGIN`, `ROLLBACK`): 2.5× for a single-query check on PostgreSQL,
  and under load the request holds its connection for those round trips
  (~3× throughput cost for the cheapest checks). The design assumes the
  request already has a transaction for its own work, which is the "in tx"
  column.
- ListObjects and ListUsers are 1.2–1.4× slower on PostgreSQL and SQLite.
  Results are buffered so the connection is free for the next statement, and
  upstream's reverse expansion reads in parallel.

**Writes.** A tuple write inside a transaction is a savepoint around
upstream's statements: `SAVEPOINT`, `SELECT … FOR UPDATE`, the `INSERT`s or
`DELETE`s, `RELEASE`. Committing *only* a tuple is 1.26–1.29× upstream on
PostgreSQL (two more round trips), and at parity on SQLite. Writing a tuple
**together with an application row** is what atomicfga is for. Upstream needs
two transactions and two commits for that, and the pair is not atomic.
atomicfga does it in one: **0.82–0.91×** upstream's time on SQLite and
PostgreSQL, and atomic.

**SQLite** is in-process, so serialization costs no round trips. Checks that
issue several queries are within 5% of upstream or faster (0.27–1.05×); single-query checks are 13–15% slower, the loopback layer's
overhead (routing, buffering, conversion through `database/sql` twice) where
the database is fastest. Under 16 concurrent callers atomicfga sustains
1.1–7× the throughput of the pooled original, whose connections contend.

**Turso** runs the same SQLite datastore through `turso.tech/database/tursogo`.

- The engine itself is slower than modernc SQLite for OpenFGA's statements: a
  raw indexed point lookup takes 82 µs against SQLite's 48 µs, so
  single-query checks are 1.4–1.6× upstream-on-SQLite. Row-heavy reads pay
  per row (ListObjects 2.8×, ListUsers 2.5×), and tuple writes take 3×.
- Checks that issue many queries still gain from dropped branches (exclusion
  0.35×, BatchCheck 0.65×). Under 16 concurrent callers, Turso checks sustain
  1.7–9× the throughput of the original on SQLite.
- **Pitfall found and fixed:** without planner statistics, Turso answered
  every point lookup by scanning the store's tuples through the `(store,
  ulid)` index: 370 µs per lookup instead of 82 µs on this dataset, growing
  with the store. `Migrate` now seeds `sqlite_stat1`; see the
  [design notes](../docs/design.md#turso).

## Sequential (one caller)

### Read — sqlite

| operation | OpenFGA | atomicfga, in tx | atomicfga, tx per op |
|---|---:|---:|---:|
| check/direct | 84 µs<br>1.0 q | 95 µs (1.13×)<br>1.0 q | 97 µs (1.15×)<br>1.0 q |
| check/direct-denied | 81 µs<br>1.0 q | 92 µs (1.14×)<br>1.0 q | 93 µs (1.15×)<br>1.0 q |
| check/computed | 479 µs<br>7.0 q | 391 µs (0.82×)<br>6.2 q | 387 µs (0.81×)<br>6.1 q |
| check/nested-groups | 1.14 ms<br>16.0 q | 1.14 ms (1.00×)<br>16.0 q | 1.16 ms (1.02×)<br>16.0 q |
| check/folder-depth-10 | 1.91 ms<br>36.9 q | 1.97 ms (1.03×)<br>35.8 q | 2.00 ms (1.05×)<br>35.8 q |
| check/wide-union-last | 560 µs<br>8.0 q | 537 µs (0.96×)<br>8.0 q | 545 µs (0.97×)<br>8.0 q |
| check/wide-union-denied | 548 µs<br>8.0 q | 534 µs (0.97×)<br>8.0 q | 544 µs (0.99×)<br>8.0 q |
| check/intersection | 898 µs<br>10.0 q | 583 µs (0.65×)<br>9.1 q | 594 µs (0.66×)<br>9.1 q |
| check/exclusion | 659 µs<br>7.1 q | 179 µs (0.27×)<br>2.0 q | 184 µs (0.28×)<br>2.0 q |
| check/condition | 88 µs<br>1.0 q | 99 µs (1.13×)<br>1.0 q | 105 µs (1.19×)<br>1.0 q |
| batch-check/25 | 23.43 ms<br>175.0 q | 10.30 ms (0.44×)<br>174.3 q | 10.33 ms (0.44×)<br>174.4 q |
| list-objects/1000 | 4.45 ms<br>9.0 q | 6.21 ms (1.39×)<br>9.0 q | 6.20 ms (1.39×)<br>9.0 q |
| list-users/200 | 889 µs<br>4.0 q | 800 µs (0.90×)<br>4.0 q | 814 µs (0.91×)<br>4.0 q |

### Read — turso

| operation | OpenFGA (on SQLite) | atomicfga, in tx | atomicfga, tx per op |
|---|---:|---:|---:|
| check/direct | 85 µs<br>1.0 q | 126 µs (1.48×)<br>1.0 q | 135 µs (1.60×)<br>1.0 q |
| check/direct-denied | 82 µs<br>1.0 q | 113 µs (1.37×)<br>1.0 q | 125 µs (1.52×)<br>1.0 q |
| check/computed | 481 µs<br>7.0 q | 577 µs (1.20×)<br>6.2 q | 591 µs (1.23×)<br>6.2 q |
| check/nested-groups | 1.14 ms<br>16.0 q | 1.74 ms (1.52×)<br>16.0 q | 1.76 ms (1.54×)<br>16.0 q |
| check/folder-depth-10 | 1.94 ms<br>36.9 q | 3.29 ms (1.69×)<br>35.0 q | 3.30 ms (1.70×)<br>35.0 q |
| check/wide-union-last | 558 µs<br>8.0 q | 725 µs (1.30×)<br>8.0 q | 737 µs (1.32×)<br>8.0 q |
| check/wide-union-denied | 558 µs<br>8.0 q | 710 µs (1.27×)<br>8.0 q | 722 µs (1.29×)<br>8.0 q |
| check/intersection | 953 µs<br>10.0 q | 856 µs (0.90×)<br>9.2 q | 871 µs (0.91×)<br>9.2 q |
| check/exclusion | 682 µs<br>7.3 q | 235 µs (0.35×)<br>2.0 q | 249 µs (0.37×)<br>2.0 q |
| check/condition | 88 µs<br>1.0 q | 129 µs (1.47×)<br>1.0 q | 139 µs (1.58×)<br>1.0 q |
| batch-check/25 | 23.69 ms<br>175.0 q | 15.42 ms (0.65×)<br>174.2 q | 15.49 ms (0.65×)<br>174.4 q |
| list-objects/1000 | 4.44 ms<br>9.0 q | 12.43 ms (2.80×)<br>9.0 q | 12.57 ms (2.83×)<br>9.0 q |
| list-users/200 | 890 µs<br>4.0 q | 2.19 ms (2.46×)<br>4.0 q | 2.19 ms (2.47×)<br>4.0 q |

### Read — postgres

| operation | OpenFGA | atomicfga, in tx | atomicfga, tx per op | pgx, in tx | pgx, tx per op |
|---|---:|---:|---:|---:|---:|
| check/direct | 161 µs<br>1.0 q | 173 µs (1.07×)<br>1.0 q | 403 µs (2.50×)<br>1.0 q | 163 µs (1.01×)<br>1.0 q | 395 µs (2.45×)<br>1.0 q |
| check/direct-denied | 156 µs<br>1.0 q | 169 µs (1.08×)<br>1.0 q | 399 µs (2.56×)<br>1.0 q | 159 µs (1.02×)<br>1.0 q | 390 µs (2.50×)<br>1.0 q |
| check/computed | 409 µs<br>7.0 q | 939 µs (2.30×)<br>6.4 q | 1.18 ms (2.90×)<br>6.4 q | 379 µs (0.93×)<br>7.0 q / 2.0 rt | 618 µs (1.51×)<br>7.0 q / 2.0 rt |
| check/nested-groups | 1.75 ms<br>16.0 q | 2.47 ms (1.41×)<br>16.0 q | 2.71 ms (1.55×)<br>16.0 q | 1.79 ms (1.02×)<br>16.0 q / 11.0 rt | 2.03 ms (1.16×)<br>16.0 q / 11.0 rt |
| check/folder-depth-10 | 2.77 ms<br>37.0 q | 5.11 ms (1.84×)<br>35.6 q | 5.36 ms (1.93×)<br>35.6 q | 3.55 ms (1.28×)<br>37.0 q / 22.0 rt | 3.74 ms (1.35×)<br>37.0 q / 22.0 rt |
| check/wide-union-last | 468 µs<br>8.0 q | 1.17 ms (2.51×)<br>8.0 q | 1.41 ms (3.02×)<br>8.0 q | 394 µs (0.84×)<br>8.0 q / 2.1 rt | 619 µs (1.32×)<br>8.0 q / 2.1 rt |
| check/wide-union-denied | 499 µs<br>8.0 q | 1.17 ms (2.34×)<br>8.0 q | 1.41 ms (2.83×)<br>8.0 q | 387 µs (0.78×)<br>8.0 q / 2.1 rt | 619 µs (1.24×)<br>8.0 q / 2.1 rt |
| check/intersection | 545 µs<br>10.0 q | 1.41 ms (2.58×)<br>9.5 q | 1.62 ms (2.98×)<br>9.5 q | 454 µs (0.83×)<br>10.0 q / 2.0 rt | 687 µs (1.26×)<br>10.0 q / 2.0 rt |
| check/exclusion | 403 µs<br>8.0 q | 362 µs (0.90×)<br>2.0 q | 596 µs (1.48×)<br>2.0 q | 393 µs (0.98×)<br>8.0 q / 2.0 rt | 626 µs (1.55×)<br>8.0 q / 2.0 rt |
| check/condition | 161 µs<br>1.0 q | 177 µs (1.10×)<br>1.0 q | 408 µs (2.53×)<br>1.0 q | 164 µs (1.02×)<br>1.0 q | 397 µs (2.47×)<br>1.0 q |
| batch-check/25 | 7.53 ms<br>175.0 q | 24.89 ms (3.31×)<br>174.4 q | 25.05 ms (3.33×)<br>174.3 q | 2.83 ms (0.38×)<br>175.0 q / 4.0 rt | 3.11 ms (0.41×)<br>175.0 q / 3.7 rt |
| list-objects/1000 | 3.45 ms<br>9.0 q | 4.69 ms (1.36×)<br>9.0 q | 5.01 ms (1.45×)<br>9.0 q | 4.00 ms (1.16×)<br>9.0 q / 4.0 rt | 4.43 ms (1.28×)<br>9.0 q / 4.0 rt |
| list-users/200 | 728 µs<br>4.0 q | 945 µs (1.30×)<br>4.0 q | 1.19 ms (1.63×)<br>4.0 q | 945 µs (1.30×)<br>4.0 q / 2.0 rt | 1.18 ms (1.63×)<br>4.0 q / 2.0 rt |

### Write — sqlite

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| write/tuple | 221 µs | 227 µs (1.03×)<br>5.0 q |
| write/tuple-with-app-row | 296 µs | 242 µs (0.82×)<br>5.0 q |

### Write — turso

| operation | OpenFGA (on SQLite) | atomicfga, tx per op |
|---|---:|---:|
| write/tuple | 221 µs | 660 µs (2.99×)<br>5.0 q |
| write/tuple-with-app-row | 296 µs | 676 µs (2.28×)<br>5.0 q |

### Write — postgres

| operation | OpenFGA | atomicfga, tx per op | pgx, tx per op |
|---|---:|---:|---:|
| write/tuple | 648 µs | 834 µs (1.29×)<br>5.0 q | 819 µs (1.26×)<br>5.0 q |
| write/tuple-with-app-row | 1.04 ms | 952 µs (0.91×)<br>5.0 q | 931 µs (0.89×)<br>5.0 q |

## Parallel (16 callers, one transaction per operation)

Lower is better: time per operation across all callers.

### ReadParallel — sqlite

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| check/direct | 47 µs | 32 µs (0.69×) |
| check/direct-denied | 47 µs | 33 µs (0.70×) |
| check/computed | 655 µs | 212 µs (0.32×) |
| check/nested-groups | 1.23 ms | 521 µs (0.42×) |
| check/folder-depth-10 | 2.80 ms | 1.02 ms (0.36×) |
| check/wide-union-last | 712 µs | 276 µs (0.39×) |
| check/wide-union-denied | 807 µs | 270 µs (0.33×) |
| check/intersection | 1.02 ms | 325 µs (0.32×) |
| check/exclusion | 762 µs | 103 µs (0.13×) |
| check/condition | 51 µs | 36 µs (0.70×) |
| batch-check/25 | 31.77 ms | 6.35 ms (0.20×) |
| list-objects/1000 | 3.33 ms | 3.09 ms (0.93×) |
| list-users/200 | 436 µs | 267 µs (0.61×) |

### ReadParallel — turso

| operation | OpenFGA (on SQLite) | atomicfga, tx per op |
|---|---:|---:|
| check/direct | 53 µs | 31 µs (0.59×) |
| check/direct-denied | 51 µs | 30 µs (0.57×) |
| check/computed | 724 µs | 158 µs (0.22×) |
| check/nested-groups | 1.47 ms | 465 µs (0.32×) |
| check/folder-depth-10 | 2.88 ms | 896 µs (0.31×) |
| check/wide-union-last | 752 µs | 195 µs (0.26×) |
| check/wide-union-denied | 836 µs | 198 µs (0.24×) |
| check/intersection | 1.09 ms | 228 µs (0.21×) |
| check/exclusion | 806 µs | 90 µs (0.11×) |
| check/condition | 62 µs | 31 µs (0.50×) |
| batch-check/25 | 34.48 ms | 4.96 ms (0.14×) |
| list-objects/1000 | 3.31 ms | 4.76 ms (1.44×) |
| list-users/200 | 437 µs | 679 µs (1.55×) |

### ReadParallel — postgres

| operation | OpenFGA | atomicfga, tx per op | pgx, tx per op |
|---|---:|---:|---:|
| check/direct | 42 µs | 114 µs (2.75×) | 146 µs (3.52×) |
| check/direct-denied | 41 µs | 114 µs (2.76×) | 153 µs (3.70×) |
| check/computed | 295 µs | 353 µs (1.20×) | 241 µs (0.82×) |
| check/nested-groups | 680 µs | 764 µs (1.12×) | 688 µs (1.01×) |
| check/folder-depth-10 | 1.82 ms | 2.11 ms (1.16×) | 1.48 ms (0.81×) |
| check/wide-union-last | 341 µs | 421 µs (1.23×) | 237 µs (0.69×) |
| check/wide-union-denied | 338 µs | 456 µs (1.35×) | 265 µs (0.78×) |
| check/intersection | 436 µs | 513 µs (1.18×) | 289 µs (0.66×) |
| check/exclusion | 331 µs | 196 µs (0.59×) | 236 µs (0.71×) |
| check/condition | 43 µs | 118 µs (2.74×) | 131 µs (3.03×) |
| batch-check/25 | 8.04 ms | 8.60 ms (1.07×) | 2.56 ms (0.32×) |
| list-objects/1000 | 2.41 ms | 2.68 ms (1.11×) | 2.76 ms (1.14×) |
| list-users/200 | 309 µs | 418 µs (1.35×) | 383 µs (1.24×) |

## Engine reference

`BenchmarkRawQuery`: the indexed lookup of a direct check sent straight to each SQLite engine's driver, in a read transaction (median of three runs).

| engine | unprepared | prepared |
|---|---:|---:|
| SQLite (modernc) | 48 µs | 31 µs |
| Turso (tursogo, seeded statistics) | 82 µs | 82 µs |
| Turso without statistics (before the fix) | 368 µs | 370 µs |
