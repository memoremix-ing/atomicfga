package bind

import (
	"errors"

	"github.com/go-sql-driver/mysql"
)

type abortError struct{ cause error }

func (e *abortError) Error() string { return ErrTransactionAborted.Error() + ": " + e.cause.Error() }

func (e *abortError) Unwrap() []error { return []error{ErrTransactionAborted, e.cause} }

// abortsTransaction reports whether err means the database has aborted (or
// will only roll back) the whole transaction, rather than one statement.
func abortsTransaction(err error) bool {
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
	// MySQL: InnoDB rolls back the whole transaction of a deadlock victim
	// (1213, SQLSTATE 40001); a lock wait timeout rolls back one statement.
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		return my.Number == 1213 || string(my.SQLState[:]) == "40001"
	}
	return false
}
