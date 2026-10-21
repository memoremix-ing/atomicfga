package atomicfga

import (
	"fmt"
	"runtime"
	"sync"
	"weak"

	"github.com/memoremix-ing/atomicfga/internal/bind"
)

// registry maps live transactions to their shared state. Keys are weak, and
// states hold no reference to their transaction, so an entry never keeps a
// transaction alive; a cleanup removes it once the transaction is
// unreachable. Weak pointers identify objects, not addresses, so a later
// transaction allocated at the same address gets a new entry.
type registry struct {
	mu sync.Mutex
	m  map[weak.Pointer[byte]]*txState
}

func (r *registry) state(tx Tx) *txState {
	p := bind.Identity(tx)
	if p == nil {
		panic(fmt.Sprintf("atomicfga: cannot identify the transaction behind a %T; implement a custom Tx on a pointer type, so that every Session of one transaction shares its state", tx))
	}
	obj := (*byte)(p)
	key := weak.Make(obj)
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.m[key]; ok {
		return st
	}
	if r.m == nil {
		r.m = map[weak.Pointer[byte]]*txState{}
	}
	st := newTxState()
	r.m[key] = st
	runtime.AddCleanup(obj, r.forget, key)
	return st
}

func (r *registry) forget(key weak.Pointer[byte]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, key)
}

func (r *registry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.m)
}
