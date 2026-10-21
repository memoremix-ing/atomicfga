package atomicfga_test

import (
	"context"
	"fmt"
	"testing"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	storagetest "github.com/openfga/openfga/pkg/storage/test"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// TestUpstreamDatastoreSuite runs OpenFGA's own datastore conformance suite
// against the transaction-bound datastores. Each datastore call runs in its
// own application transaction, committed on success.
//
// The variants run sequentially: the suite expects IDs from ulid.Make to
// increase in call order, which only holds while no other goroutine draws
// ULIDs in the same millisecond (ulid.Make pools its monotonic readers).
func TestUpstreamDatastoreSuite(t *testing.T) {
	for _, d := range testdb.Dialects() {
		for _, b := range testdb.Backends(d) {
			t.Run(fmt.Sprintf("%s/%s", d, b), func(t *testing.T) {
				db := testdb.Open(t, d)
				storagetest.RunAllTests(t, newAutocommit(t, db, b.Pgx))

				custom := newAutocommit(t, db, b.Pgx, sqlcommon.WithMaxTuplesPerWrite(5000))
				t.Run("WriteTuplesWithMaxTuplesPerWrite", storagetest.WriteTuplesWithMaxTuplesPerWrite(custom, context.Background()))
			})
		}
	}
}

// autocommit adapts a transaction-bound datastore to storage.OpenFGADatastore
// by running every call in its own transaction. Iterators are read to the
// end inside the transaction.
type autocommit struct {
	storage.OpenFGADatastore
	engine *atomicfga.Engine
	db     *testdb.DB
	pgx    bool
}

func newAutocommit(t *testing.T, db *testdb.DB, pgx bool, opts ...sqlcommon.DatastoreOption) *autocommit {
	e, err := atomicfga.New(db.Dialect, atomicfga.WithDatastoreOptions(opts...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return &autocommit{OpenFGADatastore: e.Datastore(), engine: e, db: db, pgx: pgx}
}

func (a *autocommit) run(ctx context.Context, fn func(context.Context) error) error {
	tx, commit, rollback, err := a.db.BeginTx(ctx, a.pgx)
	if err != nil {
		return err
	}
	bound, done := a.engine.Session(tx).Bind(ctx)
	err = fn(bound)
	done()
	if err != nil {
		_ = rollback()
		return err
	}
	return commit()
}

func drain(ctx context.Context, it storage.TupleIterator, err error) ([]*openfgav1.Tuple, error) {
	if err != nil {
		return nil, err
	}
	defer it.Stop()
	var out []*openfgav1.Tuple
	for {
		t, err := it.Next(ctx)
		if err != nil {
			if storage.IterIsDoneOrCancelled(err) {
				return out, nil
			}
			return nil, err
		}
		out = append(out, t)
	}
}

func (a *autocommit) iterator(ctx context.Context, open func(context.Context) (storage.TupleIterator, error)) (storage.TupleIterator, error) {
	var tuples []*openfgav1.Tuple
	err := a.run(ctx, func(ctx context.Context) error {
		it, err := open(ctx)
		tuples, err = drain(ctx, it, err)
		return err
	})
	if err != nil {
		return nil, err
	}
	return storage.NewStaticTupleIterator(tuples), nil
}

func (a *autocommit) Close() {}

func (a *autocommit) IsReady(ctx context.Context) (s storage.ReadinessStatus, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		s, err = a.OpenFGADatastore.IsReady(ctx)
		return err
	})
	return s, err
}

func (a *autocommit) Read(ctx context.Context, store string, filter storage.ReadFilter, options storage.ReadOptions) (storage.TupleIterator, error) {
	return a.iterator(ctx, func(ctx context.Context) (storage.TupleIterator, error) {
		return a.OpenFGADatastore.Read(ctx, store, filter, options)
	})
}

func (a *autocommit) ReadPage(ctx context.Context, store string, filter storage.ReadFilter, options storage.ReadPageOptions) (ts []*openfgav1.Tuple, token string, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		ts, token, err = a.OpenFGADatastore.ReadPage(ctx, store, filter, options)
		return err
	})
	return ts, token, err
}

func (a *autocommit) ReadUserTuple(ctx context.Context, store string, filter storage.ReadUserTupleFilter, options storage.ReadUserTupleOptions) (t *openfgav1.Tuple, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		t, err = a.OpenFGADatastore.ReadUserTuple(ctx, store, filter, options)
		return err
	})
	return t, err
}

func (a *autocommit) ReadUsersetTuples(ctx context.Context, store string, filter storage.ReadUsersetTuplesFilter, options storage.ReadUsersetTuplesOptions) (storage.TupleIterator, error) {
	return a.iterator(ctx, func(ctx context.Context) (storage.TupleIterator, error) {
		return a.OpenFGADatastore.ReadUsersetTuples(ctx, store, filter, options)
	})
}

func (a *autocommit) ReadStartingWithUser(ctx context.Context, store string, filter storage.ReadStartingWithUserFilter, options storage.ReadStartingWithUserOptions) (storage.TupleIterator, error) {
	return a.iterator(ctx, func(ctx context.Context) (storage.TupleIterator, error) {
		return a.OpenFGADatastore.ReadStartingWithUser(ctx, store, filter, options)
	})
}

func (a *autocommit) Write(ctx context.Context, store string, d storage.Deletes, w storage.Writes, opts ...storage.TupleWriteOption) error {
	return a.run(ctx, func(ctx context.Context) error {
		return a.OpenFGADatastore.Write(ctx, store, d, w, opts...)
	})
}

func (a *autocommit) ReadAuthorizationModel(ctx context.Context, store, id string) (m *openfgav1.AuthorizationModel, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		m, err = a.OpenFGADatastore.ReadAuthorizationModel(ctx, store, id)
		return err
	})
	return m, err
}

func (a *autocommit) ReadAuthorizationModels(ctx context.Context, store string, options storage.ReadAuthorizationModelsOptions) (ms []*openfgav1.AuthorizationModel, token string, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		ms, token, err = a.OpenFGADatastore.ReadAuthorizationModels(ctx, store, options)
		return err
	})
	return ms, token, err
}

func (a *autocommit) FindLatestAuthorizationModel(ctx context.Context, store string) (m *openfgav1.AuthorizationModel, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		m, err = a.OpenFGADatastore.FindLatestAuthorizationModel(ctx, store)
		return err
	})
	return m, err
}

func (a *autocommit) WriteAuthorizationModel(ctx context.Context, store string, model *openfgav1.AuthorizationModel) error {
	return a.run(ctx, func(ctx context.Context) error {
		return a.OpenFGADatastore.WriteAuthorizationModel(ctx, store, model)
	})
}

func (a *autocommit) CreateStore(ctx context.Context, store *openfgav1.Store) (s *openfgav1.Store, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		s, err = a.OpenFGADatastore.CreateStore(ctx, store)
		return err
	})
	return s, err
}

func (a *autocommit) DeleteStore(ctx context.Context, id string) error {
	return a.run(ctx, func(ctx context.Context) error {
		return a.OpenFGADatastore.DeleteStore(ctx, id)
	})
}

func (a *autocommit) GetStore(ctx context.Context, id string) (s *openfgav1.Store, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		s, err = a.OpenFGADatastore.GetStore(ctx, id)
		return err
	})
	return s, err
}

func (a *autocommit) ListStores(ctx context.Context, options storage.ListStoresOptions) (ss []*openfgav1.Store, token string, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		ss, token, err = a.OpenFGADatastore.ListStores(ctx, options)
		return err
	})
	return ss, token, err
}

func (a *autocommit) WriteAssertions(ctx context.Context, store, modelID string, assertions []*openfgav1.Assertion) error {
	return a.run(ctx, func(ctx context.Context) error {
		return a.OpenFGADatastore.WriteAssertions(ctx, store, modelID, assertions)
	})
}

func (a *autocommit) ReadAssertions(ctx context.Context, store, modelID string) (as []*openfgav1.Assertion, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		as, err = a.OpenFGADatastore.ReadAssertions(ctx, store, modelID)
		return err
	})
	return as, err
}

func (a *autocommit) ReadChanges(ctx context.Context, store string, filter storage.ReadChangesFilter, options storage.ReadChangesOptions) (cs []*openfgav1.TupleChange, token string, err error) {
	err = a.run(ctx, func(ctx context.Context) error {
		cs, token, err = a.OpenFGADatastore.ReadChanges(ctx, store, filter, options)
		return err
	})
	return cs, token, err
}
