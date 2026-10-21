package tests

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/storage/postgres"
	"github.com/openfga/openfga/pkg/storage/sqlcommon"
	"github.com/openfga/openfga/pkg/storage/sqlite"
	"github.com/openfga/openfga/pkg/tuple"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// newOracle starts the original OpenFGA server: upstream's own pooled SQL
// datastore for the dialect, on a database of its own, with its own
// transactions. It is the reference implementation for differential tests.
func newOracle(t testing.TB, d atomicfga.Dialect, opts ...server.OpenFGAServiceV1Option) *server.Server {
	t.Helper()
	if d == atomicfga.Turso {
		// Upstream has no Turso datastore; its SQLite one is the reference.
		d = atomicfga.SQLite
	}
	db := testdb.Open(t, d)
	ds, err := atomicfga.Construct(func() (storage.OpenFGADatastore, error) {
		return OpenUpstream(d, db.URI)
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.NewServerWithOpts(append(opts, server.WithDatastore(ds))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// OpenUpstream opens upstream OpenFGA's datastore for a dialect.
func OpenUpstream(d atomicfga.Dialect, uri string) (storage.OpenFGADatastore, error) {
	cfg := sqlcommon.NewConfig()
	switch d {
	case atomicfga.Postgres:
		return postgres.New(uri, cfg)
	case atomicfga.SQLite:
		return sqlite.New(uri, cfg)
	}
	return nil, fmt.Errorf("unknown dialect %q", d)
}

var ulidPattern = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{26}`)

// outcome renders an API error for comparison: its status code and message
// with store/model IDs (which differ between the two implementations)
// replaced.
func outcome(err error) string {
	if err == nil {
		return "ok"
	}
	s, ok := status.FromError(err)
	if !ok {
		return "non-status error: " + err.Error()
	}
	return fmt.Sprintf("%d %s", s.Code(), ulidPattern.ReplaceAllString(s.Message(), "<ulid>"))
}

func tupleKeyString(k *openfgav1.TupleKey) string {
	s := tuple.TupleKeyToString(k)
	if c := k.GetCondition(); c != nil {
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
		s += fmt.Sprintf(" with %s %x", c.GetName(), b)
	}
	return s
}

func sortedTupleKeys(ts []*openfgav1.Tuple) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, tupleKeyString(t.GetKey()))
	}
	slices.Sort(out)
	return out
}

func sortedUsers(us []*openfgav1.User) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, tuple.UserProtoToString(u))
	}
	slices.Sort(out)
	return out
}

// readAll pages through Read with the given page size.
func readAll(ctx context.Context, read func(context.Context, *openfgav1.ReadRequest) (*openfgav1.ReadResponse, error), req *openfgav1.ReadRequest) ([]*openfgav1.Tuple, error) {
	var out []*openfgav1.Tuple
	for {
		resp, err := read(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetTuples()...)
		if resp.GetContinuationToken() == "" {
			return out, nil
		}
		req = proto.Clone(req).(*openfgav1.ReadRequest)
		req.ContinuationToken = resp.GetContinuationToken()
	}
}

// expandString renders an Expand tree with children and users sorted, since
// neither implementation orders them.
func expandString(n *openfgav1.UsersetTree_Node) string {
	if n == nil {
		return "nil"
	}
	var b strings.Builder
	b.WriteString(ulidPattern.ReplaceAllString(n.GetName(), "<ulid>"))
	switch v := n.GetValue().(type) {
	case *openfgav1.UsersetTree_Node_Leaf:
		switch l := v.Leaf.GetValue().(type) {
		case *openfgav1.UsersetTree_Leaf_Users:
			users := slices.Clone(l.Users.GetUsers())
			slices.Sort(users)
			fmt.Fprintf(&b, "{users %v}", users)
		case *openfgav1.UsersetTree_Leaf_Computed:
			fmt.Fprintf(&b, "{computed %s}", l.Computed.GetUserset())
		case *openfgav1.UsersetTree_Leaf_TupleToUserset:
			var computed []string
			for _, c := range l.TupleToUserset.GetComputed() {
				computed = append(computed, c.GetUserset())
			}
			slices.Sort(computed)
			fmt.Fprintf(&b, "{ttu %s %v}", l.TupleToUserset.GetTupleset(), computed)
		}
	case *openfgav1.UsersetTree_Node_Union:
		b.WriteString(nodesString("union", v.Union.GetNodes()))
	case *openfgav1.UsersetTree_Node_Intersection:
		b.WriteString(nodesString("intersection", v.Intersection.GetNodes()))
	case *openfgav1.UsersetTree_Node_Difference:
		fmt.Fprintf(&b, "{difference %s %s}", expandString(v.Difference.GetBase()), expandString(v.Difference.GetSubtract()))
	}
	return b.String()
}

func nodesString(op string, nodes []*openfgav1.UsersetTree_Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, expandString(n))
	}
	slices.Sort(parts)
	return "{" + op + " " + strings.Join(parts, " ") + "}"
}
