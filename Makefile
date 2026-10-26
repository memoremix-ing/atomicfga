.PHONY: databases databases-down test test-unit test-upstream test-hegel bench vet rust test-rust test-spec bench-rust web test-web wasm-size

GO ?= GOTOOLCHAIN=local go

databases:
	./scripts/databases.sh up

databases-down:
	./scripts/databases.sh down

vet:
	$(GO) vet ./...

# Transaction semantics, upstream datastore conformance and the loopback driver.
test-unit:
	$(GO) test -race -count=1 . ./internal/...

# OpenFGA's own Check/ListObjects/ListUsers assertion suites and matrices.
test-upstream:
	$(GO) test -count=1 -timeout 90m -run 'TestUpstream' ./tests/

# Hegel model-based tests against the original OpenFGA (ATOMICFGA_HEGEL_CASES).
test-hegel:
	$(GO) test -count=1 -timeout 180m -run 'TestHegel' ./tests/

test: vet test-unit test-upstream test-hegel

# Repeated processes rather than -count: with sub-benchmarks, -count also
# repeats every top-level benchmark.
bench:
	mkdir -p dist && rm -f dist/bench.txt dist/bench-parallel.txt
	for i in 1 2 3; do $(GO) test -run '^$$' -bench 'BenchmarkRead$$|BenchmarkWrite' -benchtime 1s -timeout 120m ./bench/ >> dist/bench.txt; done
	for i in 1 2; do $(GO) test -run '^$$' -bench 'BenchmarkReadParallel' -benchtime 1s -timeout 120m ./bench/ >> dist/bench-parallel.txt; done
	python3 scripts/report.py dist/bench.txt
	python3 scripts/report.py dist/bench-parallel.txt

# The Rust port of Check (see docs/rustcheck.md), linked with -tags rustcheck.
rust:
	cd rust && cargo build --release -p atomicfga-ffi

# Conformance gates: Rust unit tests, OpenFGA's Check suites and matrices,
# the Hegel differentials and edge cases, comparing every Rust check with
# OpenFGA's Check in the same transaction, and every compiled model with
# OpenFGA's WriteAuthorizationModel.
test-rust: rust test-spec
	cd rust && cargo test --release -p atomicfga-check --lib -p atomicfga-redb
	$(GO) test -tags rustcheck -count=1 -timeout 120m -run 'TestRustUpstream|TestHegelRustCheck|TestRustEdgeCases|TestRustConfiguredLimits|TestHegelRustCompileModel|TestRustCompileEdgeCases|TestHegelWeightedGraphMatchesOpenFGA|TestHegelRustRedb' ./tests/

bench-rust: rust
	ATOMICFGA_WASMFGA=1 $(GO) test -tags rustcheck -count=1 -timeout 120m -run TestWasmfgaWorkloads ./bench/

# The browser modules: the storage-free evaluator and the redb store.
web:
	cd rust && cargo build --profile wasm --target wasm32-unknown-unknown -p atomicfga-web -p atomicfga-web-redb

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-audit --no-fund
	touch web/node_modules

# The JS glue's contracts in Node; then store adapters (redb in memory and
# on OPFS, SQLite on OPFS) against the in-memory reference, in Chromium,
# Firefox and WebKit, across a browser restart; then timings.
test-web: web web/node_modules
	node --test web/test/unit.mjs
	node web/test/run.mjs

wasm-size: web
	@for f in rust/target/wasm32-unknown-unknown/wasm/atomicfga_web.wasm rust/target/wasm32-unknown-unknown/wasm/atomicfga_web_redb.wasm; do echo "$$(basename $$f): $$(wc -c < $$f) bytes, $$(gzip -9 -c $$f | wc -c) gzipped"; done

# The canonical Check specification (spec/check.qnt, Quint pinned in
# spec/package.json): its invariants, then quint-connect replaying its traces
# against the evaluator in canonical semantics.
spec/node_modules: spec/package.json spec/package-lock.json
	cd spec && npm ci --no-audit --no-fund
	touch spec/node_modules

test-spec: spec/node_modules
	cd spec && ./node_modules/.bin/quint run check.qnt --max-samples 200 --max-steps 30 --invariants ClosureIsFixed BlockedNeverViews
	cd rust && PATH="$(CURDIR)/spec/node_modules/.bin:$$PATH" cargo test --release -p atomicfga-check --features spec --test quint -- --nocapture
