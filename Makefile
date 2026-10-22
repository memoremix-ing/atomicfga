.PHONY: databases databases-down test test-unit test-upstream test-hegel bench vet rust test-rust bench-rust wasm-size

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
	cd rust && cargo build --release

# Conformance gates: Rust unit tests, OpenFGA's Check suites and matrices,
# the Hegel differentials and edge cases, comparing every Rust check with
# OpenFGA's Check in the same transaction, and every compiled model with
# OpenFGA's WriteAuthorizationModel.
test-rust: rust
	cd rust && cargo test --release
	$(GO) test -tags rustcheck -count=1 -timeout 120m -run 'TestRustUpstream|TestHegelRustCheck|TestRustEdgeCases|TestHegelRustCompileModel|TestRustCompileEdgeCases|TestHegelWeightedGraphMatchesOpenFGA' ./tests/

bench-rust: rust
	ATOMICFGA_WASMFGA=1 $(GO) test -tags rustcheck -count=1 -timeout 120m -run TestWasmfgaWorkloads ./bench/

wasm-size:
	cd rust && cargo build --profile wasm --target wasm32-unknown-unknown
	@f=rust/target/wasm32-unknown-unknown/wasm/atomicfga_check.wasm; echo "$$(wc -c < $$f) bytes, $$(gzip -9 -c $$f | wc -c) gzipped"
