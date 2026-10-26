package atomicfga

import (
	"errors"
	"fmt"
	"reflect"
	"unsafe"

	"github.com/openfga/openfga/pkg/server"
	serverconfig "github.com/openfga/openfga/pkg/server/config"
)

// checkSettings are the embedded server's settings that change what Check
// answers, beyond the model and the request: what an evaluator other than
// OpenFGA's own (the Rust port) must follow, or refuse.
//
// They are read from the constructed server rather than from the options
// given to it: options are opaque functions, and later ones override
// earlier ones. The server keeps them in unexported fields, so reading them
// depends on upstream's field names; [readCheckSettings] fails, rather than
// guess, when they change.
type checkSettings struct {
	// ResolveNodeLimit is server.WithResolveNodeLimit.
	ResolveNodeLimit uint32 `json:"resolve_node_limit"`

	flags         featureFlags // per store; see refusal
	accessControl bool
}

// featureFlags is upstream's featureflags.Client (an internal package).
type featureFlags interface {
	Boolean(flag string, storeID string) bool
}

func readCheckSettings(srv *server.Server) (checkSettings, error) {
	v := reflect.ValueOf(srv).Elem()
	field := func(name string, kind reflect.Kind) (reflect.Value, error) {
		f := v.FieldByName(name)
		if !f.IsValid() || f.Kind() != kind {
			return reflect.Value{}, fmt.Errorf("atomicfga: OpenFGA's server has no %s field %s", kind, name)
		}
		return f, nil
	}
	limit, err := field("resolveNodeLimit", reflect.Uint32)
	if err != nil {
		return checkSettings{}, err
	}
	client, err := field("featureFlagClient", reflect.Interface)
	if err != nil {
		return checkSettings{}, err
	}
	// Read-only access to an unexported field of the server, which is never
	// written after construction.
	flags, ok := reflect.NewAt(client.Type(), unsafe.Pointer(client.UnsafeAddr())).Elem().Interface().(featureFlags)
	if !ok || flags == nil {
		return checkSettings{}, errors.New("atomicfga: OpenFGA's server has no feature flag client")
	}
	return checkSettings{
		ResolveNodeLimit: uint32(limit.Uint()),
		flags:            flags,
		accessControl:    srv.IsAccessControlEnabled(),
	}, nil
}

// refusal says why an evaluator that follows only ResolveNodeLimit cannot
// answer a check of store as OpenFGA would, or "".
//
// The other settings were reviewed against upstream's Check: breadth and
// concurrency limits, throttling, timeouts and the planner change timing,
// not answers (the Rust port already answers for every strategy the planner
// may pick); caches are always disabled; the shadow checker only logs; and
// the check optimizations flag is recorded but unused.
func (c checkSettings) refusal(storeID string) string {
	switch {
	case c.accessControl:
		return "access control is enabled, and only OpenFGA's Check authorizes the caller"
	case c.flags.Boolean(serverconfig.ExperimentalWeightedGraphCheck, storeID):
		return "the " + serverconfig.ExperimentalWeightedGraphCheck + " flag selects OpenFGA's other Check implementation"
	}
	return ""
}
