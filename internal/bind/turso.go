package bind

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"

	"modernc.org/sqlite"
)

// TursoErrors adapts errors from the Turso database engine
// (turso.tech/database/tursogo) for OpenFGA's SQLite datastore, which
// recognizes constraint violations only as modernc.org/sqlite errors. The
// driver is matched by its error messages so that atomicfga does not import
// it (and its native libraries) for applications that do not use it.
func TursoErrors(b Backend) Backend { return tursoBackend{b} }

type tursoBackend struct{ Backend }

func (t tursoBackend) Query(ctx context.Context, query string, args []any) (*Result, error) {
	res, err := t.Backend.Query(ctx, query, args)
	return res, translateTurso(err)
}

func (t tursoBackend) Exec(ctx context.Context, query string, args []any) (int64, error) {
	n, err := t.Backend.Exec(ctx, query, args)
	return n, translateTurso(err)
}

func translateTurso(err error) error {
	switch {
	case err == nil:
		return nil
	case chainHas(err, "turso: constraint failed"):
		return &tursoConstraint{err}
	case chainHas(err, "turso: database is busy"):
		return &abortError{err}
	}
	return err
}

func chainHas(err error, msg string) bool {
	for ; err != nil; err = errors.Unwrap(err) {
		if err.Error() == msg || strings.HasPrefix(err.Error(), msg+":") {
			return true
		}
	}
	return false
}

// tursoConstraint is a Turso constraint violation that also matches, with
// errors.As, a modernc SQLITE_CONSTRAINT_UNIQUE error.
type tursoConstraint struct{ cause error }

func (e *tursoConstraint) Error() string { return e.cause.Error() }
func (e *tursoConstraint) Unwrap() error { return e.cause }

func (e *tursoConstraint) As(target any) bool {
	p, ok := target.(**sqlite.Error)
	if !ok {
		return false
	}
	c := moderncConstraint()
	if c == nil {
		return false
	}
	*p = c
	return true
}

// moderncConstraint returns a real modernc constraint error. Its fields are
// unexported, so one is produced once from an in-memory database.
var moderncConstraint = sync.OnceValue(func() *sqlite.Error {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec("CREATE TABLE t (x PRIMARY KEY); INSERT INTO t VALUES (1); INSERT INTO t VALUES (1)")
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e
	}
	return nil
})
