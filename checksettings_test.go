package atomicfga

import (
	"strings"
	"testing"

	"github.com/openfga/openfga/pkg/server"
	serverconfig "github.com/openfga/openfga/pkg/server/config"
)

// The Rust evaluator follows settings read from OpenFGA's server's
// unexported fields: an upstream change to them must fail here.
func TestCheckSettingsAreReadFromTheServer(t *testing.T) {
	read := func(opts ...server.OpenFGAServiceV1Option) checkSettings {
		t.Helper()
		e, err := New(SQLite, WithServerOptions(opts...))
		if err != nil {
			t.Fatal(err)
		}
		defer e.Close()
		c, err := readCheckSettings(e.server)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	const store = "01HZZZSTORE000000000000000"

	c := read()
	if c.ResolveNodeLimit != serverconfig.DefaultResolveNodeLimit || c.refusal(store) != "" {
		t.Errorf("defaults: limit %d, refusal %q", c.ResolveNodeLimit, c.refusal(store))
	}
	if c := read(server.WithResolveNodeLimit(9), server.WithResolveNodeLimit(7)); c.ResolveNodeLimit != 7 {
		t.Errorf("the last limit given is the server's: got %d", c.ResolveNodeLimit)
	}
	c = read(server.WithExperimentals(serverconfig.ExperimentalWeightedGraphCheck))
	if why := c.refusal(store); !strings.Contains(why, serverconfig.ExperimentalWeightedGraphCheck) {
		t.Errorf("weighted graph check: refusal %q", why)
	}
	c = read(
		server.WithExperimentals(serverconfig.ExperimentalAccessControlParams),
		server.WithAccessControlParams(true, store, "01HZZZMODEL000000000000000", "oidc"),
	)
	if why := c.refusal(store); !strings.Contains(why, "access control") {
		t.Errorf("access control: refusal %q", why)
	}
}
