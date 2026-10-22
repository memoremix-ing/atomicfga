#!/bin/sh
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/atomicfga-review-rust.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
mkdir "$scratch/src"
cp "$repo/reviews/reproductions/rust.rs" "$scratch/src/lib.rs"
cp "$repo/rust/Cargo.lock" "$scratch/Cargo.lock"
cat > "$scratch/Cargo.toml" <<EOF
[package]
name = "atomicfga-review-probes"
version = "0.0.0"
edition = "2021"
[dependencies]
atomicfga-check = { path = "$repo/rust/check" }
serde_json = { version = "1.0", features = ["float_roundtrip", "raw_value"] }
EOF
cargo test --manifest-path "$scratch/Cargo.toml" "$@"
