package bind

import "context"

// SQLiteLiveness wraps a SQLite or Turso transaction so that its session can
// tell when the database itself has ended the transaction. SQLite rolls a
// transaction back on some errors (a trigger's RAISE(ROLLBACK), some I/O
// and memory errors), and its connection then runs later statements in
// autocommit mode: each would commit on its own, outside the application's
// transaction.
func SQLiteLiveness(b Backend) Backend { return sqliteBackend{b} }

type sqliteBackend struct{ Backend }

func (b sqliteBackend) Unwrap() Backend { return b.Backend }

// ended reports whether the connection is no longer in a transaction. BEGIN
// fails inside a transaction and writes nothing; if it succeeds, the
// transaction had already ended, and the one BEGIN started is rolled back
// at once.
func (b sqliteBackend) ended(ctx context.Context) bool {
	if _, err := b.Backend.Exec(ctx, "BEGIN", nil); err != nil {
		return false
	}
	_, _ = b.Backend.Exec(ctx, "ROLLBACK", nil)
	return true
}
