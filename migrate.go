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
func Migrate(ctx context.Context, db *sql.DB, dialect Dialect) error {
	var (
		gd  goose.Dialect
		dir string
	)
	switch dialect {
	case Postgres:
		gd, dir = goose.DialectPostgres, assets.PostgresMigrationDir
	case MySQL:
		gd, dir = goose.DialectMySQL, assets.MySQLMigrationDir
	case SQLite:
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
	return nil
}
