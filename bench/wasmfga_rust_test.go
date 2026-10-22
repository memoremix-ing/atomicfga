//go:build rustcheck

package bench

import (
	"context"
	"testing"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/memoremix-ing/atomicfga"
)

// rustSession answers Check with the Rust evaluator only (a refusal fails
// the workload).
type rustSession struct{ *atomicfga.Session }

func (s rustSession) Check(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	return s.RustCheckStrict(ctx, req)
}

func init() {
	rustTargets = func(sqliteTx txOpener, pg func(native bool) txOpener) []wasmfgaTarget {
		target := func(name string, open func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func())) wasmfgaTarget {
			t := atomicTarget(name, open)
			inner := t.open
			t.open = func(tb *testing.T) (api, func() int64, func()) {
				a, statements, done := inner(tb)
				return rustSession{a.(*atomicfga.Session)}, statements, done
			}
			return t
		}
		return []wasmfgaTarget{
			target("rust-sqlite", sqliteTx),
			target("rust-postgres-pgx", pg(true)),
		}
	}
}
