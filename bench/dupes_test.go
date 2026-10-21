package bench

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/memoremix-ing/atomicfga"
)

// recordingTx records the statements atomicfga sends to the application's
// transaction.
type recordingTx struct {
	atomicfga.Tx
	mu  sync.Mutex
	log []string
}

func (r *recordingTx) Query(ctx context.Context, q string, args []any) (*atomicfga.Result, error) {
	r.mu.Lock()
	r.log = append(r.log, fmt.Sprint(q, args))
	r.mu.Unlock()
	return r.Tx.Query(ctx, q, args)
}

// TestStatementProfile reports, per read operation, how many statements one
// call sends and how many of them are distinct.
func TestStatementProfile(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	for _, d := range dialects() {
		e := getEnvT(t, d)
		for _, o := range reads {
			ctx := context.Background()
			tx, done, err := e.db.BeginReader(ctx, false)
			if err != nil {
				t.Fatal(err)
			}
			rec := &recordingTx{Tx: tx}
			if _, err := invoke(ctx, e.engine.Session(rec), o.build(e.store, e.model)); err != nil {
				t.Fatal(err)
			}
			_ = done()
			distinct := map[string]int{}
			for _, s := range rec.log {
				distinct[s]++
			}
			t.Logf("%-8s %-26s statements=%3d distinct=%3d", d, o.name, len(rec.log), len(distinct))
		}
	}
}
