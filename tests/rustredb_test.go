//go:build rustcheck

package tests

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/memoremix-ing/atomicfga"
)

// redbGate tallies the redb store, served by the Rust port alone, against
// the original OpenFGA server.
type redbGate struct {
	mu       sync.Mutex
	ops      map[string]int // compared operations by kind and OpenFGA outcome
	refusals map[string]int
	wrong    []string
}

func (g *redbGate) compare(kind string, req fmt.Stringer, want, got string, gerr error) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if errors.Is(gerr, atomicfga.ErrRustRefused) {
		g.refusals[kind+": "+strings.TrimPrefix(gerr.Error(), atomicfga.ErrRustRefused.Error()+": ")]++
		return ""
	}
	g.ops[kind+" "+want]++
	if want != got {
		d := fmt.Sprintf("%s %v\n  openfga: %s\n  redb:    %s", kind, req, want, got)
		g.wrong = append(g.wrong, d)
		return d
	}
	return ""
}

func (g *redbGate) report(t *testing.T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, n := range g.ops {
		total += n
	}
	t.Logf("%d operations compared (%d wrong); by kind and OpenFGA outcome: %v", total, len(g.wrong), g.ops)
	for _, r := range slices.Sorted(maps.Keys(g.refusals)) {
		t.Logf("  refused %5d× %s", g.refusals[r], r)
	}
	for _, w := range g.wrong {
		t.Errorf("wrong: %s", w)
	}
}

func outcomeOf(err error) string {
	if err == nil {
		return "ok"
	}
	code, _ := checkOutcome(nil, err)
	return code
}

// drawWrite draws a WriteRequest: tuples valid for the model or not,
// deletes of tuples written before or not, duplicates, and the ignore
// options.
func drawWrite(tc hegel.TestCase, rm rustModel, written []*openfgav1.TupleKey, store string, models []string) *openfgav1.WriteRequest {
	req := &openfgav1.WriteRequest{StoreId: store}
	if len(models) > 0 && hegel.Draw(tc, hegel.Booleans()) {
		req.AuthorizationModelId = drawFrom(tc, models)
	}
	if n := hegel.Draw(tc, hegel.Integers(-1, 3)); n > 0 {
		req.Writes = &openfgav1.WriteRequestWrites{}
		for range n {
			req.Writes.TupleKeys = append(req.Writes.TupleKeys, drawRustTuple(tc, rm))
		}
		if rare(tc, 6) {
			req.Writes.OnDuplicate = "ignore"
		}
	}
	if n := hegel.Draw(tc, hegel.Integers(-2, 2)); n > 0 {
		req.Deletes = &openfgav1.WriteRequestDeletes{}
		for range n {
			k := drawRustTuple(tc, rm)
			if len(written) > 0 && hegel.Draw(tc, hegel.Booleans()) {
				k = drawFrom(tc, written)
			}
			req.Deletes.TupleKeys = append(req.Deletes.TupleKeys, &openfgav1.TupleKeyWithoutCondition{Object: k.GetObject(), Relation: k.GetRelation(), User: k.GetUser()})
		}
		if rare(tc, 6) {
			req.Deletes.OnMissing = "ignore"
		}
	}
	if req.Writes != nil && rare(tc, 8) {
		req.Writes.TupleKeys = append(req.Writes.TupleKeys, req.Writes.TupleKeys[0]) // a duplicate
	}
	return req
}

// TestHegelRustRedb compares a redb store served by the Rust port (model
// compiler, write validation, evaluator) with the original OpenFGA server,
// operation by operation: model writes, tuple writes and deletes, checks.
func TestHegelRustRedb(t *testing.T) {
	ctx := context.Background()
	oracle := newOracle(t, atomicfga.SQLite)
	gate := &redbGate{ops: map[string]int{}, refusals: map[string]int{}}
	t.Cleanup(func() { gate.report(t) })
	hegel.Test(t, func(ht *hegel.T) {
		st, err := oracle.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "redb"})
		if err != nil {
			ht.Fatalf("create store: %v", err)
		}
		store := st.GetId()
		r := atomicfga.NewRedbStore()
		defer r.Close()
		var models []string
		var drawn []rustModel
		for range hegel.Draw(ht, hegel.Integers(1, 2)) {
			rm := drawRustModel(ht)
			// Without the list-typed condition, whose CEL is outside the port's
			// subset: the store could not decide whether to accept the model.
			rm.dsl = strings.ReplaceAll(rm.dsl, "user with list_ok", "user with str_ok")
			rm.dsl = strings.Replace(rm.dsl, "condition list_ok(xs: list<string>, x: string) {\n  x in xs\n}\n", "", 1)
			for k, rs := range rm.restrictions {
				for j, r := range rs {
					if r == "user with list_ok" {
						rm.restrictions[k][j] = "user with str_ok"
					}
				}
			}
			m, err := parser.TransformDSLToProto(rm.dsl)
			if err != nil {
				ht.Fatalf("model: %v", err)
			}
			req := &openfgav1.WriteAuthorizationModelRequest{StoreId: store, SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions(), Conditions: m.GetConditions()}
			resp, werr := oracle.WriteAuthorizationModel(ctx, req)
			id := resp.GetAuthorizationModelId()
			if id == "" {
				id = "01ZZZZZZZZZZZZZZZZZZZZZZZZ" // not stored by OpenFGA either
			}
			gerr := r.WriteAuthorizationModel(store, id, req)
			if wrong := gate.compare("model", req, outcomeOf(werr), outcomeOf(gerr), gerr); wrong != "" {
				ht.Fatalf("%s", wrong)
			}
			if errors.Is(gerr, atomicfga.ErrRustRefused) {
				return // the stores would diverge from here on
			}
			if werr == nil {
				models = append(models, id)
				drawn = append(drawn, rm)
			}
		}
		if len(models) == 0 {
			return
		}
		var written []*openfgav1.TupleKey
		for range hegel.Draw(ht, hegel.Integers(1, 25)) {
			i := hegel.Draw(ht, hegel.Integers(0, len(models)-1))
			if hegel.Draw(ht, hegel.Integers(0, 2)) > 0 {
				req := drawWrite(ht, drawn[i], written, store, models)
				_, werr := oracle.Write(ctx, req)
				gerr := r.Write(store, req)
				if wrong := gate.compare("write", req, outcomeOf(werr), outcomeOf(gerr), gerr); wrong != "" {
					ht.Fatalf("%s", wrong)
				}
				if errors.Is(gerr, atomicfga.ErrRustRefused) {
					return // the stores may differ from here on
				}
				if werr == nil {
					written = append(written, req.GetWrites().GetTupleKeys()...)
				}
				continue
			}
			model := ""
			if hegel.Draw(ht, hegel.Booleans()) {
				model = models[i]
			}
			req := drawRustCheck(ht, drawn[i], store, model)
			want, werr := oracle.Check(ctx, req)
			got, gerr := r.Check(store, req)
			wc, _ := checkOutcome(want, werr)
			gc, _ := checkOutcome(got, gerr)
			if wrong := gate.compare("check", req, wc, gc, gerr); wrong != "" {
				ht.Fatalf("%s", wrong)
			}
		}
	}, hegel.WithTestCases(hegelCases()*5), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch))
}
