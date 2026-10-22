# Architecture, performance, verification, and documentation

This complements the reproduced [correctness findings](correctness.md). The performance conclusions below come from source analysis and the browser test timings produced during validation. The full published SQL benchmark matrix was not rerun, and asymptotic concerns are not presented as measured regressions.

## Keep the Go adapter focused

The main architectural decision is appropriate: reuse upstream's server, resolver, validation, and SQL implementation while replacing transaction ownership. The loopback `database/sql` driver is more indirect than implementing a new datastore, but centralizes the dangerous parts—statement serialization, savepoints, cancellation, and completion—rather than repeating them across every API method.

The PostgreSQL port is the justified exception. Its use of upstream builders and shared write helpers reduces drift. Keep a mechanical comparison of query shapes and lock ordering against the tested upstream revision, including condition predicates, pagination boundaries, collation, and errors. Conformance tests demonstrate outcomes on their inputs; they cannot by themselves establish that all future SQL changes remain equivalent.

Buffering results is necessary for the current upstream concurrent iterator model on one connection. Returning live rows to independent resolver branches would reintroduce connection-busy failures and potential deadlocks. Improvements should preserve that ownership guarantee while bounding or reducing buffer cost.

The registry's weak transaction identity is purposeful, and the cleanup/concurrency tests are useful. Nevertheless, pointer identity is only a proxy for a transaction boundary. Document that a custom wrapper must preserve identity consistently, that nested pgx transaction objects can represent savepoints on one physical connection, and whether multiple Engines may wrap the same transaction. Currently the registry and semaphore are per Engine: the “one state per transaction” guarantee does not automatically span Engines. This is a scope/documentation concern; overlapping multiple-Engine calls were not reproduced in this review.

## Separate four distinct facts about models

The current caches conflate facts that need different lifetimes:

1. A blob decoded into immutable model content.
2. A model identity bound to exactly that content in a particular namespace.
3. A model row visible in a specific transaction snapshot.
4. A transaction that remains live after savepoint rollback or database error.

Content caching can be long lived. Visibility and liveness cannot be inferred merely from content immutability or another transaction's observation. F01, F02, and F05 are consequences of treating these as interchangeable. Prefer explicit model handles/content identities and a separate visibility decision to increasingly elaborate invalidation heuristics.

For the current caller-owned SQL contract, a small indexed visibility query per request is a reasonable correctness baseline. Optimize only after a proof obligation states which externally possible transaction operations leave a positive answer valid. Raw caller savepoints make an unobserved monotonic visibility cache particularly difficult to justify.

## Normalize wire input once

Rust has separate serde shapes for model compilation, model execution, and API validation. The compiler accepts aliases such as `schemaVersion` and `typeDefinitions`; `model::ModelJson` expects snake_case. `Fga::write_model` validates the original input, stores the original spelling, and the evaluator reparses it through another shape. This creates a route for accepted input to become unevaluable later. The check/write API shapes also silently ignore unknown fields. These observations come from source inspection; they are not included in the reproduced finding count.

Use a single normalized representation after decoding, with validation producing a capability such as `ValidatedModel`. Compile and store that representation rather than reparsing arbitrary original JSON. Keep transport validation separate where protobuf rules differ, but share the actual field definitions and normalization. Test lowerCamelCase and snake_case, unknown fields, duplicate aliases, enum spellings, nulls, invalid Unicode, and malformed nested oneofs across every public boundary.

Similarly, prefer an outcome enum such as `Allowed`, `Denied`, `Error`, and `Refused` over untyped JSON objects inside the evaluator/API implementation. Encode JSON at host boundaries. FFI `in_write` already illustrates why transaction commit must inspect the outcome, not just whether a Rust/JS function returned.

The small `Store` interface is easy to implement and test, but “opaque bytes” still have an authorization-bearing format. Specify empty versus malformed condition values, model identity, byte ownership, ordering, transaction capability, and failure semantics. Rust's `StoreMut` is a useful capability distinction; JS should enforce equivalent state rather than trusting a boolean passed earlier.

## Runtime and algorithm suitability

### SQL path: suitable baseline, with operational bounds missing

Point reads and upstream relational indexes are a good fit for source-at-check-time evaluation. Avoiding a complete tuple snapshot per request is the right scaling choice. The pgx queue's immediate leader and opportunistic batching save round trips without deliberately delaying isolated reads. Dropping queued losing branches is useful work avoidance, not just a throughput trick.

There are still material limits to make explicit:

- [bind/session.go](../internal/bind/session.go#L200) strips cancellation before database execution; savepoint cleanup uses background contexts. A request deadline therefore does not bound an in-flight query, lock wait, cleanup, or `Call.Finish`. Preserve the reason for detaching branch cancellation, but provide a separately enforced transaction/database execution budget. PostgreSQL statement/lock limits and driver/network failure handling should be exercised, not just mentioned. Aborting a timed-out transaction is preferable to an unbounded wait when it can be done safely.
- Complete result buffering means memory is proportional to rows and bytes returned, and branch fan-out can keep multiple buffers alive. Add result-byte, row, and queued-statement budgets with explicit error outcomes. Streaming through the same connection is not a safe drop-in fix.
- [pipeline.go](../internal/bind/pipeline.go#L74) holds the semaphore while repeatedly draining new batches until the queue is empty. Under sustained read arrivals, writes and completion barriers may wait behind multiple batches. Measure per-call latency and fairness, then consider bounded batches/yielding between batches. The public contract permits concurrent calls on one transaction, making this a valid workload.
- Once visibility shortcuts are repaired, retime explicit-ID and latest-ID operations. Current performance claims rely in part on skipping a correctness-relevant query.
- Turso's fabricated planner statistics are an empirical workaround for a particular schema and engine. Make index-name/schema drift and query plans mechanically checkable. `seedTursoStatistics` writes entries individually and treats any tuple-table statistics as sufficient; partial seeding after failure or concurrent migrations deserves an idempotency test. This is a source-level robustness concern, not a reproduced query-plan failure.

### Rust/redb: avoid whole model-history reads

[redb::latest](../rust/redb/src/lib.rs#L70) scans from the beginning of a store's models to the end, cloning each model blob before keeping only the last. A default check calls `latest_model` before consulting the decoded cache. For M versions with total serialized size S, that is O(M) traversal and O(S) copying per lookup, despite the ordered key index. JS MemoryStore also scans its model map; SQLite uses `ORDER BY id DESC LIMIT 1`.

Use a correctly bounded reverse range or a transactionally maintained latest pointer. Verify predecessor/successor stores, empty IDs, Unicode store names, and maximum IDs so a range optimization does not cross namespaces. The interface also couples identity lookup to full model retrieval: split those if warm checks should avoid fetching/copying a blob they already decoded.

Benchmark 1, 100, and 10,000 model versions separately from tuple count. Current tuple-scale claims do not establish model-history scaling.

### Rust storage query shape: simple but increasingly expensive for mixed relations

[api::read](../rust/check/src/api.rs#L123) implements `ReadReq::Usersets` by asking the store for *every* tuple at the object/relation, then filtering users in memory. A relation mixing thousands of direct users with a few groups makes a userset lookup read and copy all those direct users. SQL's `ReadUsersetTuples` can filter `user_type` and restrictions in the database. A key-prefix index alone does not fix the Rust query shape.

If this is an intended workload, add a query capability/index for userset/wildcard reads or a typed filter that adapters must implement. Keep one simple scan for inherently broad TTU reads. Measure returned rows and bytes, not just `reads_total`: a single read can represent one indexed row or an entire high-cardinality relation.

### Rust executor: breadth can cost much more than depth suggests

[JoinAll](../rust/check/src/exec.rs#L146) visits its child vector on every poll. [Task::run](../rust/check/src/lib.rs#L442) deliberately answers one read per step. For a wide denying userset search, all W children can be created together and then the W-element structure is revisited across roughly W responses. This creates O(W²) vector-visitation work in that shape, before considering nested branches. The code also eagerly allocates the child futures and read requests; the depth-25 guard does not bound breadth.

One-at-a-time execution helps skip reads made moot by a grant, so batching everything unconditionally is not a universal improvement. Compare bounded chunks, readiness-directed polling, or a small explicit work queue. For the Go host, [answerReads](../rustcheck.go#L281) launches a goroutine per read in the step; give that path a bounded concurrency budget too. Verify refusal/outcome-set correctness before changing scheduling or early exits.

Each resolved node can also run a new model-graph BFS via [path_exists](../rust/check/src/model.rs#L464), and strategy widening may traverse model dependencies via `conditional_below`. Both depend on immutable model structure and are candidates for compiled summaries/memoization after profiling. The Rust decoded-model HashMap is unbounded, including failed/aborted/transient models, unlike the bounded Go caches. Document or bound retained model memory for long-lived evaluators.

### CEL subset: appropriate for a prototype, substantial semantic maintenance

The conservative refusal policy is preferable to guessing unsupported CEL behavior. However, this is also a custom parser, type checker, parameter conversion implementation, timestamp/duration/IP parser, arithmetic evaluator, and cost estimator. It is a large part of the trust surface despite the small WASM artifact.

Add direct differential fuzzing against the pinned cel-go implementation over both valid and malformed expressions/values. Include numeric boundaries, NaN/infinity encodings, Unicode escapes, fractional timestamps/time zones, duration overflow, network-prefix spellings, unknown/missing parameters, and error absorption. Existing generated condition menus cover useful examples but are not a general expression generator. `Program::run` checks its accumulated cost after evaluation; that is a conformance estimate, not necessarily a preemptive CPU/allocation budget. Bound parsing and execution independently.

### Benchmark interpretation

The published SQL method responsibly separates in-transaction cost from transaction-per-operation cost, compares outcomes before timing, and labels Turso's SQLite baseline. Preserve those distinctions. The browser suite ran successfully during this review, but its memory reference performs full-map scans and copies maps for writes; beating it does not establish general scalability.

Add tail latency, allocation bytes, peak buffered bytes, cancellation-drain time, and transaction lock-hold duration. Measure cold and warm model caches, low/high RTT, concurrent callers on one transaction, concurrent transactions, skewed groups, broad denies, cycles, exclusions, conditional failures, and model churn. Compare identical isolation/durability and state clearly when a baseline lacks an equivalent atomic operation. Record commit IDs, toolchains, seeds, dataset distributions, planner statistics, and raw commands with results.

One tooling cleanup: `bench/bench_test.go`'s `keepAlive.Cleanup` discards database cleanup callbacks. Process exit releases connections but does not execute PostgreSQL `DROP DATABASE` or remove manually made temporary directories. Replace it with an explicit suite-lifetime owner/cleanup if benchmarks are run repeatedly. This was identified by inspection; the full benchmark suite was not run during review.

## Make confidence mechanically checkable

### Extend the state machines at the missing boundaries

Keep tombstoned/rolled-back model IDs in a history separate from live model choices. Explicitly request them after savepoint rollback, whole rollback, and commit. Maintain old snapshots while other transactions commit and warm caches. Vary wrapper identity and engine reuse independently. Compare model visibility with raw SQL, not only an OpenFGA instance whose caches may share the same assumption.

For transaction scheduling tests, retain the existing concurrent workload but add histories without a common clock row. Instrumentation should not introduce the serialization conflict needed to make a test pass. Use barriers and deterministic fake-driver events to control queued versus admitted versus in-flight statements. Existing sleeps in bind tests are useful smoke tests, but not a substitute for exercising each state transition deliberately.

Specify the adapter state machine: idle call, queued operation, exclusive savepoint, aborted transaction, finished call, and cleanup outcome. A small model plus trace replay would target the hardest Go-specific logic more directly than the current authorization-only Quint model. Include errors during SAVEPOINT, rollback-to, release, buffered row iteration, batch close, and the final losing resolver branch.

### Prevent conformance from becoming vacuous

[rustGate.compare](../tests/rustcheck_test.go#L64) records refusals and accepts them. That is appropriate for sound partial evaluation, but an increase in refusal can hide lost functionality while leaving the differential gate green. The compiler/redb gates have similar considerations.

Maintain a required-answer corpus, per-feature allow/deny/error coverage, and a reviewed refusal budget. A gate should fail if a supported feature stops being answered. Track refusal categories and totals as artifacts, with stable seeds and a small regression corpus. Keep unsupported-feature tests separate from “must decide” tests.

Quint currently explores one fixed small authorization model with finite sets and sampled traces. Its two invariants and replay are useful; they do not verify all model compilation, arbitrary CEL, transaction/cache behavior, depth boundaries, or OPFS durability. Document that scope, and independently verify any general soundness claims about outcome-set reducers and planner widening. Small exhaustive truth tables can establish the reducer algebra more directly than only end-to-end randomized checks.

### Separate storage equivalence, evaluator equivalence, and persistence safety

The browser suite deliberately uses one evaluator for every adapter. It establishes storage agreement on that workload, not independent evaluator correctness. F05/F06 can agree across all adapters and still be wrong. Add cross-language oracle vectors exported from Go and input-byte tests at the WASM boundary.

Run one lifecycle conformance suite against every store: idle/read/write transitions, writes without capability, nested begins, missing models/tables, malformed bytes, adapter exceptions, commit/abort failures, and repeated close. Cover two stores and two adapters sharing an evaluator. Include application rows/keys in the same transaction if atomic application/FGA persistence is part of the browser promise; the current JS adapter API exposes only FGA-specific operations even though the Rust redb crate permits broader application tables.

Restart-after-clean-commit is useful but does not establish crash durability. Use controlled interruption/failure around OPFS writes, truncate, flush, commit, and reopen. The JS OPFS callbacks can throw rather than returning the Rust ABI's status code; test those exceptions explicitly to determine whether a trapped module/database can be reused or must be discarded. No crash-safety conclusion is made here.

### Make the documented gate reproducible

There is no checked-in CI configuration in the reviewed file inventory. Add one canonical gate that runs with `ATOMICFGA_REQUIRE_DATABASES=1`, verifies which variants actually ran, checks the intended upstream source revision/tree, and records toolchain and generator seeds. Local sibling dependencies fit the project's source-first model; the missing piece is a mechanically checked relationship between `upstream.json` and what was actually built.

The documented Rust subset command passes, but ordinary native `cargo test --release --workspace` attempts to link browser imports and fails. Give browser-only crates target-specific test configuration or provide an obvious aggregate command that explains/excludes them. Avoid making future reviewers infer the correct native/WASM test matrix from a linker error.

## Documentation that would make the next review faster

1. **A support matrix.** Mark Go SQL, native Rust, cgo fallback, canonical mode, and each browser adapter separately: supported operations, semantics, flags, limits, fallback/refusal behavior, storage format stability, and production/prototype status.
2. **A precise transaction contract.** State snapshot and serialization guarantees, savepoint behavior for every API, externally issued rollback limitations, per-Engine identity scope, nested pgx transactions, custom wrapper responsibilities, close/concurrency rules, and error/commit retry policy. Provide examples with complete error handling.
3. **An error-state matrix.** For each backend/error family, say whether the statement failed, a savepoint can recover, the whole transaction is unusable, the server error remains identifiable, and subsequent calls are refused. Explain request cancellation versus database execution timeout and cleanup timeout.
4. **Cache proof obligations.** List keys, namespace, capacity/eviction, content immutability source, visibility assumptions, snapshot behavior, and invalidation for every cache, including upstream caches and Rust failed-model entries.
5. **The Store/ABI specification.** Version byte encodings, absence/error distinctions, bounds, ordering, buffer ownership, pointer/result lifetimes, exception/trap handling, model insertion policy, and transaction state transitions. Supply a short adapter author guide plus the shared conformance suite.
6. **A semantic scope statement.** Distinguish OpenFGA-compatible partial evaluation from canonical rules, explain supported negation/recursion, resource exhaustion, custom server options, nondeterministic upstream outcomes, and what a caller must do on refusal. Browser hosts have no automatic Go fallback.
7. **A verification map.** Connect each invariant to exact tests/specs, oracle independence, random domains, mandatory-answer coverage, known blind spots, seed/replay commands, and mutation-test artifacts. Distinguish historical counts in docs from a particular reproducible run.
8. **An upstream-update checklist.** Enumerate files/API paths to compare when the sibling branch advances: cache defaults and new caches, context detachment, resolver lifetime, datastore SQL, migrations/indexes, protobuf validation, CEL conversion, and planner strategies. Make update checks executable where possible.
9. **An operational guide.** Cover database timeouts, statement/result budgets, lock duration and retry/backoff, model churn/cache memory, changelog horizon bounds, Turso migration/statistics lifecycle, OPFS quota/failure recovery, and observability of refusals/aborts/dropped work. Distinguish library decision correctness from who is authorized to submit models/tuples; the embedding application owns that policy.

The strongest next abstraction is one where validated immutable models, transaction visibility, write capability, and operation outcomes are represented explicitly. That would remove several failure modes at their construction points and make the remaining guarantees much easier to review and test.
