# A Rust port of Check (prototype)

`rust/` is a port of OpenFGA's Check resolution to Rust, built to answer one
question: can atomicfga's approach (reads at check time, inside the caller's
transaction) be ported to another language with *more* conformance
confidence than wasmfga's snapshot evaluator reached, and compiled to a
small WebAssembly module?

It is held to the same gates as atomicfga itself, with one addition: every
check runs through **both** OpenFGA's own Check and the Rust evaluator, in the
same transaction, and any disagreement fails the gate.

## Design

**Sans-IO.** Resolution is ordinary async Rust, but every datastore read is a
future that registers a request and waits for the host. A host drives a
`Task`: each step polls the whole resolution once and returns every read that
became necessary, so independent reads leave together. atomicfga answers them
through the session-bound datastore (so the caller's uncommitted writes are
visible), concurrently, which pgx pipelines into one round trip. The reads are
exactly OpenFGA's `RelationshipTupleReader` calls (`Read`, `ReadUserTuple`,
`ReadUsersetTuples`), so the host runs upstream's SQL unchanged. Contextual
tuples are merged in Rust as `CombinedTupleReader` does.

**What is ported** (each mirrors the upstream function named in its doc
comment): request validation (`validateCheckRequest`, the tuple string
predicates, `ValidateTupleForRead/Write`, `ValidateStruct`), the typesystem
queries Check uses, the authorization model graph's `PathExists`, the
`LocalChecker` (direct, public-wildcard and userset checks, computed usersets,
tuple-to-userset, union/intersection/exclusion, cycle detection, the depth
limit), condition filtering of tuple iterators, and condition evaluation.

**Conditions** are a typed scalar subset of CEL implemented here rather than
a CEL library: parameters of type bool, int, uint, double, string,
timestamp, duration and ipaddress; their operators, `timestamp()`,
`duration()`, `ipaddress()`, `size`, `startsWith`, `endsWith`, `contains`
and `in_cidr`. Each expression is type-checked with cel-go's overloads (no
implicit cross-type arithmetic), and evaluation follows cel-go: checked
integer overflow and division errors, error-absorbing `&&`/`||`, NaN
ordering errors, the runtime cost limit (as an upper bound), missing
parameters, and OpenFGA's own parameter conversions (`big.Float` saturation,
`time.ParseDuration` ported statement by statement).

**Fail-closed.** Whenever the port cannot be certain what OpenFGA answers, it
*refuses*; `Session.RustCheck` then answers with OpenFGA's Check, and
`Session.RustCheckStrict` returns `ErrRustRefused`. It refuses for features it
does not implement (list/map parameters, macros, regular expressions, inline
`$expression` conditions), for input spellings it does not parse exactly as Go
does (an RFC 3339 timestamp without an offset, a zoned IPv6 address, a numeric
string beyond what it parses exactly), and for answers that are not
determined upstream (below).

## Upstream's answer is not always determined

OpenFGA resolves operands concurrently, and intersection and exclusion answer
with whichever decisive operand arrives *first*. The response's cycle flag,
which decides an enclosing exclusion, and the error reported can therefore
depend on goroutine timing. Every resolution step in the port therefore
yields the **set of outcomes** OpenFGA could produce (allowed, denied, denied
with a cycle, error), never a guess at one interleaving. The engine answers
only when every member of the final set gives the same API result
(`src/outcome.rs`).

Building the gates surfaced a larger source of nondeterminism. OpenFGA's
planner chooses, per node and by Thompson sampling over observed latencies,
between the default resolver and its *recursive* and *weight-2* fast paths.
Those agree on reachability but not at the edges:

- **Depth.** The recursive path searches one level deeper. On a 26-folder
  parent chain the default resolver fails with
  `authorization_model_resolution_too_complex` and the recursive one answers
  `true` (`TestRustEdgeCases`, and the counterexample Hegel found).
- **Cycles.** The fast paths never set the cycle flag, so an exclusion whose
  subtrahend runs into a cycle can be `false` (default) or the base's answer
  (fast path).
- **Condition errors.** The fast paths also read tuples from the user's side
  and fail on a condition error there, which the default never reads.

The port implements the default resolver. At nodes where OpenFGA's own
predicates (`TTUUseRecursiveResolver`, `UsersetUseWeight2Resolver`, …) admit
a fast path, it widens the outcome set: it adds plain `false` beside a cycle,
refuses if the subtree came within three levels of the depth limit or
errored, and refuses if any relation beneath it admits conditional tuples.
The predicates rest on the weighted authorization model graph, which is not
ported yet. The host computes them once per model with OpenFGA's typesystem
and hands the table over with the model. A WebAssembly host would need the
table precomputed (it could be stored with the model when it is written,
since models are immutable) or the weighted graph ported.

## Gates

`make test-rust`, with `-tags rustcheck`:

| gate | what | Rust-answered | wrong | refused |
|---|---|---|---|---|
| `TestRustUpstream` | OpenFGA's Check assertion suites (consolidated and ABAC YAML) and generated Check matrices, with and without check optimizations | 6,908 | **0** | 2,156 |
| `TestHegelRustCheck` | Hegel differential: random models (all rewrite operators, wildcards, usersets, recursive parents, conditions including unsupported ones), tuples written under one model version and checked under another, contextual tuples, request contexts with wrong types and forbidden characters, seeded parent/membership cycles and chains around the depth limit | 32,021 | **0** | 2,497 |
| `TestRustEdgeCases` | cycles in subtrahends and intersections, chains at the depth limit, userset users | 120 | **0** | 0 |

Each row sums SQLite, Turso, PostgreSQL over database/sql and PostgreSQL over
pgx. Error responses are compared by gRPC code (as upstream's own suites do).
Messages differ in many error cases and are counted but not gated.

In the upstream suites, refusals are mostly the conditional fast-path rule
(495 of 539 per variant) and inline `$expression` conditions (36). In the
Hegel workload, they are the same rule, list parameters (unsupported, by
design of the workload) and deliberately odd input spellings.

**The gates were mutation-tested.** Six bugs were planted one at a time:
contextual tuples no longer shadowing a stored tuple, a resolution depth
limit of 26, a cyclic subtrahend no longer deciding an exclusion, a CEL `>=`
evaluated as `>`, request context keys no longer checked for control
characters, and union reporting false instead of an error. The first five
produced wrong answers and failed the gates (14–399 wrong answers each). The
sixth made outcome sets empty, so the engine refused rather than answered:
the fail-closed design absorbed it. Two of the five survived the first
version of the gates; the cycle and depth-chain seeds and
`TestRustEdgeCases` were added until they failed. Those seeds also found the
fast-path nondeterminism above, in the unmutated port.

## Size and speed

**WebAssembly.** `make wasm-size` builds the evaluator for
`wasm32-unknown-unknown` (`opt-level = "z"`, no `wasm-opt`): **441 KiB, 156
KiB gzipped**, against 3.42 MiB / 0.92 MiB for wasmfga's Rust evaluator and
44.6 MiB / 8.7 MiB for its Go build. The difference is mostly the condition
evaluator (no CEL library, no regex engine, no chrono) and the lack of an
embedded datastore. The module exports the C ABI of `src/ffi.rs`; a
JavaScript host would also need an allocation export to pass buffers in,
which is not written yet.

**Speed.** wasmfga's native workloads (`make bench-rust`), median per check
on darwin/arm64, with reads answered inside an open transaction
(`benchmarks/results/rustcheck/`):

| workload | OpenFGA server, SQLite | atomicfga Go, SQLite | Rust, SQLite | atomicfga Go, pgx | Rust, pgx |
|---|---:|---:|---:|---:|---:|
| direct-grant | 1349 µs | 273 µs | 365 µs | 394 µs | 452 µs |
| empty-deny | 1343 µs | 453 µs | 601 µs | 391 µs | 394 µs |
| userset-chain-12 | 2361 µs | 1394 µs | 2215 µs | 2435 µs | 3881 µs |
| parent-chain-12 | 6894 µs | 3307 µs | 3596 µs | 4161 µs | 4160 µs |
| intersection | 846 µs | 582 µs | 523 µs | 402 µs | 383 µs |
| exclusion | 631 µs | 190 µs | 372 µs | 389 µs | 383 µs |
| cel-int | 1308 µs | 527 µs | 554 µs | 398 µs | 380 µs |
| snapshot-100000 | 592 µs | 281 µs | 353 µs | 499 µs | 510 µs |

The SQLite columns come from one run and the pgx columns from another
(the OpenFGA server's SQLite times varied by 2× between runs). Cost does not
grow with the store (`snapshot-*`), like atomicfga and unlike wasmfga's
snapshot port. The Rust path is within 1.1–1.3× of atomicfga's Go
evaluator on most workloads, and faster than the OpenFGA server on all. It is
slower on userset chains (1.6×: the default strategy reads more per level
than upstream's recursive one) and on exclusion (2×: both operands are
always evaluated, to know the subtrahend's cycle flag). Each step crosses
cgo with JSON; for a transaction that cannot pipeline, the host answers
reads one at a time, and the evaluator reports reads that a decided union
made moot, so they are skipped (9 statements fell to 5 on `direct-grant`).

## Status and next steps

A prototype: no wrong answer in about 39,000 compared checks, at the cost of
refusing roughly one check in four of upstream's suites. The next steps,
in order of refusals removed:

1. Port the recursive and weight-2 strategies (and the weighted graph that
   selects them) as further outcome-set producers, replacing the widening
   rule. This removes nearly all refusals in upstream's suites.
2. List and map parameters, `in`, and the CEL macros OpenFGA's users rely on.
3. Inline `$expression` conditions.
4. Error messages identical to upstream's (codes already are).
