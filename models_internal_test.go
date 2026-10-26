package atomicfga

import (
	"context"
	"testing"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/storage"
)

// countingModels is a datastore that counts model reads.
type countingModels struct {
	storage.OpenFGADatastore
	reads int
}

func (d *countingModels) ReadAuthorizationModel(_ context.Context, _, id string) (*openfgav1.AuthorizationModel, error) {
	d.reads++
	return &openfgav1.AuthorizationModel{Id: id}, nil
}

func TestPinnedModelsAreNeverRead(t *testing.T) {
	inner := &countingModels{}
	ds := &modelDatastore{inner, newModelContents(10)}
	ctx := context.Background()

	pinned := &openfgav1.AuthorizationModel{Id: "pinned"}
	got, err := ds.ReadAuthorizationModel(withPinnedModel(ctx, "s", "m", pinned), "s", "m")
	if err != nil || got != pinned || inner.reads != 0 {
		t.Fatalf("pinned model: %v, %v, %d reads", got, err, inner.reads)
	}
	// A pin applies to its own store and ID only.
	for _, key := range []modelKey{{"s", "other"}, {"other", "m"}} {
		if got, _ := ds.ReadAuthorizationModel(withPinnedModel(ctx, "s", "m", pinned), key.store, key.id); got == pinned {
			t.Fatalf("pin served for %v", key)
		}
	}
	if inner.reads != 2 || ds.contents.get("s", "other") == nil || ds.contents.get("other", "m") == nil {
		t.Fatalf("unpinned reads were not recorded: %d reads", inner.reads)
	}
}
