package bench

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// The wasmfga comparison reproduces the native benchmark of the sibling
// wasmfga project (benchmarks/NATIVE_RESULTS.md there): ten workloads,
// each checked repeatedly inside one open, uncommitted transaction that
// also wrote the workload's model and tuples, with an explicit model ID.
// Iteration counts, warmups and statistics follow wasmfga's harness.
//
// Run with ATOMICFGA_WASMFGA=1. Results go to dist/wasmfga-<variant>.json.

type wasmfgaTuple struct {
	Object    string          `json:"object"`
	Relation  string          `json:"relation"`
	User      string          `json:"user"`
	Condition json.RawMessage `json:"condition,omitempty"`
}

type wasmfgaCase struct {
	Name    string          `json:"name"`
	Tuples  []wasmfgaTuple  `json:"tuples"`
	Check   json.RawMessage `json:"check"`
	Allowed bool            `json:"allowed"`
}

type wasmfgaWorkload struct {
	Name         string `json:"name"`
	Case         string `json:"case"`
	Kind         string `json:"kind"`
	Depth        int    `json:"depth"`
	SnapshotSize int    `json:"snapshot_size"`
}

// wasmfgaWorkloads ports wasmfga's benchmarks/workloads.mjs, plus a
// 100,000-tuple store beyond wasmfga's 10,000-tuple snapshot limit.
func wasmfgaWorkloads(t testing.TB) (json.RawMessage, []wasmfgaCase) {
	b, err := os.ReadFile("testdata/wasmfga.json")
	if err != nil {
		t.Fatal(err)
	}
	var src struct {
		Model     json.RawMessage   `json:"model"`
		Cases     []wasmfgaCase     `json:"cases"`
		Workloads []wasmfgaWorkload `json:"workloads"`
	}
	if err := json.Unmarshal(b, &src); err != nil {
		t.Fatal(err)
	}
	src.Workloads = append(src.Workloads, wasmfgaWorkload{Name: "snapshot-100000", Case: "owner", SnapshotSize: 100000})
	seed := func(name string) wasmfgaCase {
		for _, c := range src.Cases {
			if c.Name == name {
				c.Tuples = slices.Clone(c.Tuples)
				return c
			}
		}
		t.Fatalf("no case %q", name)
		return wasmfgaCase{}
	}
	var out []wasmfgaCase
	for _, w := range src.Workloads {
		name := w.Case
		if name == "" {
			name = "owner"
		}
		c := seed(name)
		c.Name = w.Name
		switch w.Kind {
		case "userset_chain":
			c.Tuples = []wasmfgaTuple{{"document:1", "viewer", "group:0#member", nil}}
			for i := range w.Depth {
				user := fmt.Sprintf("group:%d#member", i+1)
				if i == w.Depth-1 {
					user = "user:alice"
				}
				c.Tuples = append(c.Tuples, wasmfgaTuple{fmt.Sprintf("group:%d", i), "member", user, nil})
			}
		case "parent_chain":
			c.Tuples = []wasmfgaTuple{{"document:1", "parent", "folder:0", nil}}
			for i := range w.Depth - 1 {
				c.Tuples = append(c.Tuples, wasmfgaTuple{fmt.Sprintf("folder:%d", i), "parent", fmt.Sprintf("folder:%d", i+1), nil})
			}
			c.Tuples = append(c.Tuples, wasmfgaTuple{fmt.Sprintf("folder:%d", w.Depth-1), "viewer", "user:alice", nil})
		}
		for i := len(c.Tuples); i < w.SnapshotSize; i++ {
			c.Tuples = append(c.Tuples, wasmfgaTuple{fmt.Sprintf("document:unrelated-%d", i), "owner", fmt.Sprintf("user:unrelated-%d", i), nil})
		}
		out = append(out, c)
	}
	verifyAgainstWasmfga(t, out)
	return src.Model, out
}

// verifyAgainstWasmfga checks the generated workloads against wasmfga's own
// generated fixtures, when the sibling workspace has built them.
func verifyAgainstWasmfga(t testing.TB, ours []wasmfgaCase) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "wasmfga.git", "sts-0", "dist", "native", "fixtures.json"))
	if err != nil {
		t.Logf("wasmfga fixtures not built; not cross-checked: %v", err)
		return
	}
	var theirs []wasmfgaCase
	if err := json.Unmarshal(b, &theirs); err != nil {
		t.Fatal(err)
	}
	norm := func(raw json.RawMessage) any {
		var v any
		_ = json.Unmarshal(raw, &v)
		return v
	}
	for _, w := range theirs {
		i := slices.IndexFunc(ours, func(c wasmfgaCase) bool { return c.Name == w.Name })
		if i < 0 {
			t.Fatalf("workload %s missing", w.Name)
		}
		o := ours[i]
		same := o.Allowed == w.Allowed && len(o.Tuples) == len(w.Tuples) && reflect.DeepEqual(norm(o.Check), norm(w.Check))
		for j := 0; same && j < len(o.Tuples); j++ {
			a, b := o.Tuples[j], w.Tuples[j]
			same = a.Object == b.Object && a.Relation == b.Relation && a.User == b.User && reflect.DeepEqual(norm(a.Condition), norm(b.Condition))
		}
		if !same {
			t.Fatalf("workload %s differs from wasmfga's fixture", w.Name)
		}
	}
	t.Logf("all %d workloads match wasmfga's fixtures", len(theirs))
}

// wasmfgaTarget opens an implementation for one workload: the API to load
// and check through, a statement counter, and a cleanup.
type wasmfgaTarget struct {
	name    string
	dialect atomicfga.Dialect
	open    func(t *testing.T) (api, func() int64, func())
}

// atomicTarget runs atomicfga in an open transaction.
func atomicTarget(name string, open func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func())) wasmfgaTarget {
	d := dialectOf(name)
	return wasmfgaTarget{name: name, dialect: d, open: func(t *testing.T) (api, func() int64, func()) {
		dialect, tx, done := open(t)
		e, err := atomicfga.New(dialect)
		if err != nil {
			t.Fatal(err)
		}
		s := e.Session(tx)
		return s, s.Statements, func() { done(); e.Close() }
	}}
}

// originalTarget runs the original OpenFGA server on upstream's pooled
// datastore; every write commits on its own.
func originalTarget(name string, d atomicfga.Dialect) wasmfgaTarget {
	return wasmfgaTarget{name: name, dialect: d, open: func(t *testing.T) (api, func() int64, func()) {
		db := testdb.Open(t, d)
		ds, err := atomicfga.Construct(func() (storage.OpenFGADatastore, error) { return openUpstream(d, db.URI) })
		if err != nil {
			t.Fatal(err)
		}
		queries := &counting{OpenFGADatastore: ds}
		srv, err := server.NewServerWithOpts(server.WithDatastore(queries))
		if err != nil {
			t.Fatal(err)
		}
		return srv, queries.n.Load, srv.Close
	}}
}

func wasmfgaTargets() []wasmfgaTarget {
	pg := func(native bool) func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func()) {
		return func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func()) {
			db := testdb.Open(t, atomicfga.Postgres)
			tx, _, rollback := db.Begin(t, native)
			return atomicfga.Postgres, tx, func() { _ = rollback() }
		}
	}
	return []wasmfgaTarget{
		originalTarget("openfga-sqlite", atomicfga.SQLite),
		atomicTarget("sqlite", func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func()) {
			// As in wasmfga's harness: one in-memory connection.
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			db.SetMaxOpenConns(1)
			if err := atomicfga.Migrate(context.Background(), db, atomicfga.SQLite); err != nil {
				t.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			return atomicfga.SQLite, atomicfga.SQLTx(tx), func() { _ = tx.Rollback(); _ = db.Close() }
		}),
		atomicTarget("turso", func(t *testing.T) (atomicfga.Dialect, atomicfga.Tx, func()) {
			db := testdb.Open(t, atomicfga.Turso) // migrated through its own handle
			tx, _, rollback := db.Begin(t, false)
			return atomicfga.Turso, tx, func() { _ = rollback() }
		}),
		originalTarget("openfga-postgres", atomicfga.Postgres),
		atomicTarget("postgres-sql", pg(false)),
		atomicTarget("postgres-pgx", pg(true)),
	}
}

func TestWasmfgaWorkloads(t *testing.T) {
	if os.Getenv("ATOMICFGA_WASMFGA") == "" {
		t.Skip("set ATOMICFGA_WASMFGA=1")
	}
	model, workloads := wasmfgaWorkloads(t)
	ctx := context.Background()
	for _, target := range wasmfgaTargets() {
		if !slices.Contains(testdb.Dialects(), target.dialect) {
			continue
		}
		t.Run(target.name, func(t *testing.T) {
			var results []map[string]any
			for _, w := range workloads {
				if only := os.Getenv("ATOMICFGA_WASMFGA_ONLY"); only != "" && only != w.Name {
					continue
				}
				s, statements, done := target.open(t)
				store, modelID := loadWasmfga(t, ctx, s, model, w)
				req := &openfgav1.CheckRequest{}
				if err := protojson.Unmarshal(w.Check, req); err != nil {
					t.Fatal(err)
				}
				req.StoreId, req.AuthorizationModelId = store, modelID
				run := func() {
					resp, err := s.Check(ctx, req)
					if err != nil || resp.GetAllowed() != w.Allowed {
						t.Fatalf("%s: %v %v, want %v", w.Name, resp.GetAllowed(), err, w.Allowed)
					}
				}
				iterations, warmup := 5000, 500
				switch {
				case len(w.Tuples) >= 10000:
					iterations, warmup = 50, 10
				case len(w.Tuples) >= 1000:
					iterations, warmup = 500, 100
				}
				for range warmup {
					run()
				}
				runtime.GC()
				before := statements()
				durations := make([]float64, iterations)
				for i := range iterations {
					start := time.Now()
					run()
					durations[i] = float64(time.Since(start).Nanoseconds()) / 1000
				}
				stmts := float64(statements()-before) / float64(iterations)
				slices.Sort(durations)
				median, p95 := durations[len(durations)/2], durations[min(len(durations)-1, len(durations)*95/100)]
				t.Logf("%-16s %7d tuples  median %9.1f µs  p95 %9.1f µs  %.1f statements", w.Name, len(w.Tuples), median, p95, stmts)
				results = append(results, map[string]any{"workload": w.Name, "tuples": len(w.Tuples), "iterations": iterations, "warmup": warmup, "medianUs": median, "p95Us": p95, "statements": stmts})
				done()
			}
			b, _ := json.MarshalIndent(map[string]any{"target": target.name, "goVersion": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0), "measurements": results}, "", " ")
			_ = os.MkdirAll("../dist", 0o755)
			if err := os.WriteFile(fmt.Sprintf("../dist/wasmfga-%s.json", target.name), b, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func dialectOf(target string) atomicfga.Dialect {
	switch target {
	case "sqlite", "openfga-sqlite":
		return atomicfga.SQLite
	case "turso":
		return atomicfga.Turso
	}
	return atomicfga.Postgres
}

// loadWasmfga writes the workload's store, model and tuples through the
// session, inside the open transaction, as wasmfga's harness does.
func loadWasmfga(t *testing.T, ctx context.Context, s api, model json.RawMessage, w wasmfgaCase) (string, string) {
	st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: w.Name})
	if err != nil {
		t.Fatal(err)
	}
	m := &openfgav1.WriteAuthorizationModelRequest{}
	if err := protojson.Unmarshal(model, m); err != nil {
		t.Fatal(err)
	}
	m.StoreId = st.GetId()
	resp, err := s.WriteAuthorizationModel(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	var keys []*openfgav1.TupleKey
	for _, tu := range w.Tuples {
		b, _ := json.Marshal(tu)
		k := &openfgav1.TupleKey{}
		if err := protojson.Unmarshal(b, k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	for len(keys) > 0 {
		n := min(len(keys), 50)
		if _, err := s.Write(ctx, &openfgav1.WriteRequest{StoreId: st.GetId(), AuthorizationModelId: resp.GetAuthorizationModelId(), Writes: &openfgav1.WriteRequestWrites{TupleKeys: keys[:n]}}); err != nil {
			t.Fatal(err)
		}
		keys = keys[n:]
	}
	return st.GetId(), resp.GetAuthorizationModelId()
}
