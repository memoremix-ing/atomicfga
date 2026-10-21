# Benchmark results

atomicfga against the original OpenFGA, on SQLite, PostgreSQL 17 and MySQL
8.4. See [METHOD.md](METHOD.md) for the setup. Both implementations returned
identical results for every benchmarked operation before it was timed.
Sequential tables are medians of three independent runs, parallel tables of
two. Raw `go test -bench` output is in [results/](results/).

## Findings

**The cost of one connection.** Upstream fans a check out into concurrent
reads on pooled connections. Inside a transaction those reads share one
connection and are serialized. This is the main performance pitfall of
inverting transaction ownership, and it scales with fan-out, not data size:

| PostgreSQL, in tx | queries | `database/sql` | pgx (pipelined) |
|---|---:|---:|---:|
| direct check | 1 | 1.08× | 1.01× |
| 8-way union | 8 | 2.49× | **0.82×** (2 round trips) |
| intersection | 10 | 2.57× | **0.84×** (2 round trips) |
| BatchCheck of 25 | 175 | 3.40× | **0.39×** (4 round trips) |
| 10-level tuple-to-userset recursion | 37 | 1.86× | 1.28× (22 round trips) |

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
   database. Exclusion issues 2 queries instead of upstream's 7–8 (0.26× on
   SQLite, 0.77× on MySQL, 0.91× on PostgreSQL `database/sql`). Pipelined
   pgx batches are already sent, so they do not benefit.
3. *Model resolution without queries.* After the first sighting of a
   committed model, pinning a request to a visible model costs nothing.
   Single-query checks inside a transaction are within 1–9% of upstream on
   PostgreSQL and MySQL (12–15% on SQLite, see below).

**What remains slower, and when it matters:**

- `database/sql` on PostgreSQL and MySQL cannot pipeline. Fan-out checks run
  2.3–3.4× slower than upstream. On PostgreSQL use pgx transactions. MySQL
  has no equivalent without multi-statement DSN flags, which atomicfga does
  not require.
- A transaction opened *only* to check costs the database two extra round
  trips (`BEGIN`, `ROLLBACK`): 2.5× for a single-query check on PostgreSQL,
  and under load the request holds its connection for those round trips
  (~3× throughput cost for the cheapest checks). The design assumes the
  request already has a transaction for its own work, which is the "in tx"
  column.
- ListObjects is 1.2–1.4× slower, and ListUsers 1.3–1.4× on PostgreSQL and
  MySQL. Results are buffered so the connection is free for the next
  statement, and upstream's reverse expansion reads in parallel.

**Writes.** A tuple write inside a transaction is a savepoint around
upstream's statements: `SAVEPOINT`, `SELECT … FOR UPDATE`, the `INSERT`s or
`DELETE`s, `RELEASE`. Committing *only* a tuple is 1.2–1.3× upstream on
PostgreSQL and MySQL (two more round trips), and at parity on SQLite. Writing a tuple **together with an application row** is
what atomicfga is for. Upstream needs two transactions and two commits for
that, and the pair is not atomic. atomicfga does it in one: **0.80–0.95×**
upstream's time, and atomic.

**SQLite** is in-process, so serialization costs no round trips. Every
check that issues several queries is as fast or faster than upstream
(0.26–1.01×); single-query checks are 12–15% slower. The pooled original
contends between its connections, so under 16 concurrent callers atomicfga
sustains 1.2–7× the throughput. The exception is ListObjects (1.4×
sequentially), whose parallel reads upstream spreads over several
connections.

**Overhead of the loopback layer** itself (routing, buffering, conversion
through `database/sql` twice) is visible only where the database is fastest:
+10 µs (12%) for a single-query check on SQLite.

## Sequential (one caller)

### Read — sqlite

| operation | OpenFGA | atomicfga, in tx | atomicfga, tx per op |
|---|---:|---:|---:|
| check/direct | 85 µs<br>1.0 q | 95 µs (1.12×)<br>1.0 q | 98 µs (1.15×)<br>1.0 q |
| check/direct-denied | 82 µs<br>1.0 q | 93 µs (1.13×)<br>1.0 q | 94 µs (1.14×)<br>1.0 q |
| check/computed | 487 µs<br>7.0 q | 392 µs (0.81×)<br>6.2 q | 397 µs (0.82×)<br>6.2 q |
| check/nested-groups | 1.15 ms<br>16.0 q | 1.15 ms (1.00×)<br>16.0 q | 1.16 ms (1.01×)<br>16.0 q |
| check/folder-depth-10 | 1.97 ms<br>36.9 q | 1.97 ms (1.00×)<br>35.8 q | 1.99 ms (1.01×)<br>35.8 q |
| check/wide-union-last | 590 µs<br>8.0 q | 544 µs (0.92×)<br>8.0 q | 550 µs (0.93×)<br>8.0 q |
| check/wide-union-denied | 572 µs<br>8.0 q | 539 µs (0.94×)<br>8.0 q | 549 µs (0.96×)<br>8.0 q |
| check/intersection | 971 µs<br>10.0 q | 606 µs (0.62×)<br>9.1 q | 607 µs (0.63×)<br>9.1 q |
| check/exclusion | 685 µs<br>7.2 q | 179 µs (0.26×)<br>2.0 q | 186 µs (0.27×)<br>2.0 q |
| check/condition | 88 µs<br>1.0 q | 100 µs (1.14×)<br>1.0 q | 105 µs (1.20×)<br>1.0 q |
| batch-check/25 | 24.25 ms<br>175.0 q | 10.37 ms (0.43×)<br>174.5 q | 10.35 ms (0.43×)<br>174.3 q |
| list-objects/1000 | 4.42 ms<br>9.0 q | 6.22 ms (1.41×)<br>9.0 q | 6.18 ms (1.40×)<br>9.0 q |
| list-users/200 | 883 µs<br>4.0 q | 807 µs (0.91×)<br>4.0 q | 820 µs (0.93×)<br>4.0 q |

### Read — postgres

| operation | OpenFGA | atomicfga, in tx | atomicfga, tx per op | pgx, in tx | pgx, tx per op |
|---|---:|---:|---:|---:|---:|
| check/direct | 160 µs<br>1.0 q | 174 µs (1.08×)<br>1.0 q | 405 µs (2.53×)<br>1.0 q | 162 µs (1.01×)<br>1.0 q | 391 µs (2.44×)<br>1.0 q |
| check/direct-denied | 156 µs<br>1.0 q | 167 µs (1.07×)<br>1.0 q | 403 µs (2.58×)<br>1.0 q | 161 µs (1.03×)<br>1.0 q | 388 µs (2.48×)<br>1.0 q |
| check/computed | 409 µs<br>7.0 q | 946 µs (2.31×)<br>6.4 q | 1.18 ms (2.88×)<br>6.4 q | 384 µs (0.94×)<br>7.0 q / 2.0 rt | 607 µs (1.48×)<br>7.0 q / 2.0 rt |
| check/nested-groups | 1.75 ms<br>16.0 q | 2.46 ms (1.41×)<br>16.0 q | 2.70 ms (1.55×)<br>16.0 q | 1.84 ms (1.05×)<br>16.0 q / 11.0 rt | 2.03 ms (1.16×)<br>16.0 q / 11.0 rt |
| check/folder-depth-10 | 2.79 ms<br>37.0 q | 5.18 ms (1.86×)<br>35.5 q | 5.35 ms (1.92×)<br>35.6 q | 3.56 ms (1.28×)<br>37.0 q / 22.0 rt | 3.79 ms (1.36×)<br>37.0 q / 22.0 rt |
| check/wide-union-last | 472 µs<br>8.0 q | 1.18 ms (2.49×)<br>8.0 q | 1.42 ms (3.00×)<br>8.0 q | 385 µs (0.82×)<br>8.0 q / 2.1 rt | 615 µs (1.30×)<br>8.0 q / 2.0 rt |
| check/wide-union-denied | 499 µs<br>8.0 q | 1.18 ms (2.36×)<br>8.0 q | 1.42 ms (2.83×)<br>8.0 q | 383 µs (0.77×)<br>8.0 q / 2.0 rt | 611 µs (1.22×)<br>8.0 q / 2.0 rt |
| check/intersection | 545 µs<br>10.0 q | 1.40 ms (2.57×)<br>9.5 q | 1.64 ms (3.01×)<br>9.5 q | 456 µs (0.84×)<br>10.0 q / 2.0 rt | 689 µs (1.26×)<br>10.0 q / 2.0 rt |
| check/exclusion | 402 µs<br>8.0 q | 364 µs (0.91×)<br>2.0 q | 609 µs (1.52×)<br>2.0 q | 395 µs (0.98×)<br>8.0 q / 2.0 rt | 636 µs (1.58×)<br>8.0 q / 2.0 rt |
| check/condition | 163 µs<br>1.0 q | 178 µs (1.09×)<br>1.0 q | 411 µs (2.53×)<br>1.0 q | 167 µs (1.02×)<br>1.0 q | 398 µs (2.45×)<br>1.0 q |
| batch-check/25 | 7.31 ms<br>175.0 q | 24.86 ms (3.40×)<br>174.3 q | 25.10 ms (3.43×)<br>174.4 q | 2.84 ms (0.39×)<br>175.0 q / 4.0 rt | 3.12 ms (0.43×)<br>175.0 q / 3.9 rt |
| list-objects/1000 | 3.42 ms<br>9.0 q | 4.59 ms (1.34×)<br>9.0 q | 4.93 ms (1.44×)<br>9.0 q | 4.05 ms (1.19×)<br>9.0 q / 4.0 rt | 4.36 ms (1.28×)<br>9.0 q / 4.0 rt |
| list-users/200 | 735 µs<br>4.0 q | 952 µs (1.30×)<br>4.0 q | 1.19 ms (1.62×)<br>4.0 q | 944 µs (1.29×)<br>4.0 q / 2.0 rt | 1.19 ms (1.62×)<br>4.0 q / 2.0 rt |

### Read — mysql

| operation | OpenFGA | atomicfga, in tx | atomicfga, tx per op |
|---|---:|---:|---:|
| check/direct | 296 µs<br>1.0 q | 313 µs (1.06×)<br>1.0 q | 549 µs (1.86×)<br>1.0 q |
| check/direct-denied | 295 µs<br>1.0 q | 308 µs (1.04×)<br>1.0 q | 544 µs (1.85×)<br>1.0 q |
| check/computed | 826 µs<br>7.0 q | 1.96 ms (2.38×)<br>6.7 q | 2.21 ms (2.68×)<br>6.6 q |
| check/nested-groups | 3.59 ms<br>16.0 q | 4.94 ms (1.37×)<br>16.0 q | 5.18 ms (1.44×)<br>16.0 q |
| check/folder-depth-10 | 5.74 ms<br>37.0 q | 10.70 ms (1.86×)<br>35.6 q | 10.91 ms (1.90×)<br>35.7 q |
| check/wide-union-last | 912 µs<br>8.0 q | 2.32 ms (2.55×)<br>8.0 q | 2.56 ms (2.81×)<br>8.0 q |
| check/wide-union-denied | 985 µs<br>8.0 q | 2.32 ms (2.36×)<br>8.0 q | 2.60 ms (2.64×)<br>8.0 q |
| check/intersection | 1.10 ms<br>10.0 q | 2.81 ms (2.56×)<br>9.5 q | 3.08 ms (2.81×)<br>9.5 q |
| check/exclusion | 869 µs<br>8.0 q | 667 µs (0.77×)<br>2.0 q | 894 µs (1.03×)<br>2.0 q |
| check/condition | 302 µs<br>1.0 q | 325 µs (1.07×)<br>1.0 q | 550 µs (1.82×)<br>1.0 q |
| batch-check/25 | 17.92 ms<br>175.0 q | 50.74 ms (2.83×)<br>174.4 q | 51.17 ms (2.86×)<br>174.5 q |
| list-objects/1000 | 4.96 ms<br>9.0 q | 6.70 ms (1.35×)<br>9.0 q | 7.26 ms (1.46×)<br>9.0 q |
| list-users/200 | 1.15 ms<br>4.0 q | 1.61 ms (1.40×)<br>4.0 q | 1.84 ms (1.61×)<br>4.0 q |

### Write — sqlite

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| write/tuple | 230 µs | 228 µs (0.99×)<br>5.0 q |
| write/tuple-with-app-row | 296 µs | 243 µs (0.82×)<br>5.0 q |

### Write — postgres

| operation | OpenFGA | atomicfga, tx per op | pgx, tx per op |
|---|---:|---:|---:|
| write/tuple | 656 µs | 852 µs (1.30×)<br>5.0 q | 831 µs (1.27×)<br>5.0 q |
| write/tuple-with-app-row | 1.03 ms | 974 µs (0.95×)<br>5.0 q | 940 µs (0.92×)<br>5.0 q |

### Write — mysql

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| write/tuple | 1.57 ms | 1.89 ms (1.20×)<br>5.0 q |
| write/tuple-with-app-row | 2.59 ms | 2.07 ms (0.80×)<br>5.0 q |

## Parallel (16 callers, one transaction per operation)

Lower is better: time per operation across all callers.

### ReadParallel — sqlite

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| check/direct | 47 µs | 34 µs (0.71×) |
| check/direct-denied | 47 µs | 33 µs (0.70×) |
| check/computed | 693 µs | 210 µs (0.30×) |
| check/nested-groups | 1.25 ms | 520 µs (0.42×) |
| check/folder-depth-10 | 2.82 ms | 1.01 ms (0.36×) |
| check/wide-union-last | 724 µs | 256 µs (0.35×) |
| check/wide-union-denied | 850 µs | 253 µs (0.30×) |
| check/intersection | 1.02 ms | 342 µs (0.34×) |
| check/exclusion | 783 µs | 100 µs (0.13×) |
| check/condition | 58 µs | 37 µs (0.64×) |
| batch-check/25 | 32.86 ms | 6.25 ms (0.19×) |
| list-objects/1000 | 3.22 ms | 2.78 ms (0.86×) |
| list-users/200 | 450 µs | 263 µs (0.58×) |

### ReadParallel — postgres

| operation | OpenFGA | atomicfga, tx per op | pgx, tx per op |
|---|---:|---:|---:|
| check/direct | 42 µs | 117 µs (2.81×) | 138 µs (3.31×) |
| check/direct-denied | 42 µs | 123 µs (2.95×) | 135 µs (3.25×) |
| check/computed | 294 µs | 366 µs (1.24×) | 229 µs (0.78×) |
| check/nested-groups | 671 µs | 745 µs (1.11×) | 669 µs (1.00×) |
| check/folder-depth-10 | 1.57 ms | 1.56 ms (1.00×) | 1.35 ms (0.86×) |
| check/wide-union-last | 330 µs | 409 µs (1.24×) | 233 µs (0.71×) |
| check/wide-union-denied | 334 µs | 436 µs (1.31×) | 238 µs (0.71×) |
| check/intersection | 425 µs | 471 µs (1.11×) | 269 µs (0.63×) |
| check/exclusion | 323 µs | 196 µs (0.61×) | 234 µs (0.73×) |
| check/condition | 43 µs | 114 µs (2.64×) | 131 µs (3.05×) |
| batch-check/25 | 8.15 ms | 8.24 ms (1.01×) | 2.63 ms (0.32×) |
| list-objects/1000 | 2.52 ms | 2.77 ms (1.10×) | 2.51 ms (1.00×) |
| list-users/200 | 317 µs | 412 µs (1.30×) | 385 µs (1.21×) |

### ReadParallel — mysql

| operation | OpenFGA | atomicfga, tx per op |
|---|---:|---:|
| check/direct | 95 µs | 161 µs (1.69×) |
| check/direct-denied | 92 µs | 162 µs (1.76×) |
| check/computed | 502 µs | 698 µs (1.39×) |
| check/nested-groups | 1.51 ms | 1.60 ms (1.06×) |
| check/folder-depth-10 | 3.48 ms | 3.55 ms (1.02×) |
| check/wide-union-last | 741 µs | 805 µs (1.09×) |
| check/wide-union-denied | 709 µs | 794 µs (1.12×) |
| check/intersection | 738 µs | 940 µs (1.27×) |
| check/exclusion | 540 µs | 281 µs (0.52×) |
| check/condition | 95 µs | 166 µs (1.76×) |
| batch-check/25 | 17.95 ms | 17.76 ms (0.99×) |
| list-objects/1000 | 3.68 ms | 3.12 ms (0.85×) |
| list-users/200 | 570 µs | 693 µs (1.22×) |

