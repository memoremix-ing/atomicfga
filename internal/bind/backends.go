package bind

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SQLQueryer is satisfied by *sql.Tx, and by *sql.Conn when the caller has
// issued BEGIN on that connection.
type SQLQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// SQLBackend executes statements on a database/sql transaction.
type SQLBackend struct {
	Tx SQLQueryer
}

func (b SQLBackend) Query(ctx context.Context, query string, args []any) (*Result, error) {
	r, err := b.Tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	cols, err := r.Columns()
	if err != nil {
		return nil, err
	}
	res := &Result{Columns: cols}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for r.Next() {
		// Scanning into *any yields the driver's own value (byte slices are
		// copied), which is what the loopback rows hand back to database/sql.
		if err := r.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]driver.Value, len(cols))
		for i, v := range vals {
			row[i] = v
			vals[i] = nil
		}
		res.Rows = append(res.Rows, row)
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return res, r.Close()
}

func (b SQLBackend) Exec(ctx context.Context, query string, args []any) (int64, error) {
	res, err := b.Tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PgxQueryer is satisfied by pgx.Tx, and by *pgx.Conn when the caller has
// issued BEGIN on that connection.
type PgxQueryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// PgxBackend executes statements on a native pgx transaction.
type PgxBackend struct {
	Tx PgxQueryer
}

func (b PgxBackend) Query(ctx context.Context, query string, args []any) (*Result, error) {
	r, err := b.Tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return collect(r)
}

// Pipeline sends the statements as one pgx batch: a single round trip.
func (b PgxBackend) Pipeline(ctx context.Context, statements []Statement) ([]*Result, []error) {
	batch := &pgx.Batch{}
	for _, st := range statements {
		batch.Queue(st.Query, st.Args...)
	}
	br := b.Tx.SendBatch(ctx, batch)
	results := make([]*Result, len(statements))
	errs := make([]error, len(statements))
	for i := range statements {
		r, err := br.Query()
		if err != nil {
			errs[i] = err
			if r != nil {
				r.Close()
			}
			continue
		}
		results[i], errs[i] = collect(r)
	}
	if err := br.Close(); err != nil {
		for i := range errs {
			if errs[i] == nil && results[i] == nil {
				errs[i] = err
			}
		}
	}
	return results, errs
}

func collect(r pgx.Rows) (*Result, error) {
	defer r.Close()
	fields := r.FieldDescriptions()
	res := &Result{Columns: make([]string, len(fields))}
	for i, f := range fields {
		res.Columns[i] = f.Name
	}
	for r.Next() {
		vals, err := r.Values()
		if err != nil {
			return nil, err
		}
		row := make([]driver.Value, len(vals))
		for i, v := range vals {
			if row[i], err = pgxValue(v); err != nil {
				return nil, fmt.Errorf("column %q: %w", res.Columns[i], err)
			}
		}
		res.Rows = append(res.Rows, row)
	}
	r.Close()
	return res, r.Err()
}

func (b PgxBackend) Exec(ctx context.Context, query string, args []any) (int64, error) {
	tag, err := b.Tx.Exec(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func pgxValue(v any) (driver.Value, error) {
	switch x := v.(type) {
	case nil, int64, float64, bool, string, time.Time:
		return x, nil
	case []byte:
		return bytes.Clone(x), nil
	case int32:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case float32:
		return float64(x), nil
	case driver.Valuer:
		return x.Value()
	}
	return nil, fmt.Errorf("unsupported postgres value of type %T", v)
}
