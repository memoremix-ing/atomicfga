#!/bin/sh
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/atomicfga-review-go.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM
cp "$repo/reviews/reproductions/go_test.go.txt" "$scratch/review_test.go"
cp "$repo/reviews/reproductions/go_rust_test.go.txt" "$scratch/review_rust_test.go"
cat > "$scratch/go.mod" <<EOF
module github.com/memoremix-ing/atomicfga/review-probes
go 1.26.1
require github.com/memoremix-ing/atomicfga v0.0.0
replace github.com/memoremix-ing/atomicfga => $repo
replace github.com/openfga/openfga => $repo/../../../openfga/openfga.git/lts-1
EOF
cd "$scratch"
go test -mod=mod -count=1 -v "$@" .
