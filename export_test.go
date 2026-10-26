package atomicfga

import (
	"context"

	"github.com/openfga/openfga/pkg/storage"
)

// Datastore exposes the transaction-bound datastore to tests.
func (e *Engine) Datastore() storage.OpenFGADatastore { return e.datastore }

// Bind binds ctx to a call on the session, for direct datastore use in tests.
func (s *Session) Bind(ctx context.Context) (context.Context, func()) {
	ctx, c := s.call(ctx)
	return ctx, c.Finish
}

// RegisteredTransactions reports how many transactions have shared state.
func (e *Engine) RegisteredTransactions() int { return e.sessions.len() }
