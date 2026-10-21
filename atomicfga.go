// Package atomicfga embeds OpenFGA in a Go application that owns the
// database transaction.
//
// OpenFGA's server opens its own connections and commits each write in a
// transaction of its own. atomicfga inverts that: the application begins a
// transaction, binds it to a [Session], and every OpenFGA operation made
// through the session reads and writes inside that transaction. Relationship
// tuples, authorization models and the application's own rows therefore
// commit or roll back together, and checks observe the transaction's own
// uncommitted writes.
//
// The OpenFGA code that runs is the upstream server, graph resolution and
// SQL datastores (the PostgreSQL datastore is ported from pgxpool to
// database/sql without changing its statements). See README.md for the
// transaction contract and the caches that are disabled to keep it.
package atomicfga

import (
	"database/sql"
	"fmt"
	"sync"

	sq "github.com/Masterminds/squirrel"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/mysql"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	"github.com/openfga/openfga/pkg/storage/sqlite"

	"github.com/memoremix-ing/atomicfga/internal/bind"
	"github.com/memoremix-ing/atomicfga/internal/pgstore"
)

// Dialect selects the SQL dialect of the application's database.
type Dialect string

const (
	Postgres Dialect = "postgres"
	MySQL    Dialect = "mysql"
	SQLite   Dialect = "sqlite"
)

// Option configures an [Engine].
type Option func(*options)

type options struct {
	server    []server.OpenFGAServiceV1Option
	datastore []sqlcommon.DatastoreOption
}

// WithServerOptions passes options to the embedded OpenFGA server, such as
// resolution limits, ListObjects deadlines and experimental flags.
//
// Options that would share state between transactions are overridden: the
// check query cache, check and ListObjects iterator caches, shared iterators
// and the cache controller are always disabled, and contexts are always
// propagated to the datastore.
func WithServerOptions(opts ...server.OpenFGAServiceV1Option) Option {
	return func(o *options) { o.server = append(o.server, opts...) }
}

// WithDatastoreOptions passes options to OpenFGA's SQL datastore, such as
// the maximum tuples per write. Connection pool settings have no effect.
func WithDatastoreOptions(opts ...sqlcommon.DatastoreOption) Option {
	return func(o *options) { o.datastore = append(o.datastore, opts...) }
}

// Engine is a long-lived embedded OpenFGA server that executes every
// operation inside a caller-supplied transaction. It holds no database
// connections. It is safe for concurrent use by many sessions.
type Engine struct {
	dialect   Dialect
	db        *sql.DB
	datastore storage.OpenFGADatastore
	server    *server.Server
	stbl      sq.StatementBuilderType
	committed *modelSet
}

// constructMu serializes datastore construction: upstream's
// sqlcommon.NewDBInfo sets goose's process-wide dialect.
var constructMu sync.Mutex

// Construct runs fn while no other datastore is being constructed by this
// package. Use it when constructing upstream OpenFGA SQL datastores in the
// same process as an Engine, to avoid a data race inside upstream code.
func Construct[T any](fn func() (T, error)) (T, error) {
	constructMu.Lock()
	defer constructMu.Unlock()
	return fn()
}

// New returns an Engine for the given SQL dialect. The database must contain
// OpenFGA's schema; see [Migrate].
func New(dialect Dialect, opts ...Option) (*Engine, error) {
	return Construct(func() (*Engine, error) { return newEngine(dialect, opts...) })
}

func newEngine(dialect Dialect, opts ...Option) (*Engine, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	db := bind.OpenDB()
	cfg := sqlcommon.NewConfig(append(o.datastore, sqlcommon.WithMaxIdleConns(256))...)

	var (
		ds   storage.OpenFGADatastore
		stbl = sq.StatementBuilder.PlaceholderFormat(sq.Question).RunWith(db)
		err  error
	)
	switch dialect {
	case Postgres:
		ds = pgstore.New(db, cfg)
		stbl = stbl.PlaceholderFormat(sq.Dollar)
	case MySQL:
		ds, err = mysql.NewWithDB(db, nil, cfg)
	case SQLite:
		ds, err = sqlite.NewWithDB(db, cfg)
	default:
		err = fmt.Errorf("atomicfga: unsupported dialect %q", dialect)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	ds = bind.PropagateCancellation(ds)
	srv, err := server.NewServerWithOpts(append(o.server,
		server.WithDatastore(ds),
		server.WithContextPropagationToDatastore(true),
		server.WithCheckQueryCacheEnabled(false),
		server.WithCheckIteratorCacheEnabled(false),
		server.WithListObjectsIteratorCacheEnabled(false),
		server.WithSharedIteratorEnabled(false),
		server.WithCacheControllerEnabled(false),
	)...)
	if err != nil {
		ds.Close()
		return nil, err
	}

	return &Engine{
		dialect:   dialect,
		db:        db,
		datastore: ds,
		server:    srv,
		stbl:      stbl,
		committed: newModelSet(10000),
	}, nil
}

// Dialect reports the engine's SQL dialect.
func (e *Engine) Dialect() Dialect { return e.dialect }

// Close stops the embedded server's background work.
func (e *Engine) Close() {
	e.server.Close()
}

// Tx is a transaction owned by the application. Construct one with [SQLTx]
// or [PgxTx], or implement it for another driver.
type Tx = bind.Backend

// Result is a fully read query result returned by a [Tx].
type Result = bind.Result

// SQLQueryer is satisfied by *sql.Tx (and by a *sql.Conn on which the
// application has issued BEGIN).
type SQLQueryer = bind.SQLQueryer

// PgxQueryer is satisfied by pgx.Tx (and by a *pgx.Conn on which the
// application has issued BEGIN).
type PgxQueryer = bind.PgxQueryer

// SQLTx binds a database/sql transaction for any dialect. It panics if
// given a *sql.DB: its statements would run on different pooled connections,
// outside any one transaction.
func SQLTx(tx SQLQueryer) Tx {
	if _, ok := tx.(*sql.DB); ok {
		panic("atomicfga: SQLTx needs a *sql.Tx (or a *sql.Conn inside BEGIN), not a *sql.DB")
	}
	return bind.SQLBackend{Tx: tx}
}

// PgxTx binds a native pgx transaction (PostgreSQL only).
func PgxTx(tx PgxQueryer) Tx { return bind.PgxBackend{Tx: tx} }

// Session returns a session that runs OpenFGA operations inside tx.
//
// Create exactly one session per transaction and stop using it when the
// transaction ends. A session is safe for concurrent use; its statements are
// serialized onto the transaction. The application must not use tx while a
// session call is in progress.
func (e *Engine) Session(tx Tx) *Session {
	return &Session{
		engine:  e,
		bind:    bind.NewSession(tx),
		visible: map[string]bool{},
		written: map[string]bool{},
	}
}

// ErrTransactionAborted matches (with errors.Is) the error returned once the
// database has aborted the application's transaction, typically a deadlock
// or serialization failure. The error carries gRPC code Aborted. The session
// refuses further statements: roll back the transaction and retry it.
var ErrTransactionAborted = bind.ErrTransactionAborted

// ErrNoSession is returned if OpenFGA attempts datastore access outside the
// session's context. It indicates an upstream code path that atomicfga does
// not yet route into the caller's transaction.
var ErrNoSession = bind.ErrNoSession

// modelSet remembers authorization models known to be committed, so that
// sessions can skip the per-transaction visibility check. Models are
// immutable and are never deleted, so a committed model stays visible.
type modelSet struct {
	mu  sync.RWMutex
	m   map[string]struct{}
	max int
}

func newModelSet(max int) *modelSet {
	return &modelSet{m: map[string]struct{}{}, max: max}
}

func (s *modelSet) has(key string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.m[key]
	return ok
}

func (s *modelSet) add(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) >= s.max {
		clear(s.m)
	}
	s.m[key] = struct{}{}
}

func modelKey(store, model string) string { return store + "\x00" + model }
