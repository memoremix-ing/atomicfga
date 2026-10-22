//go:build rustcheck

package tests

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// compileGate tallies the Rust model compiler against OpenFGA's
// WriteAuthorizationModel.
type compileGate struct {
	mu        sync.Mutex
	models    int
	decided   int
	verdicts  map[string]int // of decided models
	reasons   map[string]int // OpenFGA's rejections, names masked
	undecided map[string]int
	wrong     []string
}

func (g *compileGate) report(t *testing.T) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t.Logf("%d models: %d decided by Rust (%d wrong), %d undecided; decided verdicts %v",
		g.models, g.decided, len(g.wrong), g.models-g.decided, g.verdicts)
	reasons := slices.Collect(maps.Keys(g.undecided))
	slices.SortFunc(reasons, func(a, b string) int { return g.undecided[b] - g.undecided[a] })
	for _, r := range reasons[:min(len(reasons), 12)] {
		t.Logf("  undecided %5d× %s", g.undecided[r], r)
	}
	rejections := slices.Collect(maps.Keys(g.reasons))
	slices.SortFunc(rejections, func(a, b string) int { return g.reasons[b] - g.reasons[a] })
	for _, r := range rejections[:min(len(rejections), 25)] {
		t.Logf("  rejected  %5d× %.110s", g.reasons[r], r)
	}
	for _, w := range g.wrong {
		t.Errorf("wrong verdict: %s", w)
	}
}

// compare writes req through OpenFGA (in a savepoint of the session's
// transaction) and asks the Rust compiler.
func (g *compileGate) compare(ctx context.Context, s *atomicfga.Session, req *openfgav1.WriteAuthorizationModelRequest) string {
	_, werr := s.WriteAuthorizationModel(ctx, req)
	gerr := atomicfga.RustCompileModel(req, atomicfga.CompileOptions{})
	g.mu.Lock()
	defer g.mu.Unlock()
	g.models++
	if errors.Is(gerr, atomicfga.ErrRustRefused) {
		reason := strings.TrimPrefix(gerr.Error(), atomicfga.ErrRustRefused.Error()+": ")
		g.undecided[reason]++
		return ""
	}
	g.decided++
	want, wm := checkOutcome(nil, werr)
	got, gm := checkOutcome(nil, gerr)
	if werr == nil {
		want = "valid"
	}
	if gerr == nil {
		got = "valid"
	}
	g.verdicts[want]++
	if werr != nil {
		g.reasons[maskNames.ReplaceAllString(wm, "_")]++
	}
	if want != got {
		d := fmt.Sprintf("%v\n  openfga: %s %s\n  rust:    %s %s", req, want, wm, got, gm)
		g.wrong = append(g.wrong, d)
		return d
	}
	return ""
}

// maskNames masks quoted names and numbers in error messages.
var maskNames = regexp.MustCompile(`'[^']*'|"[^"]*"|\[[^\]]*\]|\b[0-9]+\b`)

var (
	compileTypes     = []string{"user", "group", "doc", "folder"}
	compileBadTypes  = []string{"", "self", "this", "a b", "x:y", "user", strings.Repeat("t", 255), "émoji✓", strings.Repeat("ü", 254), strings.Repeat("ü", 255)}
	compileRelations = []string{"viewer", "editor", "owner", "parent", "member", "r1"}
	compileBadRels   = []string{"", "this", "self", "a#b", strings.Repeat("r", 51), "nope", "vièwer", strings.Repeat("é", 50), strings.Repeat("é", 51)}
	compileExprs     = []string{
		"x < 10", "x < y", "x + 1", "s == 'a'", "s == \"b\" || x > 2", "size(s) > 2 && s.startsWith('a')",
		"ts < timestamp('2023-01-01T00:00:00Z')", "timestamp('bad') < ts", "1 / 0 == 0", "x == 1.0", "x > 0u",
		"s in list", "list.exists(e, e == s)", "", "x <", "undeclared > 1", "true", "!(x > 1) ? true : x < 3",
		"d < duration('1h')", "ip.in_cidr('10.0.0.0/8')", "x - 9223372036854775807 - 2 < 0", "s + 1 == s",
		strings.Repeat("x < 1 && ", 60) + "true",
	}
	compileParams     = []string{"x", "y", "s", "list", "ts", "d", "ip", "in", "a-b"}
	compileParamTypes = []openfgav1.ConditionParamTypeRef_TypeName{
		openfgav1.ConditionParamTypeRef_TYPE_NAME_INT, openfgav1.ConditionParamTypeRef_TYPE_NAME_INT,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_STRING, openfgav1.ConditionParamTypeRef_TYPE_NAME_UINT,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_DOUBLE, openfgav1.ConditionParamTypeRef_TYPE_NAME_BOOL,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_TIMESTAMP, openfgav1.ConditionParamTypeRef_TYPE_NAME_DURATION,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_IPADDRESS, openfgav1.ConditionParamTypeRef_TYPE_NAME_LIST,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_MAP, openfgav1.ConditionParamTypeRef_TYPE_NAME_ANY,
		openfgav1.ConditionParamTypeRef_TYPE_NAME_UNSPECIFIED,
	}
	compileSchemas = []string{"1.1", "1.1", "1.1", "1.1", "1.2", "1.0", "", "2.0"}
)

// rare returns true with probability 1/n.
func rare(tc hegel.TestCase, n int) bool { return hegel.Draw(tc, hegel.Integers(1, n)) == 1 }

// chaos is how malformed a drawn model may be: 0 well-formed protobuf with
// references to defined types and relations, 1 also semantic faults
// (undefined references, invalid tuplesets, missing type restrictions), 2
// also protobuf faults (bad names, missing required fields).
var chaos int

// faultRate scales fault probabilities down, so that a model often carries
// a single fault: one that no other check would also reject.
var faultRate int

func faulty(tc hegel.TestCase, level, n int) bool { return chaos >= level && rare(tc, n*faultRate) }

func drawName(tc hegel.TestCase, good, bad []string) string {
	if faulty(tc, 2, 12) {
		return drawFrom(tc, bad)
	}
	return drawFrom(tc, good)
}

// shape is the types and relation names of a model being drawn.
type shape struct {
	types     []string
	relations map[string][]string // type -> relation names
}

func (sh shape) anyRelation(tc hegel.TestCase) string {
	var all []string
	for _, t := range sh.types {
		all = append(all, sh.relations[t]...)
	}
	if len(all) == 0 || faulty(tc, 1, 8) {
		return drawName(tc, compileRelations, compileBadRels)
	}
	return drawFrom(tc, all)
}

func drawUserset(tc hegel.TestCase, sh shape, own []string, depth int) *openfgav1.Userset {
	relation := func() string {
		if len(own) > 0 && !faulty(tc, 1, 6) {
			return drawFrom(tc, own)
		}
		return drawName(tc, compileRelations, compileBadRels)
	}
	leaf := depth == 0 || hegel.Draw(tc, hegel.Integers(0, 2)) > 0
	switch k := hegel.Draw(tc, hegel.Integers(0, 9)); {
	case faulty(tc, 1, 40):
		return &openfgav1.Userset{}
	case leaf && k < 5:
		return &openfgav1.Userset{Userset: &openfgav1.Userset_This{This: &openfgav1.DirectUserset{}}}
	case leaf && k < 7:
		return &openfgav1.Userset{Userset: &openfgav1.Userset_ComputedUserset{ComputedUserset: &openfgav1.ObjectRelation{Relation: relation()}}}
	case leaf:
		ttu := &openfgav1.TupleToUserset{
			Tupleset:        &openfgav1.ObjectRelation{Relation: relation()},
			ComputedUserset: &openfgav1.ObjectRelation{Relation: sh.anyRelation(tc)},
		}
		if faulty(tc, 2, 40) {
			ttu.Tupleset = nil
		}
		return &openfgav1.Userset{Userset: &openfgav1.Userset_TupleToUserset{TupleToUserset: ttu}}
	case k < 7:
		n := 2
		if faulty(tc, 1, 8) {
			n = hegel.Draw(tc, hegel.Integers(0, 3))
		} else if rare(tc, 4) {
			n = 3
		}
		var children []*openfgav1.Userset
		for range n {
			children = append(children, drawUserset(tc, sh, own, depth-1))
		}
		if hegel.Draw(tc, hegel.Booleans()) {
			return &openfgav1.Userset{Userset: &openfgav1.Userset_Union{Union: &openfgav1.Usersets{Child: children}}}
		}
		return &openfgav1.Userset{Userset: &openfgav1.Userset_Intersection{Intersection: &openfgav1.Usersets{Child: children}}}
	default:
		d := &openfgav1.Difference{Base: drawUserset(tc, sh, own, depth-1), Subtract: drawUserset(tc, sh, own, depth-1)}
		if faulty(tc, 2, 40) {
			d.Base = nil
		}
		return &openfgav1.Userset{Userset: &openfgav1.Userset_Difference{Difference: d}}
	}
}

func containsThis(u *openfgav1.Userset) bool {
	switch x := u.GetUserset().(type) {
	case *openfgav1.Userset_This:
		return true
	case *openfgav1.Userset_Union:
		return slices.ContainsFunc(x.Union.GetChild(), containsThis)
	case *openfgav1.Userset_Intersection:
		return slices.ContainsFunc(x.Intersection.GetChild(), containsThis)
	case *openfgav1.Userset_Difference:
		return containsThis(x.Difference.GetBase()) || containsThis(x.Difference.GetSubtract())
	}
	return false
}

func drawRelationReferences(tc hegel.TestCase, sh shape, conditions []string) []*openfgav1.RelationReference {
	var refs []*openfgav1.RelationReference
	for range hegel.Draw(tc, hegel.Integers(1, 3)) {
		ref := &openfgav1.RelationReference{Type: drawFrom(tc, sh.types)}
		if faulty(tc, 1, 15) {
			ref.Type = drawName(tc, []string{"nope"}, compileBadTypes)
		}
		switch hegel.Draw(tc, hegel.Integers(0, 5)) {
		case 0:
			ref.RelationOrWildcard = &openfgav1.RelationReference_Wildcard{Wildcard: &openfgav1.Wildcard{}}
		case 1, 2:
			rel := ""
			if rels := sh.relations[ref.Type]; len(rels) > 0 && !faulty(tc, 1, 6) {
				rel = drawFrom(tc, rels)
			} else {
				rel = drawName(tc, compileRelations, compileBadRels)
			}
			ref.RelationOrWildcard = &openfgav1.RelationReference_Relation{Relation: rel}
		}
		if len(conditions) > 0 && rare(tc, 3) {
			ref.Condition = drawFrom(tc, conditions)
		}
		if faulty(tc, 1, 12) {
			ref.Condition = drawFrom(tc, []string{"nope", "$expression"})
		}
		refs = append(refs, ref)
	}
	return refs
}

func drawParamType(tc hegel.TestCase, depth int) *openfgav1.ConditionParamTypeRef {
	p := &openfgav1.ConditionParamTypeRef{TypeName: drawFrom(tc, compileParamTypes)}
	generics := 0
	if p.GetTypeName() == openfgav1.ConditionParamTypeRef_TYPE_NAME_LIST || p.GetTypeName() == openfgav1.ConditionParamTypeRef_TYPE_NAME_MAP {
		generics = 1
	}
	if faulty(tc, 1, 10) {
		generics = hegel.Draw(tc, hegel.Integers(0, 2))
	}
	if depth > 0 {
		for range generics {
			p.GenericTypes = append(p.GenericTypes, drawParamType(tc, depth-1))
		}
	}
	return p
}

func scalar(t openfgav1.ConditionParamTypeRef_TypeName) *openfgav1.ConditionParamTypeRef {
	return &openfgav1.ConditionParamTypeRef{TypeName: t}
}

// wellFormedConditions are conditions OpenFGA compiles.
var wellFormedConditions = []*openfgav1.Condition{
	{Expression: "x < 10", Parameters: map[string]*openfgav1.ConditionParamTypeRef{"x": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_INT)}},
	{Expression: "s.startsWith('a') || s == \"b\"", Parameters: map[string]*openfgav1.ConditionParamTypeRef{"s": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_STRING)}},
	{Expression: "ts < timestamp('2023-01-01T00:00:00Z') && d > duration('1m')", Parameters: map[string]*openfgav1.ConditionParamTypeRef{
		"ts": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_TIMESTAMP), "d": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_DURATION)}},
	{Expression: "ip.in_cidr(cidr)", Parameters: map[string]*openfgav1.ConditionParamTypeRef{
		"ip": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_IPADDRESS), "cidr": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_STRING)}},
	{Expression: "x > 0u ? true : x == 0u", Parameters: map[string]*openfgav1.ConditionParamTypeRef{"x": scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_UINT)}},
	{Expression: "true", Parameters: map[string]*openfgav1.ConditionParamTypeRef{"xs": {TypeName: openfgav1.ConditionParamTypeRef_TYPE_NAME_LIST,
		GenericTypes: []*openfgav1.ConditionParamTypeRef{scalar(openfgav1.ConditionParamTypeRef_TYPE_NAME_STRING)}}}},
}

func drawCondition(tc hegel.TestCase, key string) *openfgav1.Condition {
	if chaos == 0 || !rare(tc, 2) {
		c := drawFrom(tc, wellFormedConditions)
		name := key
		if faulty(tc, 1, 6) {
			name = "other"
		}
		return &openfgav1.Condition{Name: name, Expression: c.GetExpression(), Parameters: maps.Clone(c.GetParameters())}
	}
	c := &openfgav1.Condition{Name: key, Expression: drawFrom(tc, compileExprs), Parameters: map[string]*openfgav1.ConditionParamTypeRef{}}
	if faulty(tc, 1, 10) {
		c.Name = "other"
	}
	for range hegel.Draw(tc, hegel.Integers(0, 3)) {
		c.Parameters[drawFrom(tc, compileParams)] = drawParamType(tc, 2)
	}
	return c
}

func drawCompileModel(tc hegel.TestCase) *openfgav1.WriteAuthorizationModelRequest {
	chaos = hegel.Draw(tc, hegel.Integers(0, 2))
	faultRate = drawFrom(tc, []int{1, 4, 16})
	req := &openfgav1.WriteAuthorizationModelRequest{SchemaVersion: drawFrom(tc, []string{"1.1", "1.1", "1.2"})}
	if faulty(tc, 2, 4) {
		req.SchemaVersion = drawFrom(tc, compileSchemas)
	}
	sh := shape{relations: map[string][]string{}}
	for _, t := range compileTypes {
		if t == "user" || hegel.Draw(tc, hegel.Integers(0, 3)) > 0 {
			sh.types = append(sh.types, t)
		}
	}
	if faulty(tc, 2, 6) {
		sh.types = append(sh.types, drawFrom(tc, compileBadTypes))
	}
	if faulty(tc, 1, 30) {
		for i := range 100 {
			sh.types = append(sh.types, fmt.Sprintf("t%d", i))
		}
	}
	for _, t := range sh.types {
		if t == "user" {
			continue
		}
		for range hegel.Draw(tc, hegel.Integers(0, 4)) {
			r := drawName(tc, compileRelations, compileBadRels)
			if !slices.Contains(sh.relations[t], r) {
				sh.relations[t] = append(sh.relations[t], r)
			}
		}
	}
	var conditions []string
	if n := hegel.Draw(tc, hegel.Integers(-1, 2)); n > 0 {
		req.Conditions = map[string]*openfgav1.Condition{}
		for i := range n {
			key := fmt.Sprintf("c%d", i+1)
			if faulty(tc, 2, 10) {
				key = "c 3"
			}
			req.Conditions[key] = drawCondition(tc, key)
			conditions = append(conditions, key)
		}
	}
	for _, t := range sh.types {
		td := &openfgav1.TypeDefinition{Type: t}
		own := sh.relations[t]
		if len(own) > 0 {
			td.Relations = map[string]*openfgav1.Userset{}
			td.Metadata = &openfgav1.Metadata{Relations: map[string]*openfgav1.RelationMetadata{}}
		}
		for _, r := range own {
			u := drawUserset(tc, sh, own, 2)
			td.Relations[r] = u
			// Type restrictions belong on (and only on) relations that are
			// directly assignable.
			if containsThis(u) != faulty(tc, 1, 10) {
				td.Metadata.Relations[r] = &openfgav1.RelationMetadata{DirectlyRelatedUserTypes: drawRelationReferences(tc, sh, conditions)}
			}
		}
		if td.Metadata != nil && faulty(tc, 2, 30) {
			td.Metadata.Module = drawFrom(tc, []string{"core", "bad module"})
		}
		req.TypeDefinitions = append(req.TypeDefinitions, td)
	}
	return req
}

// TestHegelRustCompileModel compares the Rust model compiler's verdict with
// OpenFGA's WriteAuthorizationModel on random, often malformed, models.
func TestHegelRustCompileModel(t *testing.T) {
	db := testdb.Open(t, atomicfga.SQLite)
	engine, err := atomicfga.New(db.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	gate := &compileGate{verdicts: map[string]int{}, reasons: map[string]int{}, undecided: map[string]int{}}
	t.Cleanup(func() { gate.report(t) })
	ctx := context.Background()
	hegel.Test(t, func(ht *hegel.T) {
		tx, _, rollback, err := db.BeginTx(ctx, false)
		if err != nil {
			ht.Fatalf("begin: %v", err)
		}
		defer func() { _ = rollback() }()
		s := engine.Session(tx)
		st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "compile"})
		if err != nil {
			ht.Fatalf("create store: %v", err)
		}
		for range hegel.Draw(ht, hegel.Integers(1, 4)) {
			req := drawCompileModel(ht)
			req.StoreId = st.GetId()
			if wrong := gate.compare(ctx, s, req); wrong != "" {
				ht.Fatalf("wrong verdict: %s", wrong)
			}
		}
	}, hegel.WithTestCases(hegelCases()*25), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch))
}

// TestRustCompileEdgeCases pins models that random generation reaches
// rarely, each compared with OpenFGA like every other model.
func TestRustCompileEdgeCases(t *testing.T) {
	this := &openfgav1.Userset{Userset: &openfgav1.Userset_This{This: &openfgav1.DirectUserset{}}}
	computed := func(r string) *openfgav1.Userset {
		return &openfgav1.Userset{Userset: &openfgav1.Userset_ComputedUserset{ComputedUserset: &openfgav1.ObjectRelation{Relation: r}}}
	}
	union := func(c ...*openfgav1.Userset) *openfgav1.Userset {
		return &openfgav1.Userset{Userset: &openfgav1.Userset_Union{Union: &openfgav1.Usersets{Child: c}}}
	}
	intersection := func(c ...*openfgav1.Userset) *openfgav1.Userset {
		return &openfgav1.Userset{Userset: &openfgav1.Userset_Intersection{Intersection: &openfgav1.Usersets{Child: c}}}
	}
	doc := func(relations map[string]*openfgav1.Userset) *openfgav1.WriteAuthorizationModelRequest {
		md := map[string]*openfgav1.RelationMetadata{}
		for r, u := range relations {
			if containsThis(u) {
				md[r] = &openfgav1.RelationMetadata{DirectlyRelatedUserTypes: []*openfgav1.RelationReference{{Type: "user"}}}
			}
		}
		return &openfgav1.WriteAuthorizationModelRequest{SchemaVersion: "1.1", TypeDefinitions: []*openfgav1.TypeDefinition{
			{Type: "user"}, {Type: "doc", Relations: relations, Metadata: &openfgav1.Metadata{Relations: md}},
		}}
	}
	models := map[string]*openfgav1.WriteAuthorizationModelRequest{
		"union of one":                     doc(map[string]*openfgav1.Userset{"viewer": union(this)}),
		"union of none":                    doc(map[string]*openfgav1.Userset{"viewer": union()}),
		"intersection of one":              doc(map[string]*openfgav1.Userset{"viewer": intersection(this)}),
		"union of one after an entrypoint": doc(map[string]*openfgav1.Userset{"viewer": union(this, union(computed("owner"))), "owner": this}),
		"union of one in a dead operand":   doc(map[string]*openfgav1.Userset{"viewer": intersection(computed("viewer"), union(this)), "owner": this}),
		"relation name of 50 characters":   doc(map[string]*openfgav1.Userset{strings.Repeat("é", 50): this}),
		"relation name of 51 characters":   doc(map[string]*openfgav1.Userset{strings.Repeat("é", 51): this}),
	}
	// OpenFGA rejects this model although every relation has an entrypoint:
	// hasEntrypoints clones its visited map shallowly, so marks made while
	// exploring one operand leak into its siblings.
	b, err := os.ReadFile("testdata/entrypoints-aliasing.json")
	if err != nil {
		t.Fatal(err)
	}
	aliasing := &openfgav1.WriteAuthorizationModelRequest{}
	if err := protojson.Unmarshal(b, aliasing); err != nil {
		t.Fatal(err)
	}
	models["entrypoints through shared visited marks"] = aliasing

	db := testdb.Open(t, atomicfga.SQLite)
	engine, err := atomicfga.New(db.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	gate := &compileGate{verdicts: map[string]int{}, reasons: map[string]int{}, undecided: map[string]int{}}
	t.Cleanup(func() { gate.report(t) })
	ctx := context.Background()
	tx, _, rollback, err := db.BeginTx(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rollback() }()
	s := engine.Session(tx)
	st, err := s.CreateStore(ctx, &openfgav1.CreateStoreRequest{Name: "edges"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(models)) {
		req := models[name]
		req.StoreId = st.GetId()
		gate.compare(ctx, s, req)
		_, err := s.WriteAuthorizationModel(ctx, req)
		code, _ := checkOutcome(nil, err)
		if err == nil {
			code = "valid"
		}
		t.Logf("%s: openfga %s", name, code)
	}
}
