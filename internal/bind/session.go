// Package bind routes database/sql calls into a transaction owned by the caller.
//
// OpenFGA's SQL datastores are written against *sql.DB and open their own
// transactions. [OpenDB] returns a *sql.DB whose connections hold no database
// state: every statement is forwarded to the [Call] carried by the
// statement's context, and BeginTx opens a savepoint inside that call's
// transaction. The datastores therefore run unmodified while every byte they
// read or write goes through the caller's transaction.
package bind

import (
	"context"
	"database/sql/driver"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
)

var (
	// ErrNoSession is returned when a statement reaches the loopback driver
	// without a [Call] in its context. It indicates a code path that would
	// otherwise have used a connection outside the caller's transaction.
	ErrNoSession = errors.New("atomicfga: datastore access without a transaction-bound session")

	// ErrTransactionAborted is returned for every statement after the
	// database aborted the transaction (a deadlock or serialization
	// failure). The application must roll back and retry the transaction.
	ErrTransactionAborted = errors.New("atomicfga: the database aborted the transaction; roll back and retry it")

	// ErrCallFinished is returned to goroutines that OpenFGA left running
	// after an API call returned (for example the losing branches of a
	// union). Their statements must not reach a transaction that the
	// application is using again.
	ErrCallFinished = errors.New("atomicfga: statement issued after its API call returned")
)

// Result is a fully buffered query result.
type Result struct {
	Columns []string
	Rows    [][]driver.Value
}

// Backend executes statements on a transaction owned by the caller.
//
// Query must read the complete result before returning, so that the next
// statement can be sent on the same connection. Values must be valid
// [driver.Value]s. Implementations are never called concurrently by a
// [Session] and receive contexts without cancellation: canceling a statement
// would abort or close the caller's transaction on several drivers.
type Backend interface {
	Query(ctx context.Context, query string, args []any) (*Result, error)
	Exec(ctx context.Context, query string, args []any) (int64, error)
}

// Session serializes statements onto a single Backend.
//
// A Session is safe for concurrent use. OpenFGA resolves checks with many
// goroutines; their statements are queued here, and statements whose context
// is canceled while queued are dropped without reaching the database.
type Session struct {
	backend Backend
	sem     chan struct{}

	qmu   sync.Mutex
	queue []*pending // queries awaiting a pipeline (see Pipeliner)

	savepoints atomic.Uint64
	statements atomic.Int64
	roundTrips atomic.Int64
	dropped    atomic.Int64
	aborted    atomic.Pointer[abortError]
}

// NewSession returns a Session that executes statements on b.
func NewSession(b Backend) *Session {
	return &Session{backend: b, sem: make(chan struct{}, 1)}
}

// Statements reports how many statements the session has sent to its backend,
// including savepoint management.
func (s *Session) Statements() int64 { return s.statements.Load() }

// RoundTrips reports how many times the session called its backend. It is
// lower than Statements when queries were pipelined.
func (s *Session) RoundTrips() int64 { return s.roundTrips.Load() }

// Aborted returns a non-nil error wrapping [ErrTransactionAborted] and the
// database's error once the database has aborted the transaction.
func (s *Session) Aborted() error {
	if a := s.aborted.Load(); a != nil {
		return a
	}
	return nil
}

// observe records an error that aborted the whole transaction. MySQL, for
// example, rolls back a deadlocked transaction and leaves the connection in
// autocommit mode; later statements must not run there.
func (s *Session) observe(err error) error {
	if err != nil && abortsTransaction(err) {
		a := &abortError{err}
		s.aborted.CompareAndSwap(nil, a)
		return a
	}
	return err
}

// Dropped reports how many statements were abandoned before reaching the
// backend because their call had been canceled or had returned.
func (s *Session) Dropped() int64 { return s.dropped.Load() }

func (s *Session) acquire(ctx context.Context) error {
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) release() {
	<-s.sem
}

// Call scopes the statements of one API call to a session.
type Call struct {
	session *Session
	done    atomic.Bool
}

// Call starts an API call on the session.
func (s *Session) Call() *Call {
	return &Call{session: s}
}

// Finish ends the call. Statements it issued that are still queued are
// dropped, and Finish returns only once none of them is executing, so the
// application can use its transaction again.
func (c *Call) Finish() {
	c.done.Store(true)
	c.session.sem <- struct{}{}
	c.session.release()
}

// refuse returns why a statement must not be sent, if it must not.
func (c *Call) refuse(ctx, sig context.Context) error {
	if err := sig.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.done.Load() {
		return ErrCallFinished
	}
	if a := c.session.aborted.Load(); a != nil {
		return a
	}
	return nil
}

func (c *Call) admit(ctx context.Context, held bool) error {
	s := c.session
	sig := cancellation(ctx)
	if !held {
		if err := s.acquire(sig); err != nil {
			s.dropped.Add(1)
			return err
		}
	}
	if err := c.refuse(ctx, sig); err != nil {
		if !held {
			s.release()
		}
		s.dropped.Add(1)
		return err
	}
	s.statements.Add(1)
	s.roundTrips.Add(1)
	return nil
}

func (c *Call) query(ctx context.Context, held bool, query string, args []any) (*Result, error) {
	if p, ok := c.session.backend.(Pipeliner); ok && !held {
		return c.pipelined(ctx, p, query, args)
	}
	if err := c.admit(ctx, held); err != nil {
		return nil, err
	}
	if !held {
		defer c.session.release()
	}
	res, err := c.session.backend.Query(context.WithoutCancel(ctx), query, args)
	return res, c.session.observe(err)
}

func (c *Call) exec(ctx context.Context, held bool, query string, args []any) (int64, error) {
	if err := c.admit(ctx, held); err != nil {
		return 0, err
	}
	if !held {
		defer c.session.release()
	}
	n, err := c.session.backend.Exec(context.WithoutCancel(ctx), query, args)
	return n, c.session.observe(err)
}

// begin acquires the session exclusively and opens a savepoint. The session
// stays acquired until the savepoint is released or rolled back, so concurrent
// readers cannot observe (or be rolled back with) a partially applied write.
func (c *Call) begin(ctx context.Context) (string, error) {
	if err := c.admit(ctx, false); err != nil {
		return "", err
	}
	s := c.session
	name := "atomicfga_" + strconv.FormatUint(s.savepoints.Add(1), 10)
	if _, err := s.backend.Exec(context.WithoutCancel(ctx), "SAVEPOINT "+name, nil); err != nil {
		s.release()
		return "", s.observe(err)
	}
	return name, nil
}

func (s *Session) end(statements ...string) error {
	defer s.release()
	if a := s.aborted.Load(); a != nil {
		return a
	}
	for _, stmt := range statements {
		s.statements.Add(1)
		s.roundTrips.Add(1)
		if _, err := s.backend.Exec(context.Background(), stmt, nil); err != nil {
			return s.observe(err)
		}
	}
	return nil
}

type callCtxKey struct{}

// WithCall returns a context whose datastore statements belong to c.
func WithCall(ctx context.Context, c *Call) context.Context {
	return context.WithValue(ctx, callCtxKey{}, c)
}

func callFromContext(ctx context.Context) (*Call, bool) {
	c, ok := ctx.Value(callCtxKey{}).(*Call)
	return c, ok && c != nil
}

type signalCtxKey struct{}

// WithCancellation records ctx's cancellation as a value. OpenFGA detaches
// query contexts from cancellation (context.WithoutCancel) before they reach
// the driver; values survive that, so queued statements of a canceled
// resolution branch can still be dropped.
func WithCancellation(ctx context.Context) context.Context {
	if ctx.Done() == nil {
		return ctx
	}
	return context.WithValue(ctx, signalCtxKey{}, ctx)
}

func cancellation(ctx context.Context) context.Context {
	if sig, ok := ctx.Value(signalCtxKey{}).(context.Context); ok {
		return sig
	}
	return ctx
}
