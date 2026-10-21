package bind

import (
	"context"
	"sync/atomic"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/storage"
)

// PropagateCancellation wraps ds so that the cancellation of each tuple read
// reaches the session's statement queue (see [WithCancellation]). Without it,
// statements of resolution branches that OpenFGA has already abandoned would
// still be executed one after another on the caller's transaction.
func PropagateCancellation(ds storage.OpenFGADatastore) storage.OpenFGADatastore {
	return &cancelDatastore{ds}
}

type cancelDatastore struct {
	storage.OpenFGADatastore
}

func (d *cancelDatastore) Read(ctx context.Context, store string, filter storage.ReadFilter, options storage.ReadOptions) (storage.TupleIterator, error) {
	it, err := d.OpenFGADatastore.Read(WithCancellation(ctx), store, filter, options)
	return wrapIterator(it), err
}

func (d *cancelDatastore) ReadPage(ctx context.Context, store string, filter storage.ReadFilter, options storage.ReadPageOptions) ([]*openfgav1.Tuple, string, error) {
	return d.OpenFGADatastore.ReadPage(WithCancellation(ctx), store, filter, options)
}

func (d *cancelDatastore) ReadUsersetTuples(ctx context.Context, store string, filter storage.ReadUsersetTuplesFilter, options storage.ReadUsersetTuplesOptions) (storage.TupleIterator, error) {
	it, err := d.OpenFGADatastore.ReadUsersetTuples(WithCancellation(ctx), store, filter, options)
	return wrapIterator(it), err
}

func (d *cancelDatastore) ReadStartingWithUser(ctx context.Context, store string, filter storage.ReadStartingWithUserFilter, options storage.ReadStartingWithUserOptions) (storage.TupleIterator, error) {
	it, err := d.OpenFGADatastore.ReadStartingWithUser(WithCancellation(ctx), store, filter, options)
	return wrapIterator(it), err
}

func wrapIterator(it storage.TupleIterator) storage.TupleIterator {
	if it == nil {
		return nil
	}
	return &cancelIterator{TupleIterator: it}
}

// cancelIterator marks the context of the call that fetches its rows. SQL
// iterators query lazily on the first Next or Head, then serve a buffer.
type cancelIterator struct {
	storage.TupleIterator
	fetched atomic.Bool
}

func (it *cancelIterator) ctx(ctx context.Context) context.Context {
	if it.fetched.Load() {
		return ctx
	}
	return WithCancellation(ctx)
}

func (it *cancelIterator) Next(ctx context.Context) (*openfgav1.Tuple, error) {
	t, err := it.TupleIterator.Next(it.ctx(ctx))
	if err == nil {
		it.fetched.Store(true)
	}
	return t, err
}

func (it *cancelIterator) Head(ctx context.Context) (*openfgav1.Tuple, error) {
	t, err := it.TupleIterator.Head(it.ctx(ctx))
	if err == nil {
		it.fetched.Store(true)
	}
	return t, err
}
