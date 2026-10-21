// Package bench compares atomicfga with the original OpenFGA.
//
// Both implementations run the upstream OpenFGA server code against the same
// dialect, each on a database of its own holding the same dataset. The
// original uses upstream's pooled datastore and its own transactions;
// atomicfga runs inside application transactions. Every benchmark first
// checks that both return the same result.
package bench

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"

	"google.golang.org/protobuf/proto"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/postgres"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	"github.com/openfga/openfga/pkg/storage/sqlite"
	"github.com/openfga/openfga/pkg/tuple"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// poolSize bounds the connections of both implementations.
const poolSize = 16

// api is the method set shared by *server.Server and *atomicfga.Session.
type api interface {
	Check(context.Context, *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error)
	BatchCheck(context.Context, *openfgav1.BatchCheckRequest) (*openfgav1.BatchCheckResponse, error)
	ListObjects(context.Context, *openfgav1.ListObjectsRequest) (*openfgav1.ListObjectsResponse, error)
	ListUsers(context.Context, *openfgav1.ListUsersRequest) (*openfgav1.ListUsersResponse, error)
	Write(context.Context, *openfgav1.WriteRequest) (*openfgav1.WriteResponse, error)
	WriteAuthorizationModel(context.Context, *openfgav1.WriteAuthorizationModelRequest) (*openfgav1.WriteAuthorizationModelResponse, error)
	CreateStore(context.Context, *openfgav1.CreateStoreRequest) (*openfgav1.CreateStoreResponse, error)
}

func invoke(ctx context.Context, a api, req any) (string, error) {
	switch r := req.(type) {
	case *openfgav1.CheckRequest:
		resp, err := a.Check(ctx, r)
		return fmt.Sprint(resp.GetAllowed()), err
	case *openfgav1.BatchCheckRequest:
		resp, err := a.BatchCheck(ctx, r)
		var out []string
		for id, res := range resp.GetResult() {
			out = append(out, fmt.Sprintf("%s=%v/%v", id, res.GetAllowed(), res.GetError()))
		}
		slices.Sort(out)
		return fmt.Sprint(out), err
	case *openfgav1.ListObjectsRequest:
		resp, err := a.ListObjects(ctx, r)
		return fmt.Sprint(len(resp.GetObjects()), slices.Sorted(slices.Values(resp.GetObjects()))), err
	case *openfgav1.ListUsersRequest:
		resp, err := a.ListUsers(ctx, r)
		var out []string
		for _, u := range resp.GetUsers() {
			out = append(out, tuple.UserProtoToString(u))
		}
		slices.Sort(out)
		return fmt.Sprint(len(out), out), err
	case *openfgav1.WriteRequest:
		_, err := a.Write(ctx, r)
		return "", err
	}
	panic(fmt.Sprintf("unsupported request %T", req))
}

// env holds both implementations for one dialect.
type env struct {
	dialect atomicfga.Dialect

	original        *server.Server
	originalQueries *counting
	originalDB      *sql.DB // for the application's own rows
	oStore, oModel  string
	db              *testdb.DB
	engine          *atomicfga.Engine
	store, model    string
}

var (
	envMu sync.Mutex
	envs  = map[atomicfga.Dialect]*env{}
)

func dialects() []atomicfga.Dialect {
	if os.Getenv("ATOMICFGA_DIALECTS") != "" {
		return testdb.Dialects()
	}
	return []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Turso, atomicfga.Postgres}
}

func openUpstream(d atomicfga.Dialect, uri string) (storage.OpenFGADatastore, error) {
	cfg := sqlcommon.NewConfig(sqlcommon.WithMaxOpenConns(poolSize), sqlcommon.WithMaxIdleConns(poolSize))
	switch d {
	case atomicfga.Postgres:
		return postgres.New(uri, cfg)
	default:
		return sqlite.New(uri, cfg)
	}
}

func load(ctx context.Context, a api, store, model string) error {
	ts := dataset()
	for len(ts) > 0 {
		n := min(len(ts), 50) // within every engine's write limit (Turso: 64)
		if _, err := a.Write(ctx, &openfgav1.WriteRequest{StoreId: store, AuthorizationModelId: model, Writes: &openfgav1.WriteRequestWrites{TupleKeys: ts[:n]}}); err != nil {
			return err
		}
		ts = ts[n:]
	}
	return nil
}

func setupModel(ctx context.Context, a api) (string, string, error) {
	st, err := a.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "bench"})
	if err != nil {
		return "", "", err
	}
	m := parser.MustTransformDSLToProto(benchDSL)
	resp, err := a.WriteAuthorizationModel(ctx, &openfgav1.WriteAuthorizationModelRequest{StoreId: st.GetId(), SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions(), Conditions: m.GetConditions()})
	if err != nil {
		return "", "", err
	}
	return st.GetId(), resp.GetAuthorizationModelId(), nil
}

// getEnv builds (once per process) both implementations for d.
func getEnv(b *testing.B, d atomicfga.Dialect) *env { return getEnvT(b, d) }

func getEnvT(b testing.TB, d atomicfga.Dialect) *env {
	envMu.Lock()
	defer envMu.Unlock()
	if e, ok := envs[d]; ok {
		return e
	}
	ctx := context.Background()
	tb := &keepAlive{TB: b}

	// Upstream has no Turso datastore; its SQLite one is the baseline.
	od := d
	if d == atomicfga.Turso {
		od = atomicfga.SQLite
	}
	odb := testdb.Open(tb, od)
	ds, err := atomicfga.Construct(func() (storage.OpenFGADatastore, error) { return openUpstream(od, odb.URI) })
	if err != nil {
		b.Fatal(err)
	}
	queries := &counting{OpenFGADatastore: ds}
	original, err := server.NewServerWithOpts(server.WithDatastore(queries))
	if err != nil {
		b.Fatal(err)
	}
	e := &env{dialect: d, original: original, originalQueries: queries, originalDB: odb.SQL}
	if e.oStore, e.oModel, err = setupModel(ctx, original); err != nil {
		b.Fatal(err)
	}
	if err := load(ctx, original, e.oStore, e.oModel); err != nil {
		b.Fatal(err)
	}

	e.db = testdb.Open(tb, d)
	e.db.SQL.SetMaxOpenConns(poolSize)
	e.db.SQL.SetMaxIdleConns(poolSize)
	if e.engine, err = atomicfga.New(d); err != nil {
		b.Fatal(err)
	}
	tx, commit, _, err := e.db.BeginTx(ctx, false)
	if err != nil {
		b.Fatal(err)
	}
	s := e.engine.Session(tx)
	if e.store, e.model, err = setupModel(ctx, s); err != nil {
		b.Fatal(err)
	}
	if err := load(ctx, s, e.store, e.model); err != nil {
		b.Fatal(err)
	}
	if err := commit(); err != nil {
		b.Fatal(err)
	}
	for _, db := range []*sql.DB{odb.SQL, e.db.SQL} {
		if _, err := db.Exec("CREATE TABLE bench_app (id VARCHAR(64) PRIMARY KEY)"); err != nil {
			b.Fatal(err)
		}
	}
	envs[d] = e
	return e
}

// keepAlive keeps databases until the process exits, so that every
// benchmark function can share one loaded dataset per dialect.
type keepAlive struct{ testing.TB }

func (keepAlive) Cleanup(func()) {}

func (k keepAlive) TempDir() string {
	dir, err := os.MkdirTemp("", "atomicfga-bench-")
	if err != nil {
		k.Fatal(err)
	}
	return dir
}

// mode is a way of running an operation.
type mode struct {
	name string
	// pgx binds native pgx transactions (PostgreSQL only).
	pgx bool
	// atomic runs atomicfga; otherwise the original server is used.
	atomic bool
	// shared runs all iterations in one transaction instead of one each.
	shared bool
}

func modes(d atomicfga.Dialect) []mode {
	ms := []mode{
		{name: "openfga"},
		{name: "atomicfga-tx", atomic: true},
		{name: "atomicfga-in-tx", atomic: true, shared: true},
	}
	if d == atomicfga.Postgres {
		ms = append(ms, mode{name: "atomicfga-pgx-tx", atomic: true, pgx: true}, mode{name: "atomicfga-pgx-in-tx", atomic: true, pgx: true, shared: true})
	}
	return ms
}

// gate fails the benchmark unless both implementations agree on req.
func (e *env) gate(b *testing.B, o op, pgx bool) {
	ctx := context.Background()
	want, oerr := invoke(ctx, e.original, o.build(e.oStore, e.oModel))
	tx, done, err := e.db.BeginReader(ctx, pgx)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = done() }()
	got, err := invoke(ctx, e.engine.Session(tx), o.build(e.store, e.model))
	if err != nil || oerr != nil || got != want {
		b.Fatalf("%s: atomicfga %s %v, OpenFGA %s %v", o.name, got, err, want, oerr)
	}
}

// BenchmarkRead measures read operations one at a time.
func BenchmarkRead(b *testing.B) {
	for _, d := range dialects() {
		e := getEnv(b, d)
		for _, o := range reads {
			for _, m := range modes(d) {
				b.Run(fmt.Sprintf("%s/%s/%s", d, o.name, m.name), func(b *testing.B) {
					e.gate(b, o, m.pgx)
					e.runRead(b, o, m)
				})
			}
		}
	}
}

func (e *env) runRead(b *testing.B, o op, m mode) {
	ctx := context.Background()
	b.ReportAllocs()
	if !m.atomic {
		req := o.build(e.oStore, e.oModel)
		before := e.originalQueries.n.Load()
		b.ResetTimer()
		for b.Loop() {
			if _, err := invoke(ctx, e.original, req); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(e.originalQueries.n.Load()-before)/float64(b.N), "stmts/op")
		return
	}
	req := o.build(e.store, e.model)
	var statements, trips int64
	if m.shared {
		tx, done, err := e.db.BeginReader(ctx, m.pgx)
		if err != nil {
			b.Fatal(err)
		}
		defer func() { _ = done() }()
		s := e.engine.Session(tx)
		b.ResetTimer()
		for b.Loop() {
			if _, err := invoke(ctx, s, req); err != nil {
				b.Fatal(err)
			}
		}
		statements, trips = s.Statements(), s.RoundTrips()
	} else {
		b.ResetTimer()
		for b.Loop() {
			tx, done, err := e.db.BeginReader(ctx, m.pgx)
			if err != nil {
				b.Fatal(err)
			}
			s := e.engine.Session(tx)
			if _, err := invoke(ctx, s, req); err != nil {
				b.Fatal(err)
			}
			statements += s.Statements()
			trips += s.RoundTrips()
			if err := done(); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ReportMetric(float64(statements)/float64(b.N), "stmts/op")
	b.ReportMetric(float64(trips)/float64(b.N), "trips/op")
}

// BenchmarkReadParallel measures read throughput with concurrent callers.
// The original shares a pool of poolSize connections; atomicfga callers each
// hold an application transaction from a pool of the same size.
func BenchmarkReadParallel(b *testing.B) {
	for _, d := range dialects() {
		e := getEnv(b, d)
		for _, o := range reads {
			for _, m := range modes(d) {
				if m.shared {
					continue
				}
				b.Run(fmt.Sprintf("%s/%s/%s", d, o.name, m.name), func(b *testing.B) {
					e.gate(b, o, m.pgx)
					b.SetParallelism(max(1, poolSize/runtime.GOMAXPROCS(0)))
					b.ReportAllocs()
					ctx := context.Background()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							if !m.atomic {
								if _, err := invoke(ctx, e.original, o.build(e.oStore, e.oModel)); err != nil {
									b.Error(err)
									return
								}
								continue
							}
							tx, done, err := e.db.BeginReader(ctx, m.pgx)
							if err != nil {
								b.Error(err)
								return
							}
							_, err = invoke(ctx, e.engine.Session(tx), o.build(e.store, e.model))
							_ = done()
							if err != nil {
								b.Error(err)
								return
							}
						}
					})
				})
			}
		}
	}
}

// BenchmarkWrite measures a tuple write (alternately a write and a delete of
// the same tuple), and the same write together with an application row.
//
// The original commits each tuple write in a transaction of its own, so its
// "with-app-row" case needs a second, separate transaction for the
// application row and is not atomic. atomicfga commits both in one.
func BenchmarkWrite(b *testing.B) {
	for _, d := range dialects() {
		e := getEnv(b, d)
		for _, withRow := range []bool{false, true} {
			name := "write/tuple"
			if withRow {
				name = "write/tuple-with-app-row"
			}
			for _, m := range modes(d) {
				if m.shared {
					continue
				}
				b.Run(fmt.Sprintf("%s/%s/%s", d, name, m.name), func(b *testing.B) {
					e.runWrite(b, m, withRow)
				})
			}
		}
	}
}

func writeReq(store, model string, i int) *openfgav1.WriteRequest {
	t := tk("doc:written", "owner", "user:writer")
	req := &openfgav1.WriteRequest{StoreId: store, AuthorizationModelId: model}
	if i%2 == 0 {
		req.Writes = &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{t}}
	} else {
		req.Deletes = &openfgav1.WriteRequestDeletes{TupleKeys: []*openfgav1.TupleKeyWithoutCondition{tuple.TupleKeyToTupleKeyWithoutCondition(t)}}
	}
	return req
}

func appRow(i int) string {
	if i%2 == 0 {
		return "INSERT INTO bench_app (id) VALUES ('row')"
	}
	return "DELETE FROM bench_app WHERE id = 'row'"
}

func (e *env) runWrite(b *testing.B, m mode, withRow bool) {
	ctx := context.Background()
	b.ReportAllocs()
	var statements int64
	i := 0
	for b.Loop() {
		if !m.atomic {
			if _, err := e.original.Write(ctx, writeReq(e.oStore, e.oModel, i)); err != nil {
				b.Fatal(err)
			}
			if withRow {
				tx, err := e.originalDB.BeginTx(ctx, nil)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, appRow(i)); err != nil {
					b.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					b.Fatal(err)
				}
			}
			i++
			continue
		}
		tx, commit, rollback, err := e.db.BeginTx(ctx, m.pgx)
		if err != nil {
			b.Fatal(err)
		}
		s := e.engine.Session(tx)
		if withRow {
			if _, err := tx.Exec(ctx, appRow(i), nil); err != nil {
				_ = rollback()
				b.Fatal(err)
			}
		}
		if _, err := s.Write(ctx, writeReq(e.store, e.model, i)); err != nil {
			_ = rollback()
			b.Fatal(err)
		}
		statements += s.Statements()
		if err := commit(); err != nil {
			b.Fatal(err)
		}
		i++
	}
	if m.atomic {
		b.ReportMetric(float64(statements)/float64(b.N), "stmts/op")
	}
	// Leave the dataset as it was.
	if i%2 == 1 {
		req := writeReq(e.oStore, e.oModel, 1)
		if m.atomic {
			tx, commit, _, _ := e.db.BeginTx(ctx, m.pgx)
			_, _ = e.engine.Session(tx).Write(ctx, proto.Clone(writeReq(e.store, e.model, 1)).(*openfgav1.WriteRequest))
			_ = commit()
			if withRow {
				_, _ = e.db.SQL.Exec(appRow(1))
			}
		} else {
			_, _ = e.original.Write(ctx, req)
			if withRow {
				_, _ = e.originalDB.Exec(appRow(1))
			}
		}
	}
}
