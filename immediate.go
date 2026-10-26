package atomicfga

import (
	"context"
	"database/sql"
	"sync"

	"github.com/memoremix-ing/atomicfga/internal/bind"
)

// ImmediateTx is a SQLite or Turso transaction begun with BEGIN IMMEDIATE,
// which takes the database's write lock at the start instead of at the
// first write. Bind it with [SQLTx]. It behaves like *sql.Tx: Commit or
// Rollback ends it, and Rollback after Commit returns sql.ErrTxDone, so
// `defer tx.Rollback()` is safe.
type ImmediateTx struct {
	mu   sync.Mutex
	conn *sql.Conn
	done bool
}

// BeginImmediate begins a write transaction on a SQLite or Turso database.
//
// Use it for transactions that write. A deferred transaction (database/sql's
// BeginTx, unless the driver is configured otherwise) that later needs to
// write fails with a busy error if another connection committed meanwhile,
// and has to be retried; tursogo's BeginTx is always deferred.
//
// If the write lock stays busy past the database's busy timeout, the error
// matches [ErrTransactionAborted]: retry the transaction, as for any other
// abort.
func BeginImmediate(ctx context.Context, db *sql.DB) (*ImmediateTx, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, bind.Classify(err)
	}
	return &ImmediateTx{conn: conn}, nil
}

func (t *ImmediateTx) live() (*sql.Conn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, sql.ErrTxDone
	}
	return t.conn, nil
}

// QueryContext executes a query in the transaction.
func (t *ImmediateTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	conn, err := t.live()
	if err != nil {
		return nil, err
	}
	return conn.QueryContext(ctx, query, args...)
}

// QueryRowContext executes a query that returns at most one row. After the
// transaction ends, Scan reports sql.ErrConnDone.
func (t *ImmediateTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// ExecContext executes a statement in the transaction.
func (t *ImmediateTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	conn, err := t.live()
	if err != nil {
		return nil, err
	}
	return conn.ExecContext(ctx, query, args...)
}

// Commit commits the transaction. If COMMIT fails, the transaction is
// rolled back; the error matches [ErrTransactionAborted] if retrying the
// transaction may succeed (the database was busy).
func (t *ImmediateTx) Commit() error {
	return t.end("COMMIT")
}

// Rollback rolls the transaction back.
func (t *ImmediateTx) Rollback() error {
	return t.end("ROLLBACK")
}

func (t *ImmediateTx) end(stmt string) error {
	t.mu.Lock()
	conn, done := t.conn, t.done
	t.done = true
	t.mu.Unlock()
	if done {
		return sql.ErrTxDone
	}
	defer conn.Close()
	ctx := context.Background()
	_, err := conn.ExecContext(ctx, stmt)
	if err != nil && stmt == "COMMIT" {
		// SQLite keeps the transaction open when COMMIT fails (for example
		// while busy); do not return the connection to the pool inside it.
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return bind.Classify(err)
	}
	return err
}
