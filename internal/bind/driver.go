package bind

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
)

// OpenDB returns a *sql.DB backed by the loopback driver. It can be shared by
// any number of sessions; its connections only remember an open savepoint.
func OpenDB() *sql.DB {
	db := sql.OpenDB(connector{})
	// Connections are a few words of memory; keep them instead of churning.
	db.SetMaxIdleConns(256)
	return db
}

type connector struct{}

func (connector) Connect(context.Context) (driver.Conn, error) { return &conn{}, nil }

func (connector) Driver() driver.Driver { return loopbackDriver{} }

type loopbackDriver struct{}

func (loopbackDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("atomicfga: the loopback driver is only available through bind.OpenDB")
}

type conn struct {
	tx *savepoint
}

var (
	_ driver.Conn               = (*conn)(nil)
	_ driver.ConnBeginTx        = (*conn)(nil)
	_ driver.QueryerContext     = (*conn)(nil)
	_ driver.ExecerContext      = (*conn)(nil)
	_ driver.NamedValueChecker  = (*conn)(nil)
	_ driver.SessionResetter    = (*conn)(nil)
	_ driver.Validator          = (*conn)(nil)
	_ driver.Pinger             = (*conn)(nil)
	_ driver.StmtQueryContext   = (*stmt)(nil)
	_ driver.StmtExecContext    = (*stmt)(nil)
	_ driver.Tx                 = (*savepoint)(nil)
	_ driver.Rows               = (*rows)(nil)
	_ driver.Result             = result(0)
	_ driver.Connector          = connector{}
	_ driver.Driver             = loopbackDriver{}
	_ driver.ConnPrepareContext = (*conn)(nil)
)

// call returns the call a statement belongs to, and whether this connection
// already holds the call's session exclusively for an open savepoint.
func (c *conn) call(ctx context.Context) (*Call, bool, error) {
	call, ok := callFromContext(ctx)
	if c.tx != nil {
		if ok && call.session != c.tx.call.session {
			return nil, false, errors.New("atomicfga: statement session differs from its savepoint session")
		}
		return c.tx.call, true, nil
	}
	if !ok {
		return nil, false, ErrNoSession
	}
	return call, false, nil
}

func (c *conn) QueryContext(ctx context.Context, query string, named []driver.NamedValue) (driver.Rows, error) {
	call, held, err := c.call(ctx)
	if err != nil {
		return nil, err
	}
	args, err := values(named)
	if err != nil {
		return nil, err
	}
	res, err := call.query(ctx, held, query, args)
	if err != nil {
		return nil, err
	}
	return &rows{res: res}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, named []driver.NamedValue) (driver.Result, error) {
	call, held, err := c.call(ctx)
	if err != nil {
		return nil, err
	}
	args, err := values(named)
	if err != nil {
		return nil, err
	}
	n, err := call.exec(ctx, held, query, args)
	if err != nil {
		return nil, err
	}
	return result(n), nil
}

func (c *conn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if c.tx != nil {
		return nil, errors.New("atomicfga: nested savepoint on one connection")
	}
	call, ok := callFromContext(ctx)
	if !ok {
		return nil, ErrNoSession
	}
	name, err := call.begin(ctx)
	if err != nil {
		return nil, err
	}
	c.tx = &savepoint{conn: c, call: call, name: name}
	return c.tx, nil
}

func (c *conn) Begin() (driver.Tx, error) {
	return nil, errors.New("atomicfga: Begin requires a context; use BeginTx")
}

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{conn: c, query: query}, nil
}

func (c *conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return &stmt{conn: c, query: query}, nil
}

func (c *conn) Close() error { return nil }

// CheckNamedValue passes every argument through unchanged, so the caller's
// driver performs the only conversion, exactly as it would for OpenFGA.
func (c *conn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *conn) ResetSession(context.Context) error {
	if c.tx != nil {
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) IsValid() bool { return c.tx == nil }

func (c *conn) Ping(context.Context) error { return nil }

func values(named []driver.NamedValue) ([]any, error) {
	if len(named) == 0 {
		return nil, nil
	}
	args := make([]any, len(named))
	for i, nv := range named {
		if nv.Name != "" {
			return nil, errors.New("atomicfga: named statement arguments are not supported")
		}
		args[i] = nv.Value
	}
	return args, nil
}

type stmt struct {
	conn  *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }

func (s *stmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("atomicfga: Exec requires a context")
}

func (s *stmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("atomicfga: Query requires a context")
}

func (s *stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

type savepoint struct {
	conn *conn
	call *Call
	name string
}

func (t *savepoint) Commit() error {
	t.conn.tx = nil
	return t.call.session.end(t.call.backend, "RELEASE SAVEPOINT "+t.name)
}

func (t *savepoint) Rollback() error {
	t.conn.tx = nil
	return t.call.session.end(t.call.backend, "ROLLBACK TO SAVEPOINT "+t.name, "RELEASE SAVEPOINT "+t.name)
}

type rows struct {
	res  *Result
	next int
}

func (r *rows) Columns() []string { return r.res.Columns }
func (r *rows) Close() error      { return nil }

func (r *rows) Next(dest []driver.Value) error {
	if r.next >= len(r.res.Rows) {
		return io.EOF
	}
	copy(dest, r.res.Rows[r.next])
	r.next++
	return nil
}

type result int64

func (r result) RowsAffected() (int64, error) { return int64(r), nil }

func (result) LastInsertId() (int64, error) {
	return 0, errors.New("atomicfga: LastInsertId is not supported")
}
