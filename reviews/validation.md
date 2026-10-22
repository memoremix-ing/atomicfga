# Review validation and reproduction

## Reviewed inputs and environment

- atomicfga source parent: `7c1c0043263ec70c6f84bffbe12029b634ff40a9`; initial working-copy commit `682da45e` was empty.
- OpenFGA source parent: `22c4d5ddf4d5969bee10c24e468a5acfbcf26837`; its working-copy commit was empty and its tree clean, matching `upstream.json`'s tested commit.
- Host: macOS/darwin arm64. Go `go1.27.1`; Cargo `1.92.0`; Node `v24.22.1`.
- Existing PostgreSQL test service was available at the configured/default local URI. No database service was started, stopped, or reconfigured for this review. Tests created and cleaned their own databases through the existing helpers.
- Existing dependency installations/build caches were available. Test runs produced ignored build/test artifacts; all tracked review additions are under `reviews/`.

## Existing gates run

All commands below completed successfully unless marked otherwise. Hegel used `ATOMICFGA_HEGEL_CASES=20` for the Go integration and Rust conformance runs, versus the default 40; this reduction is part of the evidence scope.

```sh
ATOMICFGA_REQUIRE_DATABASES=1 go test -race -count=1 . ./internal/...
ATOMICFGA_REQUIRE_DATABASES=1 ATOMICFGA_HEGEL_CASES=20 \
  go test -count=1 -timeout 30m ./tests/
go vet ./...
(cd rust && cargo test --release -p atomicfga-check --lib -p atomicfga-redb)
make test-spec
make test-web
make rust
ATOMICFGA_REQUIRE_DATABASES=1 ATOMICFGA_HEGEL_CASES=20 \
  go test -tags rustcheck -count=1 -timeout 30m \
  -run 'TestRustUpstream|TestHegelRustCheck|TestRustEdgeCases|TestHegelRustCompileModel|TestRustCompileEdgeCases|TestHegelWeightedGraphMatchesOpenFGA|TestHegelRustRedb' ./tests/
```

Observed results:

- Go race-enabled unit/datastore tests passed (`atomicfga`: 195.347 s; bind: 1.667 s). PostgreSQL absence was configured to fail, rather than silently skip.
- Go integration/upstream/Hegel package passed in 110.797 s across SQLite/sql, Turso/sql, PostgreSQL/sql, and PostgreSQL/pgx. The deliberate outside-transaction demonstration remains opt-in and was not enabled.
- Go vet reported no issues.
- Rust library suites: 10 evaluator/compiler/CEL tests and 1 redb test passed.
- Quint sampled invariants found no violation with 200 samples/30 steps; replay used 200 traces/40 steps and passed. Logged seeds were `0x363045eefc2ff7e1` for the invariant run and `0x8cb4e704` for replay.
- Browser suite passed in Chromium, Firefox, and WebKit: each adapter matched 263 workload outcomes and 80 final-state outcomes; OPFS stores matched another 80 outcomes after browser restart. These are adapter-equivalence checks using the same evaluator.
- Native Rust/cgo conformance package passed in 27.796 s. Its refusal accounting does not require every check/model to be decided.

An exploratory native `cargo test --release --workspace` failed linking browser evaluator imports such as `tuple_get` and `model_get`. The documented native subset and WASM/browser commands above passed. This is a test-entry-point limitation, not evidence of a WASM runtime failure.

The full SQL benchmark matrix, release/load soak tests, production deployment, crash/power-loss injection, exhaustive model checking, and unrestricted fuzzing were not run. Runtime complexity conclusions in [architecture](architecture.md) are distinguished from measurements.

## Isolated regression probes

The `.go.txt` files are not normal Go packages, `rust.rs` is outside the Cargo workspace, and `web.mjs` is outside the existing web test runner. They do not turn the project's normal gates red. The scripts create temporary standalone modules, refer back to this checkout, and remove their temporary directories afterward. PostgreSQL probes use the repository's existing test-database helper and its normal cleanup.

Run from the repository root:

```sh
# F01–F04 and F13; expected nonzero exit until repaired.
ATOMICFGA_REQUIRE_DATABASES=1 sh reviews/reproductions/run-go.sh

# F07; requires the native static library from make rust.
sh reviews/reproductions/run-go.sh -tags rustcheck -run TestRustHonorsConfiguredDepth

# F05, F06, F11; temporary standalone Cargo test module.
sh reviews/reproductions/run-rust.sh

# F08–F10, F12; requires the WASM modules from make web.
node --test reviews/reproductions/web.mjs
```

For the Go probes, `ATOMICFGA_DIALECTS=sqlite` restricts F01 to SQLite, but the dedicated F04 test intentionally targets PostgreSQL regardless of that variable. To run without PostgreSQL, also select tests, for example:

```sh
ATOMICFGA_DIALECTS=sqlite sh reviews/reproductions/run-go.sh \
  -run 'TestModelSavepointRollback|TestModelOutsideSnapshot|TestTursoAbortIsLatched|TestSQLiteRollback'
```

The native Rust probe runner uses the checked-in Rust lockfile as its dependency starting point. Cargo may add the temporary harness package to that temporary lockfile; no repository manifest or lockfile is modified.

## Observed counterexamples

| Probe | Observed result |
|---|---|
| ModelSavepointRollback | Explicit check of removed model returned no error on all four variants, after model enumeration proved the row absent. |
| ModelOutsideSnapshot | Raw SQL in the old WAL snapshot saw zero rows for the new model; Session check still returned no error. |
| TursoAbortIsLatched | Translated error matched the abort sentinel, but `Aborted()` was nil; a second backend call succeeded. |
| RepeatableReadDoesNotSerializeCheckAndAct | Both incompatible observations committed on PostgreSQL/sql and pgx at Repeatable Read. Both Serializable controls rejected the edit with `40001`. |
| SQLiteRollbackTriggerMustPoisonTransaction | Second CreateStore succeeded after trigger rollback; another connection saw 1 new store before caller commit. |
| RustHonorsConfiguredDepth | Go returned error 2002 at configured depth 1; RustCheck granted with no error. |
| same_id_in_two_stores / model_overwrite | A warm evaluator granted while a fresh evaluator denied under the stored restrictive model. |
| corrupt_condition | Valid false condition denied; malformed nonempty bytes yielded `allowed:true`. |
| canonical long chain | Returned too-complex for a finite derivation through 26 parent edges. |
| browser storage failure | Returned `internal_error` but retained the first tuple after committing the transaction scope. |
| browser async callback | Event order was `begin, commit, write`. |
| nested redb begin | Actual redb WASM returned an `unreachable` trap, not the intended already-open error. |
| memory read transaction | `tuplePut` succeeded where a write-capability error was required. |

The initial Go probes and the later SQLite rollback probe were run separately while being developed, then consolidated for the final recorded checks. The configuration probe was refined from one hop (which upstream could optimize) to two hops, yielding the retained counterexample. Probe assertions express the intended invariant and deliberately fail on these observations; a failure alone is not treated as evidence unless setup and control assertions also passed.
