package bind

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type abortError struct{ cause error }

func (e *abortError) Error() string { return ErrTransactionAborted.Error() + ": " + e.cause.Error() }

func (e *abortError) Unwrap() []error { return []error{ErrTransactionAborted, e.cause} }

// abortsTransaction reports whether err means the database has aborted (or
// will only roll back) the whole transaction, rather than one statement.
func abortsTransaction(err error) bool {
	// Already classified, as by translateTurso.
	if errors.Is(err, ErrTransactionAborted) {
		return true
	}
	// PostgreSQL (pgx, pgx/stdlib and lib/pq errors expose SQLSTATE).
	var pg interface{ SQLState() string }
	if errors.As(err, &pg) {
		switch pg.SQLState() {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"25P02": // in_failed_sql_transaction
			return true
		}
	}
	// SQLite: a busy statement inside a transaction means its snapshot
	// cannot become a write transaction (or the lock wait timed out). The
	// transaction must be rolled back and retried; BEGIN IMMEDIATE avoids it.
	var lite *sqlite.Error
	if errors.As(err, &lite) {
		switch lite.Code() & 0xff {
		case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
			return true
		}
	}
	return false
}
