package atomicfga_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// The application may wrap one transaction in a session as often as it
// likes. If each wrapper had its own state, a model written through one and
// then seen through another would be taken for a committed model, and kept
// resolvable from cache after a rollback.
func TestSessionsOfOneTransactionShareState(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		writer, tx, _, rollback := f.begin()
		v2 := f.writeModel(writer, docModelV2)
		other := f.engine.Session(tx) // e.g. a helper wrapping the same transaction
		if _, err := f.check(other, v2, "doc:1", "editor", "user:anne"); err != nil {
			t.Fatalf("second session cannot use the transaction's model: %v", err)
		}
		if err := rollback(); err != nil {
			t.Fatal(err)
		}
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			if _, err := f.check(s, v2, "doc:1", "editor", "user:anne"); status.Code(err) != codes.Code(openfgav1.ErrorCode_authorization_model_not_found) {
				return fmt.Errorf("rolled back model resolved: %v", err)
			}
			return nil
		})
	})
}

func TestSessionsOfOneTransactionRunConcurrently(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(_ *atomicfga.Session, tx atomicfga.Tx) error {
			var wg sync.WaitGroup
			errs := make(chan error, 40)
			for i := range 40 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s := f.engine.Session(tx) // a new wrapper per goroutine
					if i%4 == 0 {
						errs <- f.write(s, tk(fmt.Sprintf("doc:%d", i), "owner", "user:anne"))
						return
					}
					_, err := f.check(s, f.model, "doc:0", "viewer", "user:anne")
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					return err
				}
			}
			if !f.mustCheck(f.engine.Session(tx), f.model, "doc:36", "viewer", "user:anne") {
				return errors.New("a concurrent write was lost")
			}
			return nil
		})
	})
}

func TestTransactionStateIsReleased(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		for range 20 {
			f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
				_, err := f.check(s, f.model, "doc:1", "viewer", "user:anne")
				return err
			})
		}
		deadline := time.Now().Add(10 * time.Second)
		for f.engine.RegisteredTransactions() > 1 {
			if time.Now().After(deadline) {
				t.Fatalf("%d finished transactions still registered", f.engine.RegisteredTransactions())
			}
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
		}
	})
}

func TestConnectionsCannotBeBound(t *testing.T) {
	db := testdb.Open(t, atomicfga.SQLite)
	conn, err := db.SQL.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for name, fn := range map[string]func(){
		"*sql.DB":   func() { atomicfga.SQLTx(db.SQL) },
		"*sql.Conn": func() { atomicfga.SQLTx(conn) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SQLTx(%s) did not panic", name)
				}
			}()
			fn()
		}()
	}
}

// TestBeginImmediateWhileLockedAborts requires a writer that cannot take
// the write lock in time to get an error to retry the transaction on, like
// any other abort.
func TestBeginImmediateWhileLockedAborts(t *testing.T) {
	for _, d := range []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Turso} {
		t.Run(string(d), func(t *testing.T) {
			db := testdb.Open(t, d)
			ctx := context.Background()
			holder, err := atomicfga.BeginImmediate(ctx, db.SQL)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Rollback()

			// A second handle that waits only briefly for the lock.
			driver := map[atomicfga.Dialect]string{atomicfga.SQLite: "sqlite", atomicfga.Turso: "turso"}[d]
			impatient, err := sql.Open(driver, db.URI)
			if err != nil {
				t.Fatal(err)
			}
			defer impatient.Close()
			impatient.SetMaxOpenConns(1)
			if _, err := impatient.ExecContext(ctx, "PRAGMA busy_timeout = 10"); err != nil {
				t.Fatal(err)
			}
			_, err = atomicfga.BeginImmediate(ctx, impatient)
			if !errors.Is(err, atomicfga.ErrTransactionAborted) {
				t.Fatalf("BEGIN IMMEDIATE while locked: %v", err)
			}
		})
	}
}

// TestTransactionsEndedByTheDatabaseRefuseFurtherStatements requires a
// session to stop once SQLite has rolled the transaction back by itself
// (here, an application trigger's RAISE(ROLLBACK)): its connection would run
// later statements in autocommit mode, committing each at once.
func TestTransactionsEndedByTheDatabaseRefuseFurtherStatements(t *testing.T) {
	for _, d := range []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Turso} {
		for _, begin := range []string{"sql.Tx", "ImmediateTx"} {
			t.Run(string(d)+"/"+begin, func(t *testing.T) {
				db := testdb.Open(t, d)
				ctx := context.Background()
				if _, err := db.SQL.ExecContext(ctx, "CREATE TRIGGER app_rollback BEFORE INSERT ON store WHEN NEW.name = 'rollback' BEGIN SELECT RAISE(ROLLBACK, 'refused by the application'); END"); err != nil {
					t.Skipf("trigger: %v", err)
				}
				e, err := atomicfga.New(d)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Close()
				var tx atomicfga.Tx
				if begin == "sql.Tx" {
					stx, err := db.SQL.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer stx.Rollback()
					tx = atomicfga.SQLTx(stx)
				} else {
					itx, err := atomicfga.BeginImmediate(ctx, db.SQL)
					if err != nil {
						t.Fatal(err)
					}
					defer itx.Rollback()
					tx = atomicfga.SQLTx(itx)
				}
				s := e.Session(tx)
				if _, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "before"}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "rollback"}); !errors.Is(err, atomicfga.ErrTransactionAborted) || status.Code(err) != codes.Aborted {
					t.Fatalf("store the trigger refused: %v", err)
				}
				if _, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "after"}); !errors.Is(err, atomicfga.ErrTransactionAborted) {
					t.Fatalf("store after the transaction ended: %v", err)
				}
				var n int
				if err := db.Reader.QueryRowContext(ctx, "SELECT count(*) FROM store").Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != 0 {
					t.Fatalf("%d stores visible outside the transaction before it committed", n)
				}
			})
		}
	}
}

func TestBeginImmediate(t *testing.T) {
	for _, d := range []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Turso} {
		t.Run(string(d), func(t *testing.T) {
			db := testdb.Open(t, d)
			e, err := atomicfga.New(d)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			ctx := context.Background()

			tx, err := atomicfga.BeginImmediate(ctx, db.SQL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Session(atomicfga.SQLTx(tx)).CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "immediate"}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("rollback after commit: %v", err)
			}
			if _, err := tx.ExecContext(ctx, "SELECT 1"); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("exec after commit: %v", err)
			}

			tx, err = atomicfga.BeginImmediate(ctx, db.SQL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Session(atomicfga.SQLTx(tx)).CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "discarded"}); err != nil {
				t.Fatal(err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}

			read, err := db.Reader.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer read.Rollback()
			stores, err := e.Session(atomicfga.SQLTx(read)).ListStores(ctx, &openfgav1.ListStoresRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if len(stores.GetStores()) != 1 || stores.GetStores()[0].GetName() != "immediate" {
				t.Fatalf("stores: %v", stores.GetStores())
			}
		})
	}
}

// valueTx is a custom Tx without identity.
type valueTx struct{ atomicfga.Tx }

func TestUnidentifiableTransactionsAreRefused(t *testing.T) {
	e, err := atomicfga.New(atomicfga.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	defer func() {
		if recover() == nil {
			t.Fatal("Session accepted a transaction it cannot identify")
		}
	}()
	e.Session(valueTx{})
}
