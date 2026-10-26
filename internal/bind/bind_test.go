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
// SQLSTATE, "busy" ones fail as a busy Turso database, and "slow" ones take a
// while.
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
	if strings.Contains(query, "busy") {
		return errors.New("turso: database is busy")
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
			db := OpenDB()
			s := NewSession()
			defer db.Close()
			call := s.Call(b)
			ctx := WithCall(context.Background(), call)
			var wg sync.WaitGroup
			for i := range 50 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					// Slow, so that queries queue behind the first round trip.
					q := fmt.Sprintf("SELECT slow %d", i)
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
			b := Backend(pipelining{f})
			db := OpenDB()
			s := NewSession()
			defer db.Close()
			call := s.Call(b)
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
			db := OpenDB()
			s := NewSession()
			defer db.Close()
			call := s.Call(b)
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
			next := s.Call(b)
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
			db := OpenDB()
			s := NewSession()
			defer db.Close()
			call := s.Call(b)
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
	for _, c := range []struct {
		name, failing string
		backend       func(*fake) Backend
	}{
		{"40001", "SELECT fail:40001", func(f *fake) Backend { return pipelining{f} }},
		{"40P01", "SELECT fail:40P01", func(f *fake) Backend { return pipelining{f} }},
		{"25P02", "SELECT fail:25P02", func(f *fake) Backend { return pipelining{f} }},
		{"turso busy", "SELECT busy", func(f *fake) Backend { return TursoErrors(f) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &fake{}
			db := OpenDB()
			s := NewSession()
			defer db.Close()
			ctx := WithCall(context.Background(), s.Call(c.backend(f)))
			if _, err := query(ctx, db, c.failing); !errors.Is(err, ErrTransactionAborted) || s.Aborted() == nil {
				t.Fatalf("got %v (aborted: %v)", err, s.Aborted())
			}
			if a := s.Aborted().Error(); strings.Count(a, ErrTransactionAborted.Error()) != 1 {
				t.Fatalf("abort wrapped twice: %s", a)
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
		b := Backend(&fake{})
		db := OpenDB()
		s := NewSession()
		defer db.Close()
		ctx := WithCall(context.Background(), s.Call(b))
		if _, err := query(ctx, db, "SELECT fail:23505"); err == nil || errors.Is(err, ErrTransactionAborted) {
			t.Fatalf("got %v", err)
		}
		if _, err := query(ctx, db, "SELECT after"); err != nil {
			t.Fatal(err)
		}
	})
}

func TestDeferredQueriesRideTheNextRoundTrip(t *testing.T) {
	f := &fake{}
	db := OpenDB()
	defer db.Close()
	s := NewSession()
	c := s.Call(pipelining{f})
	ctx := WithCall(context.Background(), c)

	await, ok := c.Defer(ctx, "SELECT deferred", nil)
	if !ok {
		t.Fatal("pipelining backend refused to defer")
	}
	if n := len(f.statements()); n != 0 {
		t.Fatalf("deferring sent %d statements", n)
	}
	if _, err := query(ctx, db, "SELECT next"); err != nil {
		t.Fatal(err)
	}
	res, err := await()
	if err != nil || res.Rows[0][0] != "SELECT deferred" {
		t.Fatalf("deferred result %v, %v", res, err)
	}
	if got := f.statements(); !slices.Equal(got, []string{"SELECT deferred", "SELECT next"}) || s.RoundTrips() != 1 {
		t.Fatalf("statements %v in %d round trips", got, s.RoundTrips())
	}

	// Awaited with nothing else to ride along with, it is sent alone.
	await, _ = c.Defer(ctx, "SELECT alone", nil)
	if res, err := await(); err != nil || res.Rows[0][0] != "SELECT alone" || s.RoundTrips() != 2 {
		t.Fatalf("lone deferred query: %v, %v, %d round trips", res, err, s.RoundTrips())
	}
	c.Finish()

	if _, ok := NewSession().Call(f).Defer(ctx, "SELECT 1", nil); ok {
		t.Fatal("a backend that cannot pipeline deferred a query")
	}
}

// sqliteFake is a fake SQLite connection, whose transaction a statement
// containing "end" ends (as a trigger's RAISE(ROLLBACK) does).
type sqliteFake struct {
	fake
	outside bool
}

func (f *sqliteFake) Exec(ctx context.Context, query string, args []any) (int64, error) {
	switch {
	case query == "BEGIN" && !f.outside:
		return 0, errors.New("cannot start a transaction within a transaction")
	case query == "BEGIN":
		f.outside = false
	case query == "ROLLBACK":
		f.outside = true
	case strings.Contains(query, "end"):
		f.outside = true
		return 0, errors.New("constraint failed: trigger rolled back")
	}
	return f.fake.Exec(ctx, query, args)
}

func TestEndedTransactionsRefuseFurtherStatements(t *testing.T) {
	f := &sqliteFake{}
	db := OpenDB()
	defer db.Close()
	s := NewSession()
	ctx := WithCall(context.Background(), s.Call(SQLiteLiveness(f)))

	if _, err := db.ExecContext(ctx, "INSERT fail:23505"); err == nil || errors.Is(err, ErrTransactionAborted) {
		t.Fatalf("a statement error in a live transaction: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT ok"); err != nil {
		t.Fatalf("live transaction refused: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT end"); !errors.Is(err, ErrTransactionAborted) || s.Aborted() == nil {
		t.Fatalf("an error that ended the transaction: %v", err)
	}
	if !f.outside {
		t.Fatal("the liveness probe left a transaction open")
	}
	before := len(f.statements())
	if _, err := db.ExecContext(ctx, "INSERT after"); !errors.Is(err, ErrTransactionAborted) || len(f.statements()) != before {
		t.Fatalf("statement after the transaction ended: %v", err)
	}
}

// releaseFails fails savepoint cleanup.
type releaseFails struct{ fake }

func (f *releaseFails) Exec(ctx context.Context, query string, args []any) (int64, error) {
	if strings.HasPrefix(query, "RELEASE") {
		return 0, errors.New("no such savepoint")
	}
	return f.fake.Exec(ctx, query, args)
}

func TestFailedSavepointCleanupPoisonsTheTransaction(t *testing.T) {
	db := OpenDB()
	defer db.Close()
	s := NewSession()
	ctx := WithCall(context.Background(), s.Call(&releaseFails{}))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); !errors.Is(err, ErrTransactionAborted) {
		t.Fatalf("failed RELEASE: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT after"); !errors.Is(err, ErrTransactionAborted) {
		t.Fatalf("statement after failed cleanup: %v", err)
	}
}
