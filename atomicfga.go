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
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	"github.com/openfga/openfga/pkg/storage/sqlite"

	"github.com/memoremix-ing/atomicfga/internal/bind"
	"github.com/memoremix-ing/atomicfga/internal/pgstore"
)

// Dialect selects the SQL dialect of the application's database.
type Dialect string

const (
	Postgres Dialect = "postgres"
	SQLite   Dialect = "sqlite"
	// Turso is the Turso database engine (turso.tech/database/tursogo). It
	// speaks SQLite's SQL and uses OpenFGA's SQLite schema and datastore.
	Turso Dialect = "turso"
)

const (
	tursoDefaultMaxTuplesPerWrite = 64
	tursoMaxTuplesPerWrite        = 90
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
// propagated to the datastore. The Rust evaluator (Session.RustCheck) follows
// the resolve node limit, and refuses under settings it does not model.
func WithServerOptions(opts ...server.OpenFGAServiceV1Option) Option {
	return func(o *options) { o.server = append(o.server, opts...) }
}

// WithDatastoreOptions passes options to OpenFGA's SQL datastore, such as
// the maximum tuples per write. Connection pool settings have no effect.
func WithDatastoreOptions(opts ...sqlcommon.DatastoreOption) Option {
	return func(o *options) { o.datastore = append(o.datastore, opts...) }
}

// ErrModelIDRequired is returned (with gRPC code InvalidArgument) for a
// request that uses an authorization model but names none: Check,
// BatchCheck, Expand, ListObjects, StreamedListObjects, ListUsers, Write,
// RustCheck and RustCheckStrict. OpenFGA would use the store's latest model,
// the one with the greatest ID, and it mints IDs from the clock; atomicfga
// requires the application to choose (say, a model ID it keeps in its own
// table and updates in the transaction that writes the model), so which
// rules a request follows never depends on an ordering of IDs.
var ErrModelIDRequired = status.Error(codes.InvalidArgument, "atomicfga: authorization_model_id is required: name the model the request uses")

// Engine is a long-lived embedded OpenFGA server that executes every
// operation inside a caller-supplied transaction. It holds no database
// connections. It is safe for concurrent use by many sessions.
type Engine struct {
	dialect   Dialect
	db        *sql.DB
	datastore storage.OpenFGADatastore
	server    *server.Server
	stbl      sq.StatementBuilderType
	models    *modelContents
	sessions  registry
	rust      rustState // the Rust Check evaluator (build tag rustcheck)
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
	if dialect == Turso {
		// Turso's parser limits expression depth to 100. OpenFGA deletes a
		// write's tuples with one OR of per-tuple conditions, which exceeds
		// it beyond 91 deletes.
		o.datastore = append([]sqlcommon.DatastoreOption{sqlcommon.WithMaxTuplesPerWrite(tursoDefaultMaxTuplesPerWrite)}, o.datastore...)
	}
	cfg := sqlcommon.NewConfig(append(o.datastore, sqlcommon.WithMaxIdleConns(256))...)
	if dialect == Turso && cfg.MaxTuplesPerWriteField > tursoMaxTuplesPerWrite {
		_ = db.Close()
		return nil, fmt.Errorf("atomicfga: Turso supports at most %d tuples per write, not %d", tursoMaxTuplesPerWrite, cfg.MaxTuplesPerWriteField)
	}

	var (
		ds   storage.OpenFGADatastore
		stbl = sq.StatementBuilder.PlaceholderFormat(sq.Question).RunWith(db)
		err  error
	)
	switch dialect {
	case Postgres:
		ds = pgstore.New(db, cfg)
		stbl = stbl.PlaceholderFormat(sq.Dollar)
	case SQLite, Turso:
		ds, err = sqlite.NewWithDB(db, cfg)
	default:
		err = fmt.Errorf("atomicfga: unsupported dialect %q", dialect)
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	models := newModelContents(1000)
	ds = &modelDatastore{bind.PropagateCancellation(ds), models}
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
		models:    models,
	}, nil
}

// Dialect reports the engine's SQL dialect.
func (e *Engine) Dialect() Dialect { return e.dialect }

// Close stops the embedded server's background work.
func (e *Engine) Close() {
	e.server.Close()
	e.rust.close()
}

// Tx is a transaction owned by the application. Construct one with [SQLTx]
// or [PgxTx], or implement it for another driver.
type Tx = bind.Backend

// Result is a fully read query result returned by a [Tx].
type Result = bind.Result

// SQLQueryer is satisfied by *sql.Tx and *ImmediateTx.
type SQLQueryer = bind.SQLQueryer

// PgxQueryer is satisfied by pgx.Tx.
type PgxQueryer = bind.PgxQueryer

// SQLTx binds a database/sql transaction (*sql.Tx, or *ImmediateTx from
// [BeginImmediate]) for any dialect.
//
// It panics if given a *sql.DB or *sql.Conn. A database's statements would
// run on different pooled connections; a connection outlives its
// transactions, so it cannot identify one (see [Engine.Session]).
func SQLTx(tx SQLQueryer) Tx {
	switch tx.(type) {
	case *sql.DB:
		panic("atomicfga: SQLTx needs a transaction (*sql.Tx or *atomicfga.ImmediateTx), not a *sql.DB")
	case *sql.Conn:
		panic("atomicfga: SQLTx needs a transaction (*sql.Tx or *atomicfga.ImmediateTx), not a *sql.Conn; for BEGIN IMMEDIATE use atomicfga.BeginImmediate")
	}
	return bind.SQLBackend{Tx: tx}
}

// PgxTx binds a native pgx transaction (PostgreSQL only). It panics if
// given a *pgx.Conn: a connection outlives its transactions, so it cannot
// identify one. Use the pgx.Tx returned by Begin.
func PgxTx(tx PgxQueryer) Tx {
	if _, ok := tx.(*pgx.Conn); ok {
		panic("atomicfga: PgxTx needs a pgx.Tx, not a *pgx.Conn")
	}
	return bind.PgxBackend{Tx: tx}
}

// Session returns a session that runs OpenFGA operations inside tx.
//
// Every session of the same transaction shares its state: call Session as
// often as convenient (for example in each function that receives the
// transaction), and use the sessions concurrently if you like. Their
// statements are serialized onto the transaction. The application must not
// use tx itself while a session call is in progress.
//
// Transactions are recognized by identity: the *sql.Tx, *ImmediateTx or
// pgx.Tx behind tx, or tx itself for a custom Tx, which must therefore be
// implemented on a pointer type (Session panics otherwise).
func (e *Engine) Session(tx Tx) *Session {
	if e.dialect == Turso {
		tx = bind.TursoErrors(tx)
	}
	if e.dialect == SQLite || e.dialect == Turso {
		tx = bind.SQLiteLiveness(tx)
	}
	return &Session{engine: e, tx: tx, txState: e.sessions.state(tx)}
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
