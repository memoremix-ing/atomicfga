# atomicfga foundation review — 2026-10-22

Reviewed source: `7c1c0043263ec70c6f84bffbe12029b634ff40a9` (`sts-0`). The workspace was clean when review began. The upstream OpenFGA workspace was also clean, with parent `22c4d5ddf4d5969bee10c24e468a5acfbcf26837`, matching `upstream.json`. All tracked additions from this review are confined to `reviews/`; existing implementation, tests, manifests, and documentation were left unchanged.

**Assessment:** the Go transaction adapter has a sound overall shape and unusually useful differential testing. However, several reproducible violations of its transaction contract need fixing before treating the foundation as trustworthy. The Rust/browser prototype has additional cases that return grants using stale models or malformed condition data. Its current conformance results do not cover those boundaries.

Read [correctness findings](correctness.md) for triggers, source locations, impact, proposed remedies, and regression tests. Read [architecture, performance, and verification](architecture.md) for the broader assessment and documentation backlog. [Validation](validation.md) records exactly what ran and what the results establish.

## Priority order

Severity reflects impact within the affected API. P1 means resolve before relying on that guarantee in applications; P2 means a material correctness, availability, or integration problem. Prototype findings apply to the Rust/browser path unless stated otherwise.

| ID | Priority | Confirmed finding |
|---|---|---|
| F13 | P1 | After a SQLite trigger rolls back the transaction, the same Session can write in autocommit mode before the caller commits. |
| F01 | P1 | Application savepoint rollback leaves a removed model resolvable from Go caches; reproduced on all four database variants. |
| F02 | P1 | A globally committed model bypasses transaction snapshot visibility; reproduced with a SQLite WAL reader. |
| F03 | P1 | Turso busy translation creates an abort error without latching aborted session state; subsequent statements reach the backend. |
| F04 | P1 | README recommends PostgreSQL Repeatable Read too broadly for check-and-act serialization; a deterministic counterexample commits on both PostgreSQL adapters. |
| F05 | P1 | Rust model cache identity disagrees with storage identity and accepted model replacement; warm and cold evaluators give different grants. |
| F06 | P1 | Malformed nonempty stored condition bytes can become an unconditional grant. |
| F07 | P2 | `RustCheck` bypasses a configured resolution depth limit and grants where Go returns too-complex. |
| F08 | P2 | A browser storage failure becomes a JSON error, so the transaction helper can commit a partial write. |
| F09 | P2 | An async browser transaction callback commits before its continuation executes. |
| F10 | P2 | A nested browser redb write transaction traps before the intended already-open check. |
| F11 | P2 | Canonical mode retains a depth cap despite the documented unbounded derivability semantics. |
| F12 | P2 | The JS reference store permits writes in read transactions and cannot undo them on abort. |

Every row has an executable probe in [reproductions/](reproductions/). These are intentionally failing regression candidates, isolated so normal `go test ./...` and Cargo test discovery remain unaffected. The PostgreSQL Serializable controls pass.

## Strengths worth preserving

- The Go path reuses upstream validation and resolution instead of maintaining a second authorization engine. The PostgreSQL port is localized, follows upstream's method order, and reuses shared write machinery.
- The stateless loopback driver makes a missing transaction context an explicit error. A datastore cannot silently fall back to a real connection pool.
- Savepoint lifetime exclusivity, buffered results, cancellation of queued work, and the call completion barrier address actual single-connection hazards. Shared transaction identity avoids a common wrapper-induced race.
- The Rust outcome sets explicitly represent uncertainty and refuse ambiguous results. This is a strong basis for incremental conformance, provided input integrity and model identity are equally strict.
- The Hegel differentials, upstream suites, Quint trace replay, mutation-test discussion, and browser restart tests are valuable evidence. The failures here suggest extending those tools at their boundaries, rather than replacing them.

## Recommended next milestones

1. Restore the Go visibility and abort invariants (F01–F03, F13), correct the isolation contract (F04), and make the added histories permanent regression cases.
2. Establish mechanically enforced model identity and condition decoding for Rust (F05–F06), then explicit evaluator configuration (F07).
3. Give browser transactions one enforceable success/failure and lifecycle contract (F08–F10, F12).
4. Define canonical semantics precisely, add non-vacuous conformance gates, and measure scaling by model count, relation fan-out, concurrency, and memory—not just total tuple count.

This is a source and behavioral review, not a proof of authorization correctness or storage crash safety. Passing suites and confirmed defects are reported separately; absence of a finding in a subsystem is not a certification.
