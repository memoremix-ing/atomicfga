package atomicfga_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

const docModel = `model
  schema 1.1
type user
type group
  relations
    define member: [user, group#member]
type doc
  relations
    define owner: [user]
    define viewer: [user, user:*, group#member] or owner
`

const docModelV2 = docModel + `    define editor: [user] or owner
`

// each runs fn for every dialect and transaction kind.
func each(t *testing.T, fn func(t *testing.T, db *testdb.DB, pgx bool)) {
	for _, d := range testdb.Dialects() {
		for _, b := range testdb.Backends(d) {
			t.Run(fmt.Sprintf("%s/%s", d, b), func(t *testing.T) {
				t.Parallel()
				fn(t, testdb.Open(t, d), b.Pgx)
			})
		}
	}
}

type fixture struct {
	t      *testing.T
	db     *testdb.DB
	engine *atomicfga.Engine
	pgx    bool
	store  string
	model  string
}

func newFixture(t *testing.T, db *testdb.DB, pgx bool) *fixture {
	t.Helper()
	e, err := atomicfga.New(db.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	if _, err := db.SQL.Exec("CREATE TABLE app_doc (id VARCHAR(64) PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, engine: e, pgx: pgx}
	f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
		st, err := s.CreateStore(context.Background(), &openfgav1.CreateStoreRequest{Name: "fixture"})
		if err != nil {
			return err
		}
		f.store = st.GetId()
		f.model = f.writeModel(s, docModel)
		return nil
	})
	return f
}

// tx runs fn in a new transaction, committing if it returns nil.
func (f *fixture) tx(fn func(*atomicfga.Session, atomicfga.Tx) error) {
	f.t.Helper()
	tx, commit, rollback := f.db.Begin(f.t, f.pgx)
	if err := fn(f.engine.Session(tx), tx); err != nil {
		_ = rollback()
		f.t.Fatal(err)
	}
	if err := commit(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) begin() (*atomicfga.Session, atomicfga.Tx, func() error, func() error) {
	tx, commit, rollback := f.db.Begin(f.t, f.pgx)
	return f.engine.Session(tx), tx, commit, rollback
}

func (f *fixture) writeModel(s *atomicfga.Session, dsl string) string {
	f.t.Helper()
	m := parser.MustTransformDSLToProto(dsl)
	resp, err := s.WriteAuthorizationModel(context.Background(), &openfgav1.WriteAuthorizationModelRequest{
		StoreId: f.store, SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions(), Conditions: m.GetConditions(),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.GetAuthorizationModelId()
}

func tk(object, relation, user string) *openfgav1.TupleKey {
	return &openfgav1.TupleKey{Object: object, Relation: relation, User: user}
}

func (f *fixture) write(s *atomicfga.Session, writes ...*openfgav1.TupleKey) error {
	_, err := s.Write(context.Background(), &openfgav1.WriteRequest{
		StoreId: f.store, Writes: &openfgav1.WriteRequestWrites{TupleKeys: writes},
	})
	return err
}

func (f *fixture) check(s *atomicfga.Session, model, object, relation, user string) (bool, error) {
	r, err := s.Check(context.Background(), &openfgav1.CheckRequest{
		StoreId: f.store, AuthorizationModelId: model,
		TupleKey: &openfgav1.CheckRequestTupleKey{Object: object, Relation: relation, User: user},
	})
	return r.GetAllowed(), err
}

func (f *fixture) mustCheck(s *atomicfga.Session, model, object, relation, user string) bool {
	f.t.Helper()
	ok, err := f.check(s, model, object, relation, user)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *fixture) appExec(tx atomicfga.Tx, query string) {
	f.t.Helper()
	if _, err := tx.Exec(context.Background(), query, nil); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) appCount(tx atomicfga.Tx) int {
	f.t.Helper()
	res, err := tx.Query(context.Background(), "SELECT id FROM app_doc", nil)
	if err != nil {
		f.t.Fatal(err)
	}
	return len(res.Rows)
}

func TestTransactionsCommitAndRollBackTogether(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)

		s, tx, _, rollback := f.begin()
		f.appExec(tx, "INSERT INTO app_doc VALUES ('1')")
		if err := f.write(s, tk("doc:1", "owner", "user:anne")); err != nil {
			t.Fatal(err)
		}
		if !f.mustCheck(s, "", "doc:1", "viewer", "user:anne") {
			t.Fatal("uncommitted grant is not visible to its own transaction")
		}
		if err := rollback(); err != nil {
			t.Fatal(err)
		}

		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			if f.mustCheck(s, "", "doc:1", "viewer", "user:anne") || f.appCount(tx) != 0 {
				return errors.New("rolled back grant or row is visible")
			}
			f.appExec(tx, "INSERT INTO app_doc VALUES ('1')")
			return f.write(s, tk("doc:1", "owner", "user:anne"))
		})

		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			if !f.mustCheck(s, f.model, "doc:1", "viewer", "user:anne") || f.appCount(tx) != 1 {
				return errors.New("committed grant or row is missing")
			}
			return nil
		})
	})
}

func TestFailedWriteRollsBackToItsSavepoint(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			if err := f.write(s, tk("doc:1", "owner", "user:anne")); err != nil {
				return err
			}
			// The second tuple is new, the first already exists: the whole
			// write must fail and leave no trace of the new tuple.
			err := f.write(s, tk("doc:2", "owner", "user:bob"), tk("doc:1", "owner", "user:anne"))
			if status.Code(err) != codes.Code(openfgav1.ErrorCode_write_failed_due_to_invalid_input) {
				return fmt.Errorf("duplicate write: got %v", err)
			}
			if f.mustCheck(s, "", "doc:2", "owner", "user:bob") {
				return errors.New("partially applied write is visible")
			}
			// The transaction is still usable, including for application SQL.
			f.appExec(tx, "INSERT INTO app_doc VALUES ('after-error')")
			return f.write(s, tk("doc:3", "owner", "user:carl"))
		})
		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			if !f.mustCheck(s, "", "doc:1", "owner", "user:anne") || !f.mustCheck(s, "", "doc:3", "owner", "user:carl") || f.appCount(tx) != 1 {
				return errors.New("work around the failed write was not committed")
			}
			return nil
		})
	})
}

func TestApplicationSavepointsNestAroundOperations(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			f.appExec(tx, "SAVEPOINT app")
			if err := f.write(s, tk("doc:1", "owner", "user:anne")); err != nil {
				return err
			}
			f.appExec(tx, "ROLLBACK TO SAVEPOINT app")
			if f.mustCheck(s, "", "doc:1", "owner", "user:anne") {
				return errors.New("write survived the application's savepoint rollback")
			}
			return f.write(s, tk("doc:1", "owner", "user:bob"))
		})
	})
}

func TestUncommittedModelsAndTuplesStayIsolated(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		if db.Dialect != atomicfga.Postgres {
			t.Skip("one writer at a time; isolation is covered by Hegel's concurrent reader")
		}
		f := newFixture(t, db, pgx)
		writer, _, _, rollback := f.begin()
		v2 := f.writeModel(writer, docModelV2)
		if err := f.write(writer, tk("doc:1", "owner", "user:anne")); err != nil {
			t.Fatal(err)
		}
		// Populate every process-wide model cache with the uncommitted model.
		if !f.mustCheck(writer, "", "doc:1", "editor", "user:anne") || !f.mustCheck(writer, v2, "doc:1", "editor", "user:anne") {
			t.Fatal("writer cannot use its own model")
		}

		reader, _, _, readerDone := f.begin()
		if _, err := f.check(reader, v2, "doc:1", "editor", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_authorization_model_not_found) {
			t.Fatalf("other transaction resolved an uncommitted model: %v", err)
		}
		if _, err := f.check(reader, "", "doc:1", "editor", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_validation_error) {
			t.Fatalf("latest model for other transaction should be v1 (no editor relation): %v", err)
		}
		if f.mustCheck(reader, "", "doc:1", "owner", "user:anne") {
			t.Fatal("other transaction sees an uncommitted tuple")
		}
		_ = readerDone()

		if err := rollback(); err != nil {
			t.Fatal(err)
		}
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			if _, err := f.check(s, v2, "doc:1", "owner", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_authorization_model_not_found) {
				return fmt.Errorf("rolled back model still resolves: %v", err)
			}
			return nil
		})
	})
}

func TestRolledBackModelIsNotServedFromCache(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		s, _, _, rollback := f.begin()
		v2 := f.writeModel(s, docModelV2)
		f.mustCheck(s, v2, "doc:1", "editor", "user:anne")
		if err := rollback(); err != nil {
			t.Fatal(err)
		}
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			if _, err := f.check(s, v2, "doc:1", "editor", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_authorization_model_not_found) {
				return fmt.Errorf("rolled back model resolved: %v", err)
			}
			if _, err := s.ReadAuthorizationModel(context.Background(), &openfgav1.ReadAuthorizationModelRequest{StoreId: f.store, Id: v2}); status.Code(err) != codes.Code(openfgav1.ErrorCode_authorization_model_not_found) {
				return fmt.Errorf("rolled back model readable: %v", err)
			}
			return nil
		})
	})
}

// modelAbsent requires every way of naming model in s to report it missing,
// and the latest model to be the fixture's.
func (f *fixture) modelAbsent(s *atomicfga.Session, model string) error {
	notFound := codes.Code(openfgav1.ErrorCode_authorization_model_not_found)
	if _, err := f.check(s, model, "doc:1", "editor", "user:anne"); status.Code(err) != notFound {
		return fmt.Errorf("check resolved the model: %v", err)
	}
	if _, err := s.ReadAuthorizationModel(context.Background(), &openfgav1.ReadAuthorizationModelRequest{StoreId: f.store, Id: model}); status.Code(err) != notFound {
		return fmt.Errorf("model readable: %v", err)
	}
	// The fixture's model has no editor relation.
	if _, err := f.check(s, "", "doc:1", "editor", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_validation_error) {
		return fmt.Errorf("latest model is not the fixture's: %v", err)
	}
	return nil
}

func TestModelRolledBackToASavepointIsNotServed(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, tx atomicfga.Tx) error {
			f.appExec(tx, "SAVEPOINT app")
			v2 := f.writeModel(s, docModelV2)
			// Resolve it every way, filling the process-wide model caches.
			f.mustCheck(s, v2, "doc:1", "editor", "user:anne")
			f.mustCheck(s, "", "doc:1", "editor", "user:anne")
			if _, err := s.ReadAuthorizationModel(context.Background(), &openfgav1.ReadAuthorizationModelRequest{StoreId: f.store, Id: v2}); err != nil {
				return err
			}
			f.appExec(tx, "ROLLBACK TO SAVEPOINT app")
			if err := f.modelAbsent(s, v2); err != nil {
				return err
			}
			return f.modelAbsent(f.engine.Session(tx), v2)
		})
	})
}

func TestModelCommittedAfterTheSnapshotIsNotServed(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		if db.Dialect == atomicfga.Postgres {
			db.Isolation = sql.LevelRepeatableRead // a snapshot per transaction
		}
		f := newFixture(t, db, pgx)
		var old atomicfga.Tx
		if db.Dialect == atomicfga.Postgres {
			_, tx, _, rollback := f.begin()
			defer rollback()
			old = tx
		} else {
			tx, err := db.Reader.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			old = atomicfga.SQLTx(tx)
		}
		f.appCount(old) // takes the snapshot

		var v2 string
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			v2 = f.writeModel(s, docModelV2)
			return nil
		})
		// A later transaction sees and uses it, filling the caches.
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			_, err := f.check(s, v2, "doc:1", "editor", "user:anne")
			return err
		})
		if err := f.modelAbsent(f.engine.Session(old), v2); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConcurrentCallsShareOneTransaction(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			var writes []*openfgav1.TupleKey
			for i := range 20 {
				writes = append(writes, tk("group:g", "member", fmt.Sprintf("user:%d", i)))
				writes = append(writes, tk(fmt.Sprintf("doc:%d", i), "viewer", "group:g#member"))
			}
			if err := f.write(s, writes...); err != nil {
				return err
			}
			var wg sync.WaitGroup
			errs := make(chan error, 400)
			for i := range 20 {
				for j := range 20 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						ok, err := f.check(s, "", fmt.Sprintf("doc:%d", i), "viewer", fmt.Sprintf("user:%d", j))
						if err != nil || !ok {
							errs <- fmt.Errorf("doc:%d user:%d: %v %v", i, j, ok, err)
						}
					}()
				}
			}
			wg.Wait()
			close(errs)
			return <-errs
		})
	})
}

func TestCanceledCallsLeaveTheTransactionUsable(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			if err := f.write(s, tk("doc:1", "owner", "user:anne")); err != nil {
				return err
			}
			for i := range 50 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i)*20*time.Microsecond)
				_, _ = s.ListObjects(ctx, &openfgav1.ListObjectsRequest{StoreId: f.store, Type: "doc", Relation: "viewer", User: "user:anne"})
				cancel()
			}
			if !f.mustCheck(s, "", "doc:1", "viewer", "user:anne") {
				return errors.New("grant lost")
			}
			return f.write(s, tk("doc:2", "owner", "user:anne"))
		})
	})
}
