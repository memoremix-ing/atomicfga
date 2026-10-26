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

**Server settings.** The evaluator follows the embedded server's resolve
node limit (`server.WithResolveNodeLimit`), read from the constructed server
(checksettings.go) and passed to the Rust engine when it is created. It
refuses every check while access control is enabled (only OpenFGA's Check
authorizes the caller) and for stores where the `weighted_graph_check` flag
selects OpenFGA's other Check implementation. The remaining server settings
change timing, not answers. If an upstream change keeps the settings from
being read, every check is refused (and `TestCheckSettingsAreReadFromTheServer`
fails).

## Upstream's answer is not always determined

OpenFGA resolves operands concurrently, and intersection and exclusion answer
with whichever decisive operand arrives *first*. The response's cycle flag,
which decides an enclosing exclusion, and the error reported can therefore
depend on goroutine timing. Every resolution step in the port therefore
yields the **set of outcomes** OpenFGA could produce (allowed, denied, denied
with a cycle, error), never a guess at one interleaving. The engine answers
only when every member of the final set gives the same API result
(`rust/check/src/outcome.rs`).

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
a fast path, it widens the outcome set (see below for how those predicates
are evaluated): it adds plain `false` beside a cycle,
refuses if the subtree came within three levels of the depth limit or
errored, and refuses if any relation beneath it admits conditional tuples.
The predicates rest on the weighted authorization model graph of OpenFGA's
`language` package. `rust/check/src/weighted.rs` ports it: the builder, weight
assignment with tuple-cycle resolution, the recursive and tuple-cycle marks,
and the four predicates (userset weights and wildcard sets, which only
ListObjects uses, are not ported). Every model the port loads derives its
strategy table this way, so no host needs OpenFGA's Go typesystem.

Go assigns weights visiting graph nodes in map order. Before porting, 1,793
recursive models were each rebuilt 50 times in OpenFGA: the table never
varied, so a port is well defined. `TestHegelWeightedGraphMatchesOpenFGA`
compares the port's table with OpenFGA's on random valid models of three
generators (one draws relations recursing along several paths at once):
10,457 models, 14,424 table entries, identical. Of six planted bugs, five
fail it; the survivor (cycles reached by several branches left unmarked) is
very likely equivalent, since such relations are marked recursive or
tuple-cyclic by another path, which the predicates check first.

Because a compiled model is a pure function of the model's JSON, storing it
next to the model is now only a cache: any host running the same build
derives the same table. atomicfga keeps compiled models in memory per
engine; a persistent cache would need to be keyed by the build that
produced it.

## Gates

`make test-rust`, with `-tags rustcheck`:

| gate | what | Rust-answered | wrong | refused |
|---|---|---|---|---|
| `TestRustUpstream` | OpenFGA's Check assertion suites (consolidated and ABAC YAML) and generated Check matrices, with and without check optimizations | 6,908 | **0** | 2,156 |
| `TestHegelRustCheck` | Hegel differential: random models (all rewrite operators, wildcards, usersets, recursive parents, conditions including unsupported ones), tuples written under one model version and checked under another, contextual tuples, request contexts with wrong types and forbidden characters, seeded parent/membership cycles and chains around the depth limit | 32,021 | **0** | 2,497 |
| `TestRustEdgeCases` | cycles in subtrahends and intersections, chains at the depth limit, userset users | 120 | **0** | 0 |
| `TestRustConfiguredLimits` | resolve node limits 1–10 and 25, on chains of every length up to 12; refusal under `weighted_graph_check` | 280 | **0** | 696 |

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

## Model compiler

`rust/check/src/compile.rs` decides, from a model's JSON alone (a
`WriteAuthorizationModelRequest` body or an `AuthorizationModel`, in
protojson), whether OpenFGA's `WriteAuthorizationModel` accepts it, and with
which error code:

1. the request's protobuf validation rules (`invalid_argument`): name
   patterns (RE2 semantics: lengths in characters, ASCII whitespace),
   lengths in bytes, required fields, enum values, item counts;
2. the `inline_expressions` flag check (`validation_error`), the type count
   limit and the model size limit (`exceeded_entity_limit`);
3. `typesystem.NewAndValidate` (`invalid_authorization_model`): schema
   version, duplicate and reserved names, each relation's rewrite, type
   restrictions, entrypoints and computed-relation cycles, then conditions
   (parameter type decoding, and compilation of the expression).

Within each stage every failure has the same code, so the verdict cannot
depend on which failure OpenFGA happens to report. A condition expression
outside the CEL subset (or with a constant subexpression the port does not
fold exactly as cel-go does, or a parameter named like a CEL keyword) is
*undecided*, unless another check fails for certain. The model size limit
is decided only for models whose compact JSON is under 128 KiB (the protobuf
encoding is at most about twice as large); larger ones are undecided.

The evaluator loads every model through the compiler, so it only ever
evaluates models that pass OpenFGA's checks. `RustCompileModel` exposes it to
Go; `afc_compile_model` to any other host.

Porting it surfaced two upstream quirks, both reproduced deliberately:

- **A valid model can be rejected.** `hasEntrypoints` clones its visited map
  shallowly, so marks made while exploring one operand leak into sibling
  operands. `tests/testdata/entrypoints-aliasing.json` is rejected for "no
  entrypoints" although every relation has one. The port reproduces the
  sharing (`Rc<RefCell<…>>` inner maps); a brute-force search over random
  models against a deep-copying variant found the witness.
- **Union arity is checked only where the search reaches.** A union with one
  operand is rejected, unless an earlier operand already provided an
  entrypoint, in which case the model is accepted.

**Gates.** `TestHegelRustCompileModel` draws models at three levels of
damage (well-formed with references to defined types and relations; with
semantic faults; with protobuf faults), at three fault densities so that a
model often carries a single fault. It compares each verdict with OpenFGA's,
and reports the mix of OpenFGA's rejection reasons, so that a check the
generator never triggers shows up. `TestRustCompileEdgeCases` pins the two
quirks above, arity cases and name lengths at the limits.

| gate | models | decided by Rust | wrong | undecided |
|---|---:|---:|---:|---:|
| `TestHegelRustCompileModel` (10,000 cases) | 26,047 | 25,918 | **0** | 129 |
| `TestRustCompileEdgeCases` | 8 | 8 | **0** | 0 |

Decided verdicts: about 1,800 valid, 20,000 `invalid_authorization_model`
(every `NewAndValidate` rule triggered), 2,400 `invalid_argument`, 1,500
`validation_error` and some `exceeded_entity_limit`. The undecided ones are
parameters named like CEL keywords or not named as identifiers, and
expressions using macros or lists.

**Mutation testing.** Eight bugs were planted one at a time: no
shared-map aliasing in `hasEntrypoints`, unions of one operand accepted, no
computed-cycle check, a tuple-to-userset requiring *every* tupleset type to
define the computed relation, name lengths counted in bytes, a
tuple-to-userset counted as directly assignable, no condition key/name check,
and lists decoded without their generic type. The random gate initially
missed four. Lowering fault density, adding boundary names and adding the
edge cases made the gates catch all eight.

## Canonical semantics and the Quint spec

`spec/check.qnt` defines Check declaratively, for one model that exercises
nested group memberships, recursive folder parents, a wildcard-only
relation, a tuple-to-userset, intersection, and an exclusion whose
subtrahend can run into a membership cycle. A check holds iff its
(object, relation, user) fact is in the least fixed point of the model's
rules over a finite universe, computed bottom-up. There is no evaluation
order, no cycle flag and no depth limit. The exclusion is stratified
(`blocked` does not depend on `viewer`), so the fixed point is well
defined. Its invariants (`ClosureIsFixed`, `BlockedNeverViews`) are checked
by Quint's simulator. Implicit tuples (`group:g1#member@group:g1#member`)
are excluded, as OpenFGA refuses to write them.

The evaluator has a `Semantics::Canonical` mode, in which a cycle means only
"not derivable along this path" (path-based loop checking is complete for
positive rules, and stratification keeps negated subgoals off the path), in
which nothing is widened for OpenFGA's planner, and which has no depth limit:
the visited path ends every cycle, so resolution ends over finitely many
tuples.

It answers within a stated domain, and refuses outside it (never answering
false for want of resources):

- **Stratified models.** OpenFGA accepts models in which a relation depends
  on itself through an exclusion's subtrahend (a group whose members exclude
  banned users, who may be groups' members). The least fixed point is not
  defined there, so canonical checks of such a model refuse.
- **Bounded work.** A check nests at most 100 deep (each level is native
  stack: about 2.5 KiB in release builds, and the evaluator runs on stacks
  it does not choose, WebAssembly's and cgo's) and makes at most 100,000
  resolutions. The nesting bound applies in OpenFGA semantics too, whatever
  the configured resolve node limit. Path-based search can take exponential
  work (ten mutually nested groups, checking a user in none of them,
  exceeds the bound); tabled or bottom-up evaluation would not, and is the
  way to widen this domain.

`rust/check/tests/canonical.rs` holds canonical checks to an independent
oracle: the spec's rules evaluated bottom-up in Rust, over random universes
far larger than the spec's (folder chains up to 70 long, extra and cyclic
parents, nested and cyclic groups). Its 24 seeds make 3,688 checks, all
equal to the least fixed point. Reinstating a depth limit of 25, or the
cycle flag, fails it. `rust/check/tests/quint.rs`
uses [quint-connect](https://github.com/informalsystems/quint-connect) to
replay random traces of the spec (tuple writes, deletes, checks) against it:
after every step the evaluator's last answer must equal the spec's. `make
test-spec` runs 200 traces of up to 40 steps (about 2,000 checks) in about
four seconds, with Quint pinned in `spec/package.json`.

Every check also runs in OpenFGA semantics, and the driver reports answers
that differ from the canonical one. With implicit tuples allowed, it found
one within seconds: a blocklist containing a group whose membership is
implicitly self-referential makes OpenFGA's default resolver deny access the
canonical semantics grants (the subtrahend's cycle flag decides the
exclusion). With implicit tuples excluded, none was found. The valid
counterpart, mutually nested groups, does not diverge on OpenFGA in
practice: its planner resolves the membership with the recursive strategy,
which has no cycle flag. The OpenFGA-compatible mode refuses there.

**Mutation testing.** Of four planted bugs (canonical mode keeping the cycle
flag, no self-defining check, wildcard reachability lost, wildcard
short-circuit in userset reads removed), the first three fail the gate. The
third survived until the wildcard-only relation `doc#public` was added to
the model. The fourth is an equivalent mutant: userset reads only return
`type#relation` users, so the short-circuit never fires (in OpenFGA either).

## Layout and the storage interface

`rust/` is a Cargo workspace:

| crate | what | depends on |
|---|---|---|
| `check` (`atomicfga-check`) | evaluator, model compiler, weighted graph, and `api::Fga`: OpenFGA's model, write and check APIs over any `store::Store` | serde, serde_json |
| `redb` (`atomicfga-redb`) | `Store`/`StoreMut` over redb transactions | check, redb |
| `ffi` (`atomicfga-ffi`) | the C ABI atomicfga's Go host links (static library) | check, redb |
| `web/evaluator` (`atomicfga-web`) | the evaluator as a browser module; storage imported | check |
| `web/redb` (`atomicfga-web-redb`) | a redb store as a browser module, in memory or on OPFS | redb |

The evaluator has no storage. Everything it needs from one is
`store::Store` and `store::StoreMut`: an exact tuple lookup, a scan of
`(store, object, relation)`, put and delete, and models by ID and latest
(greatest ULID), within a transaction the caller owns, with values as
opaque bytes (a tuple's encoded condition, a model's protojson). Writes are
validated by `api::Fga` (protobuf rules, model resolution, the
inline-expression flag, `ValidateTupleForWrite`, implicit tuples, context
size, duplicates, the 100-tuple limit, then missing and existing tuples
under `on_missing`/`on_duplicate`), before anything is written; checks run
on the evaluator's synchronous runner, whose reads the store answers as
OpenFGA's SQL datastores answer them. `store::MemoryStore` is the
reference implementation.

With redb, the application wraps its transaction (`atomicfga_redb::Write(&tx)`
or `Read(&tx)`): checks see the transaction's uncommitted writes, and
committing or aborting is the application's call, so authorization data
changes atomically with the application's own redb tables.

`TestHegelRustRedb` drives the same model writes, tuple writes and deletes,
and checks into the original OpenFGA server and into an in-memory redb
store served by the Rust port alone, and compares every outcome: 12,910
operations, none wrong. A case is stopped once the port refuses a model or a
write, as the stores could differ from then on. Of five planted bugs in the
redb adapter, four fail it; the fifth (userset reads ignoring the user's
type) has no effect, because every tuple read is revalidated against the
model before use.

An earlier mutant run left a counterexample that exposed a third upstream
quirk. With `on_duplicate: ignore`, OpenFGA compares the written tuple with
the stored one using `proto.Equal`. A stored condition always reads back
with a context, an empty one if none was written, so re-sending an
identical conditional tuple *without* a context fails as a conflict
(`Aborted`), while sending `context: {}` succeeds. `api::Fga` reproduces
it; `ignored_duplicates_compare_as_openfga_does` pins it.

## In browsers

Two independent modules, composed by the host (`web/`):

- `atomicfga_web.wasm`, the evaluator, imports eight store functions
  (`store.tuple_get`, `tuple_scan`, `tuple_put`, `tuple_delete`,
  `model_get`, `model_latest`, `model_put`, `fetch`) and exports the API
  (`write_model`, `write`, `check`, `compile`) with protojson in and JSON
  out. `web/atomicfga.js` binds the imports to a JavaScript store adapter.
- `atomicfga_web_redb.wasm`, a redb store with its own transactions, imports
  only five `opfs.*` functions: redb's `StorageBackend`, which
  `web/stores/redb.js` implements with a `FileSystemSyncAccessHandle` (in a
  worker). It knows nothing of authorization, so it can hold an
  application's other data too.

Store adapters share one small interface (begin/commit/abort, the tuple and
model functions): `stores/memory.js` (the reference), `stores/redb.js` (in
memory or on OPFS), and `stores/sqlite.js` (SQLite compiled to WebAssembly,
`@sqlite.org/sqlite-wasm`, on OPFS through its SAH pool VFS, which needs no
cross-origin isolation).

`make test-web` runs `web/test/` in Chromium, Firefox and WebKit
(Playwright's browsers), each on a persistent profile. Every adapter replays
one seeded workload of 121 transactions, 26 of them aborted (model writes;
valid, invalid, duplicate and conditional tuple writes and deletes; checks
in both semantics; 263 outcomes), and must match the reference outcome for
outcome and in the final state. Then the browser quits and relaunches on
the same profile, and the OPFS stores must still hold exactly what was
committed. All pass in all three engines.

### redb or SQLite for browser persistence

Measured with `make test-web` on darwin/arm64 (10,000 tuples loaded in
100-tuple transactions; per-check medians of 300, warm):

| | Chromium | Firefox | WebKit |
|---|---:|---:|---:|
| load, redb on OPFS | 286 ms | 794 ms | 181 ms |
| load, SQLite on OPFS | 564 ms | 539 ms | 181 ms |
| direct check, redb / SQLite | 17 / 26 µs | 67 / 63 µs | 17 / 17 µs |
| parent chain (11 reads), redb / SQLite | 105 / 98 µs | 393 / 300 µs | 67 / 83 µs |
| exclusion (7 reads), redb / SQLite | 54 / 57 µs | 240 / 190 µs | 43 / 47 µs |

| download | raw | gzipped |
|---|---:|---:|
| evaluator (`atomicfga_web.wasm`) | 557 KiB | 211 KiB |
| redb store (`atomicfga_web_redb.wasm`) | 563 KiB | 204 KiB |
| SQLite (`sqlite3.wasm`) | 849 KiB | 393 KiB |
| SQLite's JavaScript (`index.mjs`, unminified) | 628 KiB | 170 KiB |

Both are correct here and fast enough that a check costs tens of
microseconds either way; neither dominates on speed (redb loads faster in
Chromium, SQLite in Firefox, a tie in WebKit). redb is about half SQLite's
download. SQLite brings SQL, a mature file format other tools read, and
atomicfga's own SQLite datastore semantics; redb brings a single-language
stack and a smaller module. The redb backend is new code (five functions);
its durability rests on redb's commit protocol and OPFS `flush`, which these
tests do not stress (no crash injection).

## Message formats

The evaluator's messages are JSON. To see whether another format would
shrink the module, one crate holding the messages' shapes (a model with
nested types, a recursive rewrite and maps; a check request with contextual
tuples; read answers) was built for `wasm32-unknown-unknown` (`opt-level =
"z"`) with each library decoding them and encoding a response:

| library | format | KiB | gzipped KiB |
|---|---|---:|---:|
| serde_json (current) | JSON | 114 | 43 |
| serde_json `Value`, no derives | JSON | 94 | 37 |
| miniserde | JSON | 71 | 27 |
| nanoserde | JSON | 119 | 51 |
| ciborium (serde) | CBOR | 141 | 53 |
| minicbor (own derive) | CBOR | 84 | 25 |
| rmp-serde | MessagePack | 272 | 74 |
| flexbuffers (serde) | FlexBuffers | 203 | 77 |
| postcard (serde) | postcard | 37 | 13 |
| nanoserde | binary | 36 | 13 |

Most of the cost is code generated per message type and per format, not the
format: FlexBuffers, MessagePack and CBOR through serde are all larger than
JSON. Libraries built to avoid that code win (miniserde for JSON, minicbor
for CBOR, postcard or nanoserde's binary format). [Lite³](https://github.com/fastserial/lite3)
was not measured: it is a C library without an official Rust crate, built
for in-place access rather than small decoders.

## Size and speed

**WebAssembly.** `make wasm-size` builds the browser evaluator (the
evaluator, model compiler, weighted graph and API layer, storage imported)
for `wasm32-unknown-unknown` (`opt-level = "z"`, no `wasm-opt`): **557 KiB,
211 KiB gzipped**, against 3.42 MiB / 0.92 MiB for wasmfga's Rust evaluator
and 44.6 MiB / 8.7 MiB for its Go build. The difference is mostly the
condition evaluator (no CEL library, no regex engine, no chrono) and the
lack of an embedded datastore. Only about a fifth of the module is the
authorization logic itself; the rest is message decoding (serde_json's
per-type code), collections (`BTreeMap`, sorting, hash maps) and number
formatting, the levers for shrinking it (see Message formats).

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

1. Port the recursive and weight-2 strategies (the weighted graph that
   selects them is ported) as further outcome-set producers, replacing the
   widening rule. This removes nearly all refusals in upstream's suites.
2. List and map parameters, `in`, and the CEL macros OpenFGA's users rely on.
3. Inline `$expression` conditions.
4. Error messages identical to upstream's (codes already are).
