//go:build rustcheck

package tests

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/server/config"
	"github.com/openfga/openfga/tests/check"

	"github.com/memoremix-ing/atomicfga"
)

// rustGate tallies the Rust evaluator against OpenFGA's own Check, both run
// in the same transaction on the same request.
type rustGate struct {
	mu       sync.Mutex
	checks   int
	answered int
	outcomes map[string]int // of answered checks
	refusals map[string]int
	messages int // answered with the right code but another message
	wrong    []string
}

func newRustGate(t *testing.T, name string) *rustGate {
	g := &rustGate{outcomes: map[string]int{}, refusals: map[string]int{}}
	t.Cleanup(func() { g.report(t, name) })
	return g
}

func checkOutcome(resp *openfgav1.CheckResponse, err error) (code, message string) {
	if err != nil {
		s, _ := status.FromError(err)
		return fmt.Sprintf("error %d", s.Code()), s.Message()
	}
	return fmt.Sprintf("allowed=%v", resp.GetAllowed()), ""
}

// compare runs both evaluators and returns OpenFGA's response, and a
// description of the disagreement if the Rust evaluator answered wrongly.
func (g *rustGate) compare(ctx context.Context, s *atomicfga.Session, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error, string) {
	want, werr := s.Check(ctx, req)
	got, gerr := s.RustCheckStrict(ctx, req)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.checks++
	if errors.Is(gerr, atomicfga.ErrRustRefused) {
		reason := strings.TrimPrefix(gerr.Error(), atomicfga.ErrRustRefused.Error()+": ")
		g.refusals[reason]++
		return want, werr, ""
	}
	g.answered++
	wc, wm := checkOutcome(want, werr)
	gc, gm := checkOutcome(got, gerr)
	g.outcomes[wc]++
	if wc != gc {
		d := fmt.Sprintf("%v\n  openfga: %s %s\n  rust:    %s %s", req, wc, wm, gc, gm)
		g.wrong = append(g.wrong, d)
		return want, werr, d
	}
	if wm != gm {
		g.messages++
	}
	return want, werr, ""
}

func (g *rustGate) report(t *testing.T, name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	refused := g.checks - g.answered
	t.Logf("%s: %d checks: %d answered by Rust (%d wrong, %d with another error message), %d refused; answered outcomes %v",
		name, g.checks, g.answered, len(g.wrong), g.messages, refused, g.outcomes)
	reasons := slices.Collect(maps.Keys(g.refusals))
	slices.SortFunc(reasons, func(a, b string) int { return g.refusals[b] - g.refusals[a] })
	for _, r := range reasons {
		t.Logf("  refused %5d× %s", g.refusals[r], r)
	}
	for _, w := range g.wrong {
		t.Errorf("wrong answer: %s", w)
	}
}

// rustClient runs every Check through both evaluators.
type rustClient struct {
	*client
	gate *rustGate
}

func (c *rustClient) Check(ctx context.Context, in *openfgav1.CheckRequest, _ ...grpc.CallOption) (*openfgav1.CheckResponse, error) {
	var resp *openfgav1.CheckResponse
	err := c.do(ctx, func(s *atomicfga.Session) error {
		var err error
		resp, err, _ = c.gate.compare(ctx, s, in)
		return err
	})
	return resp, err
}

// TestRustUpstream holds the Rust evaluator to OpenFGA's Check assertion
// suites and generated Check matrices.
func TestRustUpstream(t *testing.T) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			db := v.open(t)
			gate := newRustGate(t, v.name)
			opts := func(experimentals ...string) []server.OpenFGAServiceV1Option {
				return []server.OpenFGAServiceV1Option{
					server.WithExperimentals(experimentals...),
					server.WithListObjectsDeadline(0),
					server.WithListUsersDeadline(0),
				}
			}
			c := &rustClient{newClient(t, db, v.pgx, opts(config.ExperimentalCheckOptimizations, config.ExperimentalInlineExpressions)...), gate}
			check.RunAllTests(t, c)
			for name, experimentals := range map[string][]string{
				"with optimizations":    {config.ExperimentalCheckOptimizations, config.ExperimentalInlineExpressions},
				"without optimizations": {config.ExperimentalInlineExpressions},
			} {
				c := &rustClient{newClient(t, db, v.pgx, opts(experimentals...)...), gate}
				check.RunMatrixTests(t, string(v.dialect), name, c)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Differential testing against random models.

const rustConditions = `
condition level_ok(level: int, required: int) {
  level >= required
}
condition str_ok(s: string) {
  s.startsWith("a") || s == "b"
}
condition list_ok(xs: list<string>, x: string) {
  x in xs
}
condition dur_ok(d: duration) {
  d < duration("1h")
}
condition div_ok(a: int, b: int) {
  a / b > 1
}
condition ip_ok(ip: ipaddress, cidr: string) {
  ip.in_cidr(cidr)
}
condition when_ok(now: timestamp, until: timestamp) {
  now < until
}
`

var (
	rustRestrictions = []string{
		"user", "user:*", "group#member", "user with level_ok", "user with str_ok", "user with list_ok",
		"group#member with level_ok", "user:* with dur_ok", "user with div_ok", "user with ip_ok", "user with when_ok",
	}
	rustConditionParams = map[string][]string{
		"level_ok": {"level", "required"},
		"str_ok":   {"s"},
		"list_ok":  {"xs", "x"},
		"dur_ok":   {"d"},
		"div_ok":   {"a", "b"},
		"ip_ok":    {"ip", "cidr"},
		"when_ok":  {"now", "until"},
	}
	rustObjects   = []string{"doc:1", "doc:2", "folder:a", "folder:b", "group:eng", "group:ops"}
	rustUsers     = []string{"user:anne", "user:bob", "user:*", "group:eng#member", "group:ops#member", "folder:a", "folder:b", "doc:1"}
	rustRelations = map[string][]string{
		"group":  {"member"},
		"folder": {"parent", "owner", "viewer"},
		"doc":    {"parent", "owner", "editor", "viewer", "r1", "r2"},
	}
	// Parameter values of the declared type, and of wrong types or spellings.
	rustGoodValues = map[string][]any{
		"level": {0.0, 1.0, 2.0, 3.0}, "required": {1.0, 2.0},
		"s":  {"a", "b", "abc", "c"},
		"xs": {[]any{"a", "b"}, []any{}}, "x": {"a", "c"},
		"d": {"30m", "2h", "1h", "59m59.5s"},
		"a": {4.0, 1.0, 0.0}, "b": {2.0, 0.0, 1.0},
		"ip": {"10.0.0.1", "::ffff:10.0.0.1", "192.168.1.1"}, "cidr": {"10.0.0.0/8", "::ffff:10.0.0.0/104", "192.168.0.0/16"},
		"now": {"2023-10-11T10:00:00Z", "2023-10-11T11:00:00+01:00", "2023-10-11T09:00:00.5Z"}, "until": {"2023-10-11T10:00:00Z", "2023-10-11T10:30:00+00:00"},
	}
	rustBadValues = map[string][]any{
		"level": {"2", 1.5, "x", true, 1e30}, "required": {"1", -1.0},
		"s": {5.0}, "xs": {"a"}, "x": {1.0},
		"d": {"bad", 5.0, "1d"}, "a": {"4"}, "b": {"0"},
		"ip": {"nope", "fe80::1%eth0", 5.0}, "cidr": {"10.0.0.0/33", "bad"},
		"now": {"bad", 5.0, "2023-10-11 10:00:00Z"}, "until": {"2023-10-11T10:00:00"},
	}
	rustContextKeys = slices.Sorted(maps.Keys(rustGoodValues))
)

// rustModel is a drawn model and the type restrictions of its directly
// assignable relations.
type rustModel struct {
	dsl          string
	restrictions map[string][]string // "type#relation" -> restrictions
}

func drawRestrictions(tc hegel.TestCase, choices []string) []string {
	var picked []string
	for range hegel.Draw(tc, hegel.Integers(1, 3)) {
		r := drawFrom(tc, choices)
		if !slices.Contains(picked, r) {
			picked = append(picked, r)
		}
	}
	return picked
}

func drawRustRewrite(tc hegel.TestCase, terms []string, depth int) string {
	if depth == 0 || hegel.Draw(tc, hegel.Integers(0, 2)) == 0 {
		return drawFrom(tc, terms)
	}
	a, b := drawRustRewrite(tc, terms, depth-1), drawRustRewrite(tc, terms, depth-1)
	return fmt.Sprintf("(%s %s %s)", a, drawFrom(tc, []string{"or", "and", "but not"}), b)
}

func drawRustModel(tc hegel.TestCase) rustModel {
	m := rustModel{restrictions: map[string][]string{
		"group#member":  drawRestrictions(tc, []string{"user", "user:*", "group#member", "user with level_ok", "user with list_ok"}),
		"folder#parent": {"folder"},
		"folder#owner":  drawRestrictions(tc, rustRestrictions),
		"folder#viewer": drawRestrictions(tc, rustRestrictions),
		"doc#parent":    {"folder"},
		"doc#owner":     drawRestrictions(tc, rustRestrictions),
		"doc#editor":    drawRestrictions(tc, rustRestrictions),
		"doc#viewer":    drawRestrictions(tc, rustRestrictions),
	}}
	r := func(k string) string { return "[" + strings.Join(m.restrictions[k], ", ") + "]" }
	folderViewer := r("folder#viewer") + drawFrom(tc, []string{"", " or viewer from parent", " or viewer from parent", " or owner", " but not owner"})
	terms := []string{"owner", "editor", "viewer", "viewer from parent", "owner from parent"}
	m.dsl = fmt.Sprintf(`model
  schema 1.1
type user
type group
  relations
    define member: %s
type folder
  relations
    define parent: [folder]
    define owner: %s
    define viewer: %s
type doc
  relations
    define parent: [folder]
    define owner: %s
    define editor: %s
    define viewer: %s
    define r1: %s
    define r2: %s
%s`, r("group#member"), r("folder#owner"), folderViewer, r("doc#owner"), r("doc#editor"), r("doc#viewer"),
		drawRustRewrite(tc, terms, 3), drawRustRewrite(tc, append(terms, "r1"), 2), rustConditions)
	return m
}

func drawRustValue(tc hegel.TestCase, key string) any {
	if hegel.Draw(tc, hegel.Integers(0, 4)) == 0 {
		return drawFrom(tc, rustBadValues[key])
	}
	return drawFrom(tc, rustGoodValues[key])
}

// drawRustContext draws a context with some of the given parameters.
func drawRustContext(tc hegel.TestCase, keys []string) *structpb.Struct {
	if hegel.Draw(tc, hegel.Integers(0, 4)) == 0 {
		return nil
	}
	fields := map[string]any{}
	for _, k := range keys {
		if hegel.Draw(tc, hegel.Integers(0, 3)) > 0 {
			fields[k] = drawRustValue(tc, k)
		}
	}
	switch hegel.Draw(tc, hegel.Integers(0, 12)) {
	case 0:
		fields["unknown_key"] = "x"
	case 1:
		fields["control\x01key"] = "x" // forbidden characters
	case 2:
		fields["s"] = []any{"nested\x02value"}
	}
	s, err := structpb.NewStruct(fields)
	if err != nil {
		panic(err)
	}
	return s
}

// drawRustTuple draws a tuple that is valid for m, or (sometimes) one that
// is not.
func drawRustTuple(tc hegel.TestCase, m rustModel) *openfgav1.TupleKey {
	if hegel.Draw(tc, hegel.Integers(0, 5)) == 0 {
		object := drawFrom(tc, rustObjects)
		t := tk(object, drawFrom(tc, rustRelations[strings.Split(object, ":")[0]]), drawFrom(tc, rustUsers))
		if hegel.Draw(tc, hegel.Booleans()) {
			name := drawFrom(tc, append(slices.Sorted(maps.Keys(rustConditionParams)), "undefined_cond"))
			t.Condition = &openfgav1.RelationshipCondition{Name: name, Context: drawRustContext(tc, rustConditionParams[name])}
		}
		return t
	}
	key := drawFrom(tc, slices.Sorted(maps.Keys(m.restrictions)))
	typ, relation, _ := strings.Cut(key, "#")
	object := drawFrom(tc, slices.DeleteFunc(slices.Clone(rustObjects), func(o string) bool { return !strings.HasPrefix(o, typ+":") }))
	restriction := drawFrom(tc, m.restrictions[key])
	subject, cond, _ := strings.Cut(restriction, " with ")
	var user string
	switch subject {
	case "user":
		user = drawFrom(tc, []string{"user:anne", "user:bob"})
	case "user:*":
		user = "user:*"
	case "group#member":
		user = drawFrom(tc, []string{"group:eng#member", "group:ops#member"})
	case "folder":
		user = drawFrom(tc, []string{"folder:a", "folder:b"})
	}
	t := tk(object, relation, user)
	if cond != "" {
		t.Condition = &openfgav1.RelationshipCondition{Name: cond, Context: drawRustContext(tc, rustConditionParams[cond])}
	}
	return t
}

func drawRustCheck(tc hegel.TestCase, m rustModel, store, model string) *openfgav1.CheckRequest {
	object := drawFrom(tc, rustObjects)
	if hegel.Draw(tc, hegel.Integers(0, 9)) == 0 {
		object = drawFrom(tc, []string{"doc:3", "doc:*", "unknown:1"})
	}
	relations := rustRelations[strings.Split(object, ":")[0]]
	if relations == nil || hegel.Draw(tc, hegel.Integers(0, 19)) == 0 {
		relations = []string{"nope", "member"}
	}
	user := drawFrom(tc, []string{"user:anne", "user:bob", "user:carl", "user:*", "group:eng#member", "group:ops#member", "folder:a"})
	if hegel.Draw(tc, hegel.Integers(0, 9)) == 0 {
		user = drawFrom(tc, []string{"anne", "unknown:x", "group:eng#nope", "doc:1#viewer", "folder:b#viewer"})
	}
	req := &openfgav1.CheckRequest{
		StoreId:              store,
		AuthorizationModelId: model,
		TupleKey:             &openfgav1.CheckRequestTupleKey{Object: object, Relation: drawFrom(tc, relations), User: user},
		Context:              drawRustContext(tc, rustContextKeys),
	}
	if n := hegel.Draw(tc, hegel.Integers(-2, 3)); n > 0 {
		req.ContextualTuples = &openfgav1.ContextualTupleKeys{}
		for range n {
			req.ContextualTuples.TupleKeys = append(req.ContextualTuples.TupleKeys, drawRustTuple(tc, m))
		}
	}
	return req
}

// TestHegelRustCheck compares the Rust evaluator with OpenFGA's Check on
// random models, tuples (written under one model version and checked under
// another as well), contextual tuples and request contexts, all inside one
// transaction.
func TestHegelRustCheck(t *testing.T) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			db := v.open(t)
			engine, err := atomicfga.New(db.Dialect)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(engine.Close)
			gate := newRustGate(t, v.name)
			hegel.Test(t, func(ht *hegel.T) {
				ctx := context.Background()
				tx, _, rollback, err := db.BeginTx(ctx, v.pgx)
				if err != nil {
					ht.Fatalf("begin: %v", err)
				}
				defer func() { _ = rollback() }()
				s := engine.Session(tx)
				st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "rust"})
				if err != nil {
					ht.Fatalf("create store: %v", err)
				}
				var models []string
				var drawn []rustModel
				for range 2 {
					rm := drawRustModel(ht)
					m, err := parser.TransformDSLToProto(rm.dsl)
					if err != nil {
						ht.Fatalf("model: %v", err)
					}
					resp, err := s.WriteAuthorizationModel(ctx, &openfgav1.WriteAuthorizationModelRequest{
						StoreId: st.GetId(), SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions(), Conditions: m.GetConditions(),
					})
					if err != nil {
						continue // an invalid random model
					}
					models = append(models, resp.GetAuthorizationModelId())
					drawn = append(drawn, rm)
				}
				if len(models) == 0 {
					return
				}
				// Structures random tuples rarely form: parent cycles (cycle
				// detection) and parent chains around the resolution depth limit.
				write := func(t *openfgav1.TupleKey) {
					_, _ = s.Write(ctx, &openfgav1.WriteRequest{
						StoreId: st.GetId(), AuthorizationModelId: models[0],
						Writes: &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{t}},
					})
				}
				chain, cycle := 0, false
				switch hegel.Draw(ht, hegel.Integers(0, 3)) {
				case 0:
					cycle = true
					write(tk("doc:1", "parent", "folder:a"))
					write(tk("doc:1", "owner", "group:eng#member"))
					write(tk("folder:a", "parent", "folder:b"))
					write(tk("folder:b", "parent", "folder:a"))
					write(tk("group:eng", "member", "group:ops#member"))
					write(tk("group:ops", "member", "group:eng#member"))
				case 1:
					chain = hegel.Draw(ht, hegel.Integers(20, 28))
					for i := range chain - 1 {
						write(tk(fmt.Sprintf("folder:c%d", i), "parent", fmt.Sprintf("folder:c%d", i+1)))
					}
					for _, u := range []string{"user:anne", "group:eng#member"} {
						write(tk(fmt.Sprintf("folder:c%d", chain-1), "viewer", u))
						write(tk(fmt.Sprintf("folder:c%d", chain-1), "owner", u))
					}
					write(tk("doc:1", "parent", "folder:c0"))
				}
				for range hegel.Draw(ht, hegel.Integers(0, 20)) {
					// One write per tuple: tuples invalid for the model are rejected.
					_, _ = s.Write(ctx, &openfgav1.WriteRequest{
						StoreId: st.GetId(), AuthorizationModelId: models[0],
						Writes: &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{drawRustTuple(ht, drawn[0])}},
					})
				}
				for range hegel.Draw(ht, hegel.Integers(1, 8)) {
					i := hegel.Draw(ht, hegel.Integers(0, len(models)))
					model, rm := "", drawn[len(drawn)-1] // "" is the latest
					if i < len(models) {
						model, rm = models[i], drawn[i]
					}
					req := drawRustCheck(ht, rm, st.GetId(), model)
					if cycle && hegel.Draw(ht, hegel.Booleans()) {
						// Userset users are always resolved by the default
						// strategy, which reports cycles.
						req.TupleKey = &openfgav1.CheckRequestTupleKey{Object: "doc:1", Relation: drawFrom(ht, []string{"r1", "r2"}), User: "group:eng#member"}
					}
					if chain > 0 && hegel.Draw(ht, hegel.Booleans()) {
						req.TupleKey.Object = fmt.Sprintf("folder:c%d", hegel.Draw(ht, hegel.Integers(0, 3)))
						req.TupleKey.Relation = drawFrom(ht, []string{"viewer", "owner"})
						req.TupleKey.User = drawFrom(ht, []string{"user:anne", "group:eng#member"})
					}
					if _, _, wrong := gate.compare(ctx, s, req); wrong != "" {
						ht.Fatalf("wrong answer: %s", wrong)
					}
				}
			}, hegel.WithTestCases(hegelCases()*5), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch))
		})
	}
}

// TestRustEdgeCases pins scenarios that random generation reaches rarely,
// each compared with OpenFGA's Check like every other gate.
func TestRustEdgeCases(t *testing.T) {
	const model = `model
  schema 1.1
type user
type group
  relations
    define member: [user, group#member]
type folder
  relations
    define parent: [folder]
    define viewer: [user, group#member] or viewer from parent
type doc
  relations
    define parent: [folder]
    define owner: [user, group#member]
    define cyclic_subtrahend: owner but not viewer from parent
    define cyclic_operand: owner and viewer from parent
`
	tuples := []*openfgav1.TupleKey{
		tk("folder:a", "parent", "folder:b"), tk("folder:b", "parent", "folder:a"),
		tk("doc:1", "parent", "folder:a"), tk("doc:1", "owner", "group:eng#member"), tk("doc:1", "owner", "user:anne"),
		tk("group:eng", "member", "group:ops#member"), tk("group:ops", "member", "group:eng#member"),
	}
	for i := range 30 {
		tuples = append(tuples, tk(fmt.Sprintf("folder:c%d", i), "parent", fmt.Sprintf("folder:c%d", i+1)))
	}
	tuples = append(tuples, tk("folder:c30", "viewer", "group:eng#member"), tk("folder:c30", "viewer", "user:anne"))
	var checks []*openfgav1.CheckRequestTupleKey
	for _, user := range []string{"group:eng#member", "user:anne", "group:ops#member"} {
		for _, rel := range []string{"cyclic_subtrahend", "cyclic_operand", "owner"} {
			checks = append(checks, &openfgav1.CheckRequestTupleKey{Object: "doc:1", Relation: rel, User: user})
		}
		for _, depth := range []int{0, 4, 5, 6, 7, 8} { // chain lengths around the depth limit
			checks = append(checks, &openfgav1.CheckRequestTupleKey{Object: fmt.Sprintf("folder:c%d", depth), Relation: "viewer", User: user})
		}
		checks = append(checks, &openfgav1.CheckRequestTupleKey{Object: "group:eng", Relation: "member", User: user})
	}
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			db := v.open(t)
			engine, err := atomicfga.New(db.Dialect)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(engine.Close)
			gate := newRustGate(t, v.name)
			ctx := context.Background()
			tx, _, rollback, err := db.BeginTx(ctx, v.pgx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rollback() }()
			s := engine.Session(tx)
			st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "edges"})
			if err != nil {
				t.Fatal(err)
			}
			m := parser.MustTransformDSLToProto(model)
			if _, err := s.WriteAuthorizationModel(ctx, &openfgav1.WriteAuthorizationModelRequest{StoreId: st.GetId(), SchemaVersion: "1.1", TypeDefinitions: m.GetTypeDefinitions()}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Write(ctx, &openfgav1.WriteRequest{StoreId: st.GetId(), Writes: &openfgav1.WriteRequestWrites{TupleKeys: tuples}}); err != nil {
				t.Fatal(err)
			}
			for _, c := range checks {
				resp, err, _ := gate.compare(ctx, s, &openfgav1.CheckRequest{StoreId: st.GetId(), TupleKey: c})
				code, _ := checkOutcome(resp, err)
				t.Logf("%s#%s@%s: openfga %s", c.GetObject(), c.GetRelation(), c.GetUser(), code)
			}
		})
	}
}

// TestRustConfiguredLimits holds the Rust evaluator to OpenFGA's Check under
// configured resolve node limits, on chains of every length around each, and
// requires it to refuse under settings it does not follow.
func TestRustConfiguredLimits(t *testing.T) {
	const model = `model
  schema 1.1
type user
type group2
  relations
    define member: [user]
type group
  relations
    define member: [group2#member]
type folder
  relations
    define parent: [folder]
    define viewer: [user] or viewer from parent
type doc
  relations
    define viewer: [group#member]
`
	const chain = 12
	tuples := []*openfgav1.TupleKey{
		tk("doc:1", "viewer", "group:g#member"), tk("group:g", "member", "group2:g#member"), tk("group2:g", "member", "user:anne"),
		tk(fmt.Sprintf("folder:c%d", chain), "viewer", "user:anne"),
	}
	for i := range chain {
		tuples = append(tuples, tk(fmt.Sprintf("folder:c%d", i), "parent", fmt.Sprintf("folder:c%d", i+1)))
	}
	checks := []*openfgav1.CheckRequestTupleKey{{Object: "doc:1", Relation: "viewer", User: "user:anne"}}
	for i := range chain + 1 {
		for _, user := range []string{"user:anne", "user:bob"} {
			checks = append(checks, &openfgav1.CheckRequestTupleKey{Object: fmt.Sprintf("folder:c%d", i), Relation: "viewer", User: user})
		}
	}
	setup := func(t *testing.T, v variant, opts ...server.OpenFGAServiceV1Option) (*atomicfga.Session, string) {
		db := v.open(t)
		engine, err := atomicfga.New(db.Dialect, atomicfga.WithServerOptions(opts...))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(engine.Close)
		ctx := context.Background()
		tx, _, rollback, err := db.BeginTx(ctx, v.pgx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = rollback() })
		s := engine.Session(tx)
		st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "limits"})
		if err != nil {
			t.Fatal(err)
		}
		m := parser.MustTransformDSLToProto(model)
		if _, err := s.WriteAuthorizationModel(ctx, &openfgav1.WriteAuthorizationModelRequest{StoreId: st.GetId(), SchemaVersion: "1.1", TypeDefinitions: m.GetTypeDefinitions()}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Write(ctx, &openfgav1.WriteRequest{StoreId: st.GetId(), Writes: &openfgav1.WriteRequestWrites{TupleKeys: tuples}}); err != nil {
			t.Fatal(err)
		}
		return s, st.GetId()
	}
	ctx := context.Background()
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			for _, limit := range []uint32{1, 2, 3, 4, 5, 6, 8, 10, 25} {
				t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
					s, store := setup(t, v, server.WithResolveNodeLimit(limit))
					gate := newRustGate(t, fmt.Sprintf("%s, limit %d", v.name, limit))
					outcomes := map[string]bool{}
					for _, c := range checks {
						resp, err, _ := gate.compare(ctx, s, &openfgav1.CheckRequest{StoreId: store, TupleKey: c})
						code, _ := checkOutcome(resp, err)
						outcomes[code] = true
					}
					if gate.answered == 0 {
						t.Error("the Rust evaluator refused every check")
					}
					if limit < chain && !outcomes[fmt.Sprintf("error %d", openfgav1.ErrorCode_authorization_model_resolution_too_complex)] {
						t.Errorf("no check reached the limit: outcomes %v", outcomes)
					}
				})
			}
			for name, opts := range map[string][]server.OpenFGAServiceV1Option{
				"weighted graph check": {server.WithExperimentals(config.ExperimentalWeightedGraphCheck)},
			} {
				t.Run(name, func(t *testing.T) {
					s, store := setup(t, v, opts...)
					_, err := s.RustCheckStrict(ctx, &openfgav1.CheckRequest{StoreId: store, TupleKey: checks[0]})
					if !errors.Is(err, atomicfga.ErrRustRefused) {
						t.Errorf("RustCheckStrict: %v, want a refusal", err)
					}
				})
			}
		})
	}
}
