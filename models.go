package atomicfga

import (
	"context"
	"sync"

	"github.com/oklog/ulid/v2"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/storage"
)

// modelContents remembers the content of authorization models read from the
// database, by store and ID. Model IDs are unique ULIDs and models are
// immutable, so a model's content is the same in every transaction that can
// see it, and stays correct after a rollback (its ID is never reused). It
// says nothing about visibility: see [resolveModel].
type modelContents struct {
	mu  sync.RWMutex
	m   map[modelKey]*openfgav1.AuthorizationModel
	max int
}

type modelKey struct{ store, id string }

func newModelContents(max int) *modelContents {
	return &modelContents{m: map[modelKey]*openfgav1.AuthorizationModel{}, max: max}
}

func (c *modelContents) get(store, id string) *openfgav1.AuthorizationModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[modelKey{store, id}]
}

func (c *modelContents) add(store, id string, m *openfgav1.AuthorizationModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		clear(c.m)
	}
	c.m[modelKey{store, id}] = m
}

// modelDatastore records the models the datastore reads, and serves a model
// pinned to the request's context without reading it.
//
// A pinned model is how a request evaluates before its model's visibility is
// known (see [Session.speculate]): OpenFGA coalesces concurrent lookups of a
// model across requests, so a lookup made in a transaction that cannot see
// the model would give its "not found" to transactions that can.
type modelDatastore struct {
	storage.OpenFGADatastore
	contents *modelContents
}

func (d *modelDatastore) ReadAuthorizationModel(ctx context.Context, store, id string) (*openfgav1.AuthorizationModel, error) {
	if p, ok := ctx.Value(pinnedModelKey{}).(pinnedModel); ok && p.key == (modelKey{store, id}) {
		return p.model, nil
	}
	m, err := d.OpenFGADatastore.ReadAuthorizationModel(ctx, store, id)
	if err == nil {
		d.contents.add(store, id, m)
	}
	return m, err
}

type pinnedModelKey struct{}

type pinnedModel struct {
	key   modelKey
	model *openfgav1.AuthorizationModel
}

func withPinnedModel(ctx context.Context, store, id string, m *openfgav1.AuthorizationModel) context.Context {
	return context.WithValue(ctx, pinnedModelKey{}, pinnedModel{modelKey{store, id}, m})
}

// WriteAuthorizationModel stores model under an ID greater than the floor
// the request's context carries for its store (see
// [Session.WriteAuthorizationModel]), replacing the ID OpenFGA minted if it
// is not. OpenFGA answers with the ID of the model it handed the datastore,
// so the response names the ID stored.
func (d *modelDatastore) WriteAuthorizationModel(ctx context.Context, store string, model *openfgav1.AuthorizationModel) error {
	if f, ok := ctx.Value(modelFloorKey{}).(modelFloor); ok && f.store == store {
		id, err := ulid.ParseStrict(model.GetId())
		if err != nil {
			return err
		}
		if id.Compare(f.id) <= 0 {
			// The minting clock is behind the floor's: take the floor's
			// millisecond plus one, with fresh entropy.
			next, err := ulid.New(f.id.Time()+1, ulid.DefaultEntropy())
			if err != nil {
				return err
			}
			model.Id = next.String()
		}
	}
	return d.OpenFGADatastore.WriteAuthorizationModel(ctx, store, model)
}

type modelFloorKey struct{}

// modelFloor is the greatest model ID of store a model write must exceed.
type modelFloor struct {
	store string
	id    ulid.ULID
}

func withModelFloor(ctx context.Context, store string, id ulid.ULID) context.Context {
	return context.WithValue(ctx, modelFloorKey{}, modelFloor{store, id})
}
