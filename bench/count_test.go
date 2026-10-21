package bench

import (
	"context"
	"sync/atomic"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/storage"
)

// counting counts the SQL queries the original datastore issues: one per
// call, or per iterator that is actually read (iterators query lazily).
type counting struct {
	storage.OpenFGADatastore
	n atomic.Int64
}

func (c *counting) Read(ctx context.Context, store string, f storage.ReadFilter, o storage.ReadOptions) (storage.TupleIterator, error) {
	it, err := c.OpenFGADatastore.Read(ctx, store, f, o)
	return c.wrap(it), err
}

func (c *counting) ReadPage(ctx context.Context, store string, f storage.ReadFilter, o storage.ReadPageOptions) ([]*openfgav1.Tuple, string, error) {
	c.n.Add(1)
	return c.OpenFGADatastore.ReadPage(ctx, store, f, o)
}

func (c *counting) ReadUserTuple(ctx context.Context, store string, f storage.ReadUserTupleFilter, o storage.ReadUserTupleOptions) (*openfgav1.Tuple, error) {
	c.n.Add(1)
	return c.OpenFGADatastore.ReadUserTuple(ctx, store, f, o)
}

func (c *counting) ReadUsersetTuples(ctx context.Context, store string, f storage.ReadUsersetTuplesFilter, o storage.ReadUsersetTuplesOptions) (storage.TupleIterator, error) {
	it, err := c.OpenFGADatastore.ReadUsersetTuples(ctx, store, f, o)
	return c.wrap(it), err
}

func (c *counting) ReadStartingWithUser(ctx context.Context, store string, f storage.ReadStartingWithUserFilter, o storage.ReadStartingWithUserOptions) (storage.TupleIterator, error) {
	it, err := c.OpenFGADatastore.ReadStartingWithUser(ctx, store, f, o)
	return c.wrap(it), err
}

func (c *counting) ReadAuthorizationModel(ctx context.Context, store, id string) (*openfgav1.AuthorizationModel, error) {
	c.n.Add(1)
	return c.OpenFGADatastore.ReadAuthorizationModel(ctx, store, id)
}

func (c *counting) FindLatestAuthorizationModel(ctx context.Context, store string) (*openfgav1.AuthorizationModel, error) {
	c.n.Add(1)
	return c.OpenFGADatastore.FindLatestAuthorizationModel(ctx, store)
}

func (c *counting) wrap(it storage.TupleIterator) storage.TupleIterator {
	if it == nil {
		return nil
	}
	return &countingIterator{TupleIterator: it, n: &c.n}
}

type countingIterator struct {
	storage.TupleIterator
	n       *atomic.Int64
	fetched atomic.Bool
}

func (it *countingIterator) fetch() {
	if it.fetched.CompareAndSwap(false, true) {
		it.n.Add(1)
	}
}

func (it *countingIterator) Next(ctx context.Context) (*openfgav1.Tuple, error) {
	it.fetch()
	return it.TupleIterator.Next(ctx)
}

func (it *countingIterator) Head(ctx context.Context) (*openfgav1.Tuple, error) {
	it.fetch()
	return it.TupleIterator.Head(ctx)
}
