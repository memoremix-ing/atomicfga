// Package testdb creates fresh, migrated databases for tests and benchmarks.
//
// SQLite databases are files in the test's temporary directory. PostgreSQL
// and MySQL databases are created on the servers started by
// scripts/databases.sh (or named by ATOMICFGA_POSTGRES_URI and
// ATOMICFGA_MYSQL_URI) and dropped afterwards. Tests are skipped when a
// server is unreachable, unless ATOMICFGA_REQUIRE_DATABASES is set.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"github.com/memoremix-ing/atomicfga"
)

const (
	defaultPostgres = "postgres://postgres:atomicfga@localhost:55432/postgres?sslmode=disable"
	defaultMySQL    = "root:atomicfga@tcp(localhost:53306)/"
)

// DB is a fresh database with OpenFGA's schema.
type DB struct {
	Dialect atomicfga.Dialect
	// SQL is the application's database/sql handle.
	SQL *sql.DB
	// Pgx is a native pool on the same database (PostgreSQL only).
	Pgx *pgxpool.Pool
	// URI opens the database with upstream OpenFGA's own datastores.
	URI string
	// Name identifies the database.
	Name string
	// Isolation is used for transactions started by Begin and BeginTx.
	Isolation sql.IsolationLevel
	// Reader is a handle whose transactions do not take SQLite's write lock,
	// for observing committed state while a writer is open.
	Reader *sql.DB
}

// Dialects lists the dialects to test, from ATOMICFGA_DIALECTS (comma
// separated) or all of them.
func Dialects() []atomicfga.Dialect {
	v := os.Getenv("ATOMICFGA_DIALECTS")
	if v == "" {
		return []atomicfga.Dialect{atomicfga.SQLite, atomicfga.Postgres, atomicfga.MySQL}
	}
	var ds []atomicfga.Dialect
	for _, d := range strings.Split(v, ",") {
		ds = append(ds, atomicfga.Dialect(strings.TrimSpace(d)))
	}
	return ds
}

func unavailable(tb testing.TB, format string, args ...any) {
	tb.Helper()
	if os.Getenv("ATOMICFGA_REQUIRE_DATABASES") != "" {
		tb.Fatalf(format, args...)
	}
	tb.Skipf(format+" (run scripts/databases.sh up)", args...)
}

func uniqueName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "atomicfga_" + hex.EncodeToString(b)
}

// Open returns a fresh database for dialect d that is dropped when tb ends.
func Open(tb testing.TB, d atomicfga.Dialect) *DB {
	tb.Helper()
	var db *DB
	switch d {
	case atomicfga.SQLite:
		db = openSQLite(tb)
	case atomicfga.Postgres:
		db = openPostgres(tb)
	case atomicfga.MySQL:
		db = openMySQL(tb)
	default:
		tb.Fatalf("unknown dialect %q", d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := atomicfga.Migrate(ctx, db.SQL, d); err != nil {
		tb.Fatalf("migrate %s: %v", d, err)
	}
	return db
}

// SQLiteDSN returns the application DSN used for SQLite files: WAL, a busy
// timeout, and BEGIN IMMEDIATE for every transaction (as OpenFGA uses).
func SQLiteDSN(path string) string {
	return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_txlock=immediate"
}

func openSQLite(tb testing.TB) *DB {
	path := filepath.Join(tb.TempDir(), "openfga.db")
	sqldb, err := sql.Open("sqlite", SQLiteDSN(path))
	if err != nil {
		tb.Fatal(err)
	}
	reader, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = sqldb.Close(); _ = reader.Close() })
	return &DB{Dialect: atomicfga.SQLite, SQL: sqldb, URI: path, Name: path, Reader: reader}
}

var (
	adminMu sync.Mutex
	admins  = map[string]*sql.DB{}
)

func admin(tb testing.TB, driver, uri string) *sql.DB {
	tb.Helper()
	adminMu.Lock()
	defer adminMu.Unlock()
	if db, ok := admins[uri]; ok {
		return db
	}
	db, err := sql.Open(driver, uri)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = db.PingContext(ctx)
		cancel()
	}
	if err != nil {
		unavailable(tb, "%s server unavailable at %s: %v", driver, uri, err)
	}
	db.SetMaxOpenConns(4)
	admins[uri] = db
	return db
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openPostgres(tb testing.TB) *DB {
	adminURI := env("ATOMICFGA_POSTGRES_URI", defaultPostgres)
	a := admin(tb, "pgx", adminURI)
	name := uniqueName()
	if _, err := a.Exec("CREATE DATABASE " + name); err != nil {
		tb.Fatalf("create database: %v", err)
	}
	u, err := url.Parse(adminURI)
	if err != nil {
		tb.Fatal(err)
	}
	u.Path = "/" + name
	uri := u.String()

	sqldb, err := sql.Open("pgx", uri)
	if err != nil {
		tb.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), uri)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		pool.Close()
		_ = sqldb.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})
	return &DB{Dialect: atomicfga.Postgres, SQL: sqldb, Pgx: pool, URI: uri, Name: name, Reader: sqldb}
}

func openMySQL(tb testing.TB) *DB {
	adminURI := env("ATOMICFGA_MYSQL_URI", defaultMySQL)
	a := admin(tb, "mysql", adminURI)
	name := uniqueName()
	if _, err := a.Exec("CREATE DATABASE " + name); err != nil {
		tb.Fatalf("create database: %v", err)
	}
	cfg, err := mysql.ParseDSN(adminURI)
	if err != nil {
		tb.Fatal(err)
	}
	cfg.DBName = name
	cfg.ParseTime = true
	uri := cfg.FormatDSN()

	sqldb, err := sql.Open("mysql", uri)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_ = sqldb.Close()
		_, _ = a.Exec("DROP DATABASE IF EXISTS " + name)
	})
	return &DB{Dialect: atomicfga.MySQL, SQL: sqldb, URI: uri, Name: name, Reader: sqldb}
}

// Begin starts an application transaction and binds it as a [atomicfga.Tx].
// With pgx set (PostgreSQL only) the transaction is a native pgx.Tx.
func (db *DB) Begin(tb testing.TB, native bool) (atomicfga.Tx, func() error, func() error) {
	tb.Helper()
	tx, commit, rollback, err := db.BeginTx(context.Background(), native)
	if err != nil {
		tb.Fatal(err)
	}
	return tx, commit, rollback
}

// BeginReader starts a transaction on the Reader handle (or a native pgx
// transaction), which observes committed state only.
func (db *DB) BeginReader(ctx context.Context, native bool) (atomicfga.Tx, func() error, error) {
	if native {
		tx, err := db.Pgx.Begin(ctx)
		if err != nil {
			return nil, nil, err
		}
		return atomicfga.PgxTx(tx), func() error { return tx.Rollback(ctx) }, nil
	}
	tx, err := db.Reader.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	return atomicfga.SQLTx(tx), tx.Rollback, nil
}

// BeginTx is Begin without a testing.TB.
func (db *DB) BeginTx(ctx context.Context, native bool) (tx atomicfga.Tx, commit, rollback func() error, err error) {
	if native {
		if db.Pgx == nil {
			return nil, nil, nil, fmt.Errorf("pgx transactions need PostgreSQL, not %s", db.Dialect)
		}
		ptx, err := db.Pgx.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgxIsolation[db.Isolation]})
		if err != nil {
			return nil, nil, nil, err
		}
		return atomicfga.PgxTx(ptx), func() error { return ptx.Commit(ctx) }, func() error { return ptx.Rollback(ctx) }, nil
	}
	stx, err := db.SQL.BeginTx(ctx, &sql.TxOptions{Isolation: db.Isolation})
	if err != nil {
		return nil, nil, nil, err
	}
	return atomicfga.SQLTx(stx), stx.Commit, stx.Rollback, nil
}

var pgxIsolation = map[sql.IsolationLevel]pgx.TxIsoLevel{
	sql.LevelReadCommitted:  pgx.ReadCommitted,
	sql.LevelRepeatableRead: pgx.RepeatableRead,
	sql.LevelSerializable:   pgx.Serializable,
}

// Backends lists the transaction kinds to test for a dialect.
func Backends(d atomicfga.Dialect) []Backend {
	if d == atomicfga.Postgres {
		return []Backend{{Name: "sql"}, {Name: "pgx", Pgx: true}}
	}
	return []Backend{{Name: "sql"}}
}

// Backend names a transaction kind.
type Backend struct {
	Name string
	Pgx  bool
}

// String implements fmt.Stringer.
func (b Backend) String() string { return fmt.Sprint(b.Name) }
