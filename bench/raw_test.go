package bench

import (
	"context"
	"database/sql"
	"testing"

	"github.com/memoremix-ing/atomicfga"
)

// BenchmarkRawQuery measures the engine alone: the indexed lookup a direct
// check issues, sent straight to each SQLite engine's driver.
func BenchmarkRawQuery(b *testing.B) {
	const q = `SELECT object_type, object_id, relation, user_object_type, user_object_id, user_relation, condition_name, condition_context, inserted_at FROM tuple WHERE store = ? AND object_type = ? AND object_id = ? AND relation = ? AND user_object_type = ? AND user_object_id = ? AND user_relation = ? AND user_type = ?`
	for _, d := range []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Turso} {
		e := getEnv(b, d)
		for _, prepared := range []bool{false, true} {
			name := string(d) + "/unprepared"
			if prepared {
				name = string(d) + "/prepared"
			}
			b.Run(name, func(b *testing.B) {
				ctx := context.Background()
				tx, err := e.db.Reader.BeginTx(ctx, nil)
				if err != nil {
					b.Fatal(err)
				}
				defer tx.Rollback()
				var stmt *sql.Stmt
				if prepared {
					if stmt, err = tx.PrepareContext(ctx, q); err != nil {
						b.Fatal(err)
					}
				}
				args := []any{e.store, "doc", "500", "owner", "user", "u500", "", "user"}
				for b.Loop() {
					var rows *sql.Rows
					if prepared {
						rows, err = stmt.QueryContext(ctx, args...)
					} else {
						rows, err = tx.QueryContext(ctx, q, args...)
					}
					if err != nil {
						b.Fatal(err)
					}
					n := 0
					for rows.Next() {
						n++
					}
					rows.Close()
					if n != 1 {
						b.Fatalf("%d rows", n)
					}
				}
			})
		}
	}
}
