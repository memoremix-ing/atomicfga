# Correctness findings

Source line references refer to the reviewed commit identified in [README](README.md). Reproduction commands run from the repository root; their nonzero exits are expected until the reported invariants are repaired. See [validation](validation.md) for prerequisites and observed results.

## F01 — P1: model visibility survives an application savepoint rollback

**Locations:** [session.go](../session.go#L149), `modelVisible` and `WriteAuthorizationModel` around lines 274–293.

`WriteAuthorizationModel` puts the new ID in `txState.visible` and `written`. `modelVisible` trusts `visible` without another database read. The caller is allowed to create and roll back application savepoints directly, so a rollback can remove the model row without invalidating either map. A second `Session` wrapper shares the same stale state.

**Confirmed history:** create an application savepoint; write a model; check with its explicit ID to populate the upstream caches; roll back the savepoint; enumerate models and observe no model row; check again with that ID. The final check succeeds without an error instead of returning `authorization_model_not_found`. This reproduces on SQLite, Turso, PostgreSQL/sql, and PostgreSQL/pgx.

The particular probe returns a normal denial because no tuples were installed. The material defect is that a nonexistent model is accepted; with other tuples or contextual tuples, its cached rules can participate in authorization after their write was rolled back.

**Why existing tests miss it:** `RuleDiscardedSavepoint` sometimes writes a model but drops its ID. Later draws use only `working.models`, so the removed model is never requested explicitly in that transaction. Whole-transaction rollback tests get fresh transaction state and exercise a different case.

**Remedy:** cache immutable model *content*, but recheck visibility in the actual transaction whenever external savepoint changes can invalidate it. A visibility epoch is safe only if every relevant rollback is observable; the current public contract permits raw transaction access. Creating a new wrapper will not fix the problem because wrappers intentionally share state.

**Regression:** `TestModelSavepointRollback` in [go_test.go.txt](reproductions/go_test.go.txt). Include explicit Check and ReadAuthorizationModel, nested savepoints, requests with contextual grants, and both previously loaded and unloaded model content.

## F02 — P1: globally committed does not mean visible in this snapshot

**Locations:** [session.go:149](../session.go#L149), [atomicfga.go](../atomicfga.go#L243), and the shared-cache explanation in `docs/design.md`.

The engine-wide `committed` set skips model existence checks in every transaction. Immutability and permanence establish that model content will stay the same; they do not establish that an older transaction snapshot can see a newly committed row.

**Confirmed history:** establish a SQLite WAL reader snapshot; commit a newer model from a writer; check it from a fresh reader, which marks it globally committed and warms the caches; verify raw SQL in the old reader cannot see that ID; ask the old reader's Session to check that explicit ID. It returns no error. The same shortcut is present for PostgreSQL Repeatable Read/Serializable snapshots; those variants were not separately reproduced for this finding.

The explicit-ID result now depends on whether another transaction warmed process caches. This contradicts the promise that every request uses a model visible in the bound transaction and can mix newer model rules with older tuple/application state.

**Remedy:** keep content reuse separate from transaction visibility. An indexed existence query is the simple safe baseline. Any optimization needs a demonstrated relationship between the model's commit and this transaction's snapshot; an engine-wide boolean cannot provide that. Account for F01 before relying on per-transaction positive visibility caching either.

**Regression:** `TestModelOutsideSnapshot` in [go_test.go.txt](reproductions/go_test.go.txt). Add old-snapshot tests on both PostgreSQL adapters, varying cache warmup order and explicit versus latest IDs. Require API results to agree with SQL visibility.

## F03 — P1: translated Turso aborts are not recorded in session state

**Locations:** [internal/bind/turso.go:32](../internal/bind/turso.go#L32), [internal/bind/session.go:104](../internal/bind/session.go#L104), [internal/bind/abort.go](../internal/bind/abort.go#L20).

`translateTurso` turns `turso: database is busy` into `*abortError`. `Session.observe` only latches errors recognized by `abortsTransaction`, which checks SQLSTATE and modernc SQLite codes. The translated abort wrapper and its ordinary Turso cause satisfy neither test.

**Confirmed injection:** pass the exact recognized Turso busy message through the real translation and loopback driver. The returned error matches `ErrTransactionAborted`, but `Session.Aborted()` remains nil. A following INSERT executes successfully on the fake backend. This tests the production classification/state transition; it does not claim to have forced a real Turso engine to auto-roll back.

The session therefore breaks its documented refusal policy. The public wrapper also loses the reliable route to gRPC `Aborted`, because `Session.wrap` consults the unlatched state after upstream error translation. Whether any particular driver error has already rolled back the physical transaction is a separate question; safety must not depend on allowing more work after declaring it aborted.

**Remedy:** make an existing abort classification itself sufficient to poison session state, and preserve its cause. Test classification, persistent state, refusal of Query/Exec/BeginTx, and public gRPC translation as one contract.

**Regression:** `TestTursoAbortIsLatched` in [go_test.go.txt](reproductions/go_test.go.txt). Add a driver-level contention test as a complement to deterministic injection.

## F04 — P1: Repeatable Read is overpromised for check-and-act serialization

**Locations:** [README.md:109](../README.md#L109), [tests/guarded_test.go](../tests/guarded_test.go#L35).

The README recommends either PostgreSQL Repeatable Read or Serializable to make a decision safe against concurrent revocation. Repeatable Read provides a stable snapshot, but permits serialization anomalies. See PostgreSQL's own [transaction isolation documentation](https://www.postgresql.org/docs/17/transaction-iso.html#XACT-REPEATABLE-READ).

**Confirmed history without a shared clock:** an edit transaction reads an allowed grant. A revocation transaction reads the document's edit count as zero, deletes the grant, and commits. The edit then increments the document's count and commits. Both commit under Repeatable Read on PostgreSQL/sql and PostgreSQL/pgx. Neither serial order explains both observations: edit-first would make the revoker see the edit; revoke-first would deny the edit. Running the same history at Serializable produces SQLSTATE `40001` on both adapters.

The existing Hegel test runs PostgreSQL at Serializable and makes every operation update one shared clock row. That clock imposes an additional write conflict absent from ordinary applications. It is useful for testing its workload, but does not establish the broad Repeatable Read claim.

**Remedy:** document Serializable plus whole-transaction retry as the general route to serializable application/authorization histories, or specify an explicit common locking protocol that every relevant operation follows. Distinguish snapshot consistency, serializability, and the stronger policy “a revocation committed before this overlapping edit must win.” A valid serialization can order overlapping transactions differently from their commit order; Serializable alone should not be described as universally guaranteeing that stronger policy.

**Regression:** `TestRepeatableReadDoesNotSerializeCheckAndAct` in [go_test.go.txt](reproductions/go_test.go.txt), with passing Serializable controls. Keep a test without the global clock so instrumentation does not supply the property under test.

## F05 — P1: Rust cached model identity is weaker than the writable API

**Locations:** [rust/check/src/api.rs:207](../rust/check/src/api.rs#L207), [api.rs:235](../rust/check/src/api.rs#L235), [rust/check/src/lib.rs:187](../rust/check/src/lib.rs#L187), and all `StoreMut::put_model` implementations.

Storage keys models by `(store, id)`, while the evaluator caches by `id` alone. The API accepts caller-chosen IDs and `put_model` overwrites existing content. Neither global uniqueness nor immutability is enforced by this boundary. A comment assuming immutable ULIDs cannot make either invariant true.

**Two confirmed stale-grant cases:**

1. Warm model ID X in store A, where viewer means editor. Store a different model under X in store B, where viewer means editor minus blocked. With editor and blocked tuples in B, the warm evaluator grants; a fresh evaluator denies.
2. Warm a permissive model, then successfully replace the same store/ID with the restrictive model. The warm evaluator still grants; a fresh evaluator denies.

These are deterministic caller-selected ID reuse, not random ULID collisions. They affect the standalone Rust API and its browser host. The Go Session normally receives upstream-generated model IDs and does not expose the same model replacement API.

**Remedy:** choose and enforce one identity contract. Use a cache namespace that includes the store and, if an evaluator can span databases/adapters, the storage namespace. Make model writes insert-only with checked same-content idempotency, or use an immutable content identity. Merely evicting on `write_model` does not cover other stores, aborted writes, snapshots, or multiple writers/evaluators. Validate supplied IDs too; this method currently does not call `ulid_ok`.

**Regressions:** `same_id_in_two_stores_must_not_share_compiled_models` and `model_overwrite_must_not_leave_a_stale_grant` in [rust.rs](reproductions/rust.rs). Extend across two adapters and rollback/reuse histories.

## F06 — P1: malformed condition bytes can grant unconditionally

**Locations:** [rust/check/src/api.rs:70](../rust/check/src/api.rs#L70) and the `read` conversion at lines 104–132.

`decode_condition` returns `None` both for the legitimate empty encoding and for failure to decode nonempty bytes. The read path turns either into `Tuple { condition: None }`.

**Confirmed case:** a relation allows both unconditional `user` and `user with guard`. A tuple carrying a valid false guard denies access. Replace its bytes with the invalid JSON `{` through the storage interface; the same check grants. Because the unconditional type restriction is valid too, later tuple validation does not recover the lost distinction.

This requires malformed stored/adapter data; ordinary validated writes do not intentionally create those bytes. That is still a meaningful trust boundary for an API designed to compose with user-supplied stores and persistent byte formats. Invalid encoding must not weaken a guard.

**Remedy:** return `Result<Option<Cond>, DecodeError>`; only empty bytes mean no condition. Propagate malformed nonempty content as an internal error or refusal. Apply the same rule wherever condition bytes are read, including duplicate comparison. Avoid `ok()`/default recovery for authorization-bearing data.

**Regression:** `corrupt_condition_must_not_become_unconditional` in [rust.rs](reproductions/rust.rs). Fuzz arbitrary nonempty values and require no grant caused by dropping a malformed condition.

## F07 — P2: RustCheck does not honor a configured resolution depth

**Locations:** [rust/check/src/resolve.rs:16](../rust/check/src/resolve.rs#L16), [rustcheck.go](../rustcheck.go#L114), `WithServerOptions` in `atomicfga.go`.

Rust hardcodes `MAX_DEPTH = 25`. The Go host does not transfer the engine's configured resolver limit to it. `RustCheck` only falls back on refusal; a normal Rust grant bypasses the configured Go limit.

**Confirmed case:** create an engine with `server.WithResolveNodeLimit(1)` and a valid doc → group → group2 membership path. Go Check returns authorization-model-resolution-too-complex (2002); `RustCheck` returns allowed=true and no error. A single-hop probe did not diverge because upstream can optimize it, which is why the retained reproduction uses two hops.

**Remedy:** pass explicit evaluator configuration through the host/ABI and include relevant settings in compilation/cache identity. Until supported, refuse Rust evaluation for configurations whose semantics it cannot reproduce. Review breadth, concurrent-read, and other resource settings similarly; those were source-level concerns, not separately reproduced here.

**Regression:** `TestRustHonorsConfiguredDepth` in [go_rust_test.go.txt](reproductions/go_rust_test.go.txt); run with `-tags rustcheck` after building the native library. Add a configuration conformance matrix around exact boundaries.

## F08 — P2: browser transaction helper can commit a partial API write

**Locations:** [rust/check/src/api.rs:460](../rust/check/src/api.rs#L460), [web/atomicfga.js:153](../web/atomicfga.js#L153).

Rust validates the request before mutation, but successive storage writes can still fail midway. `Fga::write` turns that failure into a normal JSON `error` result. The JS `transaction` helper commits whenever the callback returns normally, including when it returns that JSON error.

**Confirmed injection:** use a MemoryStore adapter that throws on the second tuple insertion. `transaction(adapter, true, () => evaluator.write(...two tuples...))` returns `internal_error`, yet the first tuple is committed. The top-level Rust module comment warns callers to roll back on storage failure, but the helper and the method-level “on an error nothing is changed” wording provide incompatible defaults. The FFI's `in_write` does check error/refusal results before committing and is a useful local example.

**Remedy:** use a typed operation outcome and a transaction scope that cannot commit after a storage failure. Alternatives include throwing a dedicated storage exception through the JS API, poisoning the scope, or providing an API-aware helper that rejects error/refusal outcomes. Checking only the callback's final return is insufficient if earlier failed operations were ignored. Clarify whether request-level savepoint atomicity or mandatory whole-transaction rollback is the contract.

**Regression:** storage-failure test in [web.mjs](reproductions/web.mjs). Inject failure at every delete/write position, then try a later successful operation and commit to test failure poisoning.

## F09 — P2: async transaction callbacks commit before their work

**Location:** [web/atomicfga.js:153](../web/atomicfga.js#L153).

`transaction` accepts an ordinary callback with no runtime guard. An async function returns a Promise immediately; the helper commits it as a successful result before any continuation after `await` runs. The interfaces are documented as synchronous, but this common JavaScript mistake is accepted silently.

**Confirmed order:** `transaction(adapter, true, async () => { await Promise.resolve(); write(); })` records `begin, commit, write`. A later rejection likewise occurs after commit. SQLite adapter writes made afterward can execute outside the intended transaction; redb instead requires an open write transaction.

**Remedy:** reject thenables before commit, abort the scope, and document a synchronous callback type. Where an async transaction API is desired, design it separately with ownership across suspension and concurrency constraints. A synchronous safe default should be mechanically enforced.

**Regression:** async-callback test in [web.mjs](reproductions/web.mjs), plus rejection and nested async cases. Rejection cannot cancel already scheduled arbitrary JS work, so transaction-state enforcement in adapters also matters.

## F10 — P2: nested browser redb writes trap before validation

**Location:** [rust/web/redb/src/lib.rs:181](../rust/web/redb/src/lib.rs#L181).

`begin` opens the new redb transaction before checking whether `TX` already holds one. With a live write transaction, a second `begin_write` attempts to acquire redb's writer lock before reaching the intended already-open error.

**Confirmed behavior:** load the actual redb WASM module in Node, open an in-memory store, and call `begin(true)` twice. The second call throws a WebAssembly `unreachable` trap, rather than the intended “a transaction is already open” error. This was not separately exercised inside each browser. The WASM profile aborts panics, so this path cannot rely on Rust unwinding for clean recovery.

**Remedy:** validate the transaction state before acquiring any new transaction, ideally through a state transition with one owner. Test double-begin, read-to-write nesting, double-end, close-with-open-transaction, and operations after close.

**Regression:** nested redb test in [web.mjs](reproductions/web.mjs), executed in a bounded child process so a blocking implementation cannot hang the review runner.

## F11 — P2: canonical mode does not implement the full documented domain

**Locations:** [rust/check/src/resolve.rs:64](../rust/check/src/resolve.rs#L64), `Semantics::Canonical` in [lib.rs](../rust/check/src/lib.rs#L39), and the opening of [spec/check.qnt](../spec/check.qnt).

The specification describes finite-universe least-fixed-point derivability without a depth limit. Canonical mode only changes cycle handling and strategy widening; it still executes the unconditional depth-25 guard.

**Confirmed case:** a finite chain of 26 parent edges ends in an unconditional viewer grant. Canonical mode returns too-complex instead of the derivable true result. The Quint universe has only two folders, so current trace replay cannot expose this mismatch.

This is a semantic-domain/availability gap, not a reproduced false grant. Do not simply remove the cap: recursion, exclusions, finite exploration, and resource exhaustion need an explicit specification and implementation strategy.

**Remedy:** either document a bounded evaluator implementing the canonical rules only within a stated domain, or implement and verify a suitable fixed-point/worklist algorithm, including stratification requirements for negation. Expand the specification/test universe to cover boundary depth and graph shapes, and distinguish semantic false from resource exhaustion.

**Regression:** `canonical_semantics_must_cover_a_derivable_long_chain` in [rust.rs](reproductions/rust.rs).

## F12 — P2: JS reference store does not enforce transaction capability

**Location:** [web/stores/memory.js:13](../web/stores/memory.js#L13).

Read transactions save references to the current maps instead of copies, while mutators do not check whether a write transaction is open. A write inside `begin(false)` changes those maps; `abort()` then restores the same already-mutated maps. Operations are also accepted without any transaction.

**Confirmed case:** after `begin(false)`, `tuplePut` succeeds instead of rejecting the write. The source then establishes why abort cannot undo it. The production redb adapter rejects writes without a write transaction, so the in-memory reference and persistent adapters disagree on lifecycle behavior.

**Remedy:** represent idle/read/write states explicitly and gate every method. A reference implementation should enforce the shared contract, including failure behavior. Give returned mutable byte arrays an ownership policy as well: current getters expose references to stored arrays, another potential mutation route.

**Regression:** reference-adapter test in [web.mjs](reproductions/web.mjs). Run the same lifecycle suite against all adapters, checking rollback state as well as returned errors.

## F13 — P1: SQLite automatic rollback allows later writes outside the transaction

**Locations:** [internal/bind/abort.go](../internal/bind/abort.go#L20), [internal/bind/session.go:104](../internal/bind/session.go#L104), and [immediate.go](../immediate.go#L33).

The abort detector recognizes SQLite BUSY/LOCKED, but an error can end the underlying SQLite transaction for other reasons. `ImmediateTx` uses an acquired connection and raw `BEGIN IMMEDIATE`; its `done` flag changes only on caller Commit/Rollback. If the database itself rolls the transaction back and the detector does not notice, further operations on that connection run in autocommit mode.

**Confirmed with the actual modernc driver:** add an application trigger on `store` that calls `RAISE(ROLLBACK, 'review rollback')` for one store name. Begin an `ImmediateTx`, then call Session.CreateStore with that name. It returns an internal error. Call CreateStore again with a different name through the same Session. It succeeds, and another connection sees the new store **before any caller Commit**. The failing statement passed through the Session, so this is not an error made invisible by bypassing atomicfga.

The prerequisite is a trigger that rolls back the transaction; the stock migrated schema does not install this trigger. SQLite exposes trigger rollback through the constraint error family, as described in its [RAISE documentation](https://www.sqlite.org/lang_createtrigger.html#the_raise_function). The consequence demonstrates the exact unsafe state the session's abort policy is intended to prevent. Other automatic-rollback causes and the other transaction adapters were not established by this probe.

**Remedy:** make actual transaction liveness part of the backend/transaction ownership contract, and refuse all work once the underlying transaction has ended. Where liveness cannot be queried safely, conservatively poison after errors that may roll back the transaction, including failed savepoint recovery. Error numbers alone may not distinguish statement-only and whole-transaction trigger failures. Test the physical connection state and external visibility, not just the returned error wrapper. Do not perform a potentially committing write as a liveness probe.

**Regression:** `TestSQLiteRollbackTriggerMustPoisonTransaction` in [go_test.go.txt](reproductions/go_test.go.txt). Require both `ErrTransactionAborted` and no external visibility for subsequent writes. Extend to application rows, database-generated rollback causes, savepoint cleanup failure, and `*sql.Tx`/Turso behavior where supported.
