package bind

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a Backend that records statements. Queries return one row holding
// the query text. Statements containing "fail:<sqlstate>" fail with that
// SQLSTATE, and "slow" ones take a while.
type fake struct {
	mu        sync.Mutex
	log       []string
	active    atomic.Int32
	maxActive atomic.Int32
	pipelines atomic.Int32
}

type stateErr string

func (e stateErr) Error() string    { return "sqlstate " + string(e) }
func (e stateErr) SQLState() string { return string(e) }

func (f *fake) run(query string) error {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for {
		m := f.maxActive.Load()
		if n <= m || f.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.log = append(f.log, query)
	f.mu.Unlock()
	if strings.Contains(query, "slow") {
		time.Sleep(20 * time.Millisecond)
	}
	if i := strings.Index(query, "fail:"); i >= 0 {
		return stateErr(query[i+5 : i+10])
	}
	return nil
}

func (f *fake) Query(_ context.Context, query string, _ []any) (*Result, error) {
	if err := f.run(query); err != nil {
		return nil, err
	}
	return &Result{Columns: []string{"q"}, Rows: [][]driver.Value{{query}}}, nil
}

func (f *fake) Exec(_ context.Context, query string, _ []any) (int64, error) {
	return 1, f.run(query)
}

func (f *fake) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

// pipelining is a fake that also pipelines.
type pipelining struct{ *fake }

func (p pipelining) Pipeline(ctx context.Context, stmts []Statement) ([]*Result, []error) {
	p.pipelines.Add(1)
	results := make([]*Result, len(stmts))
	errs := make([]error, len(stmts))
	for i, st := range stmts {
		results[i], errs[i] = p.fake.Query(ctx, st.Query, st.Args)
	}
	return results, errs
}

func query(ctx context.Context, db *sql.DB, q string) (string, error) {
	var got string
	err := db.QueryRowContext(ctx, q).Scan(&got)
	return got, err
}

func TestStatementsNeedASession(t *testing.T) {
	db := OpenDB()
	defer db.Close()
	if _, err := query(context.Background(), db, "SELECT 1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
	if _, err := db.BeginTx(context.Background(), nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v", err)
	}
}

func TestQueriesRunOnTheSessionBackend(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprint("pipeline=", pipeline), func(t *testing.T) {
			f := &fake{}
			var b Backend = f
			if pipeline {
				b = pipelining{f}
			}
			s := NewSession(b)
			db := OpenDB()
			defer db.Close()
			call := s.Call()
			ctx := WithCall(context.Background(), call)
			var wg sync.WaitGroup
			for i := range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					q := fmt.Sprintf("SELECT %d", i)
					if got, err := query(ctx, db, q); err != nil || got != q {
						t.Errorf("%s: %q %v", q, got, err)
					}
				}()
			}
			wg.Wait()
			call.Finish()
			if f.maxActive.Load() != 1 {
				t.Fatalf("statements overlapped on the backend: %d", f.maxActive.Load())
			}
			if s.Statements() != 50 || len(f.statements()) != 50 {
				t.Fatalf("statements: session %d, backend %d", s.Statements(), len(f.statements()))
			}
			if pipeline && s.RoundTrips() >= 50 {
				t.Fatalf("no query was pipelined: %d round trips", s.RoundTrips())
			}
		})
	}
}

func TestSavepointHoldsTheSessionUntilItEnds(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(fmt.Sprint("commit=", commit), func(t *testing.T) {
			f := &fake{}
			s := NewSession(pipelining{f})
			db := OpenDB()
			defer db.Close()
			call := s.Call()
			ctx := WithCall(context.Background(), call)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			outside := make(chan error)
			go func() {
				_, err := query(ctx, db, "SELECT outside")
				outside <- err
			}()
			if _, err := tx.ExecContext(ctx, "INSERT slow"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.QueryContext(ctx, "SELECT inside"); err != nil {
				t.Fatal(err)
			}
			if commit {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-outside; err != nil {
				t.Fatal(err)
			}
			want := []string{"SAVEPOINT atomicfga_1", "INSERT slow", "SELECT inside", "RELEASE SAVEPOINT atomicfga_1", "SELECT outside"}
			if !commit {
				want = []string{"SAVEPOINT atomicfga_1", "INSERT slow", "SELECT inside", "ROLLBACK TO SAVEPOINT atomicfga_1", "RELEASE SAVEPOINT atomicfga_1", "SELECT outside"}
			}
			if got := f.statements(); !slices.Equal(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

func TestFinishedCallsSendNoMoreStatements(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprint("pipeline=", pipeline), func(t *testing.T) {
			f := &fake{}
			var b Backend = f
			if pipeline {
				b = pipelining{f}
			}
			s := NewSession(b)
			db := OpenDB()
			defer db.Close()
			call := s.Call()
			// OpenFGA detaches query contexts from cancellation; stragglers
			// are recognized by their call alone.
			ctx := context.WithoutCancel(WithCall(context.Background(), call))
			started := make(chan struct{})
			go func() {
				close(started)
				_, _ = query(ctx, db, "SELECT slow")
			}()
			<-started
			var wg sync.WaitGroup
			errs := make(chan error, 20)
			for range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := query(ctx, db, "SELECT straggler")
					errs <- err
				}()
			}
			time.Sleep(5 * time.Millisecond)
			call.Finish()
			after := len(f.statements())
			wg.Wait()
			close(errs)
			if got := len(f.statements()); got != after {
				t.Fatalf("%d statements reached the backend after Finish returned", got-after)
			}
			for err := range errs {
				if err != nil && !errors.Is(err, ErrCallFinished) {
					t.Fatal(err)
				}
			}
			// The application, and other calls, can use the session again.
			next := s.Call()
			if _, err := query(WithCall(context.Background(), next), db, "SELECT next"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCanceledQueriesAreDroppedWhileQueued(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprint("pipeline=", pipeline), func(t *testing.T) {
			f := &fake{}
			var b Backend = f
			if pipeline {
				b = pipelining{f}
			}
			s := NewSession(b)
			db := OpenDB()
			defer db.Close()
			call := s.Call()
			base := WithCall(context.Background(), call)
			go func() { _, _ = query(base, db, "SELECT slow") }()
			time.Sleep(2 * time.Millisecond)

			branch, cancel := context.WithCancel(base)
			// As in OpenFGA: the query context is detached from cancellation,
			// but carries it as a value.
			detached := context.WithoutCancel(WithCancellation(branch))
			var wg sync.WaitGroup
			for range 10 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := query(detached, db, "SELECT canceled")
					if !errors.Is(err, context.Canceled) {
						t.Errorf("got %v", err)
					}
				}()
			}
			time.Sleep(2 * time.Millisecond)
			cancel()
			wg.Wait()
			call.Finish()
			for _, q := range f.statements() {
				if q == "SELECT canceled" {
					t.Fatal("a canceled query reached the backend")
				}
			}
			if s.Dropped() != 10 {
				t.Fatalf("dropped %d", s.Dropped())
			}
		})
	}
}

func TestAbortedTransactionsRefuseFurtherStatements(t *testing.T) {
	for _, state := range []string{"40001", "40P01", "25P02"} {
		t.Run(state, func(t *testing.T) {
			f := &fake{}
			s := NewSession(pipelining{f})
			db := OpenDB()
			defer db.Close()
			ctx := WithCall(context.Background(), s.Call())
			if _, err := query(ctx, db, "SELECT fail:"+state); !errors.Is(err, ErrTransactionAborted) || s.Aborted() == nil {
				t.Fatalf("got %v", err)
			}
			for _, q := range []func() error{
				func() error { _, err := query(ctx, db, "SELECT after"); return err },
				func() error { _, err := db.ExecContext(ctx, "INSERT after"); return err },
				func() error { _, err := db.BeginTx(ctx, nil); return err },
			} {
				if err := q(); !errors.Is(err, ErrTransactionAborted) {
					t.Fatalf("got %v", err)
				}
			}
			if n := len(f.statements()); n != 1 {
				t.Fatalf("%d statements reached an aborted transaction", n)
			}
		})
	}
	t.Run("statement errors do not abort", func(t *testing.T) {
		s := NewSession(&fake{})
		db := OpenDB()
		defer db.Close()
		ctx := WithCall(context.Background(), s.Call())
		if _, err := query(ctx, db, "SELECT fail:23505"); err == nil || errors.Is(err, ErrTransactionAborted) {
			t.Fatalf("got %v", err)
		}
		if _, err := query(ctx, db, "SELECT after"); err != nil {
			t.Fatal(err)
		}
	})
}
