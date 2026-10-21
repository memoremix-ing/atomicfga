package atomicfga

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	"github.com/pressly/goose/v3"

	"github.com/openfga/openfga/assets"
)

// Migrate applies OpenFGA's own schema migrations to db, recording them in
// goose_db_version exactly as `openfga migrate` does, so either tool can
// manage the schema. Migrations run on db's own connections, outside any
// application transaction.
//
// On Turso, Migrate also seeds query planner statistics (see
// seedTursoStatistics). Turso connections read statistics only when they
// open: migrate through a separate *sql.DB, or before the application's
// connections open, the first time.
func Migrate(ctx context.Context, db *sql.DB, dialect Dialect) error {
	var (
		gd  goose.Dialect
		dir string
	)
	switch dialect {
	case Postgres:
		gd, dir = goose.DialectPostgres, assets.PostgresMigrationDir
	case SQLite, Turso:
		gd, dir = goose.DialectSQLite3, assets.SqliteMigrationDir
	default:
		return fmt.Errorf("atomicfga: unsupported dialect %q", dialect)
	}
	fsys, err := fs.Sub(assets.EmbedMigrations, dir)
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(gd, db, fsys)
	if err != nil {
		return fmt.Errorf("atomicfga: migrations: %w", err)
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("atomicfga: migrations: %w", err)
	}
	if dialect == Turso {
		if err := seedTursoStatistics(ctx, db); err != nil {
			return fmt.Errorf("atomicfga: planner statistics: %w", err)
		}
	}
	return nil
}

// tursoStatistics describe a large database of many stores, in which each
// additional equality column narrows a lookup. sqlite_stat1 format: the row
// count, then the average rows per distinct prefix of the index's columns.
var tursoStatistics = [][3]string{
	{"tuple", "sqlite_autoindex_tuple_1", "1000000 1000 100 20 10 5 2 1"},
	{"tuple", "idx_tuple_partial_user", "500000 1000 100 20 10 5 2 1"},
	{"tuple", "idx_tuple_partial_userset", "500000 1000 100 20 10 5 2 1"},
	{"tuple", "idx_reverse_lookup_user", "1000000 1000 100 20 10 5 2"},
	{"tuple", "idx_store_ulid", "1000000 1000 1"},
	{"tuple", "idx_tuple_ulid", "1000000 1"},
	{"changelog", "sqlite_autoindex_changelog_1", "1000000 1000 1"},
	{"authorization_model", "sqlite_autoindex_authorization_model_1", "10000 10 1"},
	{"store", "sqlite_autoindex_store_1", "1000 1"},
}

// seedTursoStatistics gives Turso's query planner statistics unless the
// database already has some for the tuple table. Without them, Turso
// answers OpenFGA's point lookups from the (store, ulid) index, scanning
// every tuple of the store; statistics of empty tables do not help. Running
// ANALYZE later replaces these with measured statistics.
//
// Turso connections load statistics when they open and keep them, so only
// connections opened after the first migration benefit.
func seedTursoStatistics(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'tuple'").Scan(&n); err != nil {
		// sqlite_stat1 does not exist until the first ANALYZE.
		if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_stat1 WHERE tbl = 'tuple'").Scan(&n); err != nil {
			return err
		}
	}
	if n > 0 {
		return nil
	}
	for _, s := range tursoStatistics {
		if _, err := db.ExecContext(ctx, "DELETE FROM sqlite_stat1 WHERE tbl = ? AND idx = ?", s[0], s[1]); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES (?, ?, ?)", s[0], s[1], s[2]); err != nil {
			return err
		}
	}
	return nil
}
