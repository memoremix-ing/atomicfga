package bench

import (
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/tuple"
)

const benchDSL = `model
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
    define owner: [user]
    define editor: [user, group#member] or owner
    define viewer: [user, user:*, group#member] or editor or viewer from parent
    define blocked: [user]
    define restricted: viewer but not blocked
    define both: viewer and editor
    define r1: [user]
    define r2: [user]
    define r3: [user]
    define r4: [user]
    define r5: [user]
    define r6: [user]
    define r7: [user]
    define r8: [user]
    define wide: r1 or r2 or r3 or r4 or r5 or r6 or r7 or r8
    define conditional: [user with in_hours]

condition in_hours(hour: int) {
  hour >= 9 && hour <= 17
}
`

func tk(object, relation, user string) *openfgav1.TupleKey {
	return &openfgav1.TupleKey{Object: object, Relation: relation, User: user}
}

// dataset is loaded into both databases before any benchmark runs.
func dataset() []*openfgav1.TupleKey {
	var ts []*openfgav1.TupleKey
	// Direct grants on many documents.
	for i := range 1000 {
		ts = append(ts, tk(fmt.Sprintf("doc:%d", i), "owner", fmt.Sprintf("user:u%d", i)))
	}
	// A chain of 8 nested groups.
	for i := range 7 {
		ts = append(ts, tk(fmt.Sprintf("group:g%d", i+1), "member", fmt.Sprintf("group:g%d#member", i)))
	}
	ts = append(ts, tk("group:g0", "member", "user:deep"), tk("doc:nested", "viewer", "group:g7#member"))
	// A folder hierarchy 10 levels deep.
	for i := range 9 {
		ts = append(ts, tk(fmt.Sprintf("folder:f%d", i), "parent", fmt.Sprintf("folder:f%d", i+1)))
	}
	ts = append(ts, tk("folder:f9", "viewer", "user:root"), tk("doc:leaf", "parent", "folder:f0"))
	// A grant reached only through the last of eight union branches.
	ts = append(ts, tk("doc:wide", "r8", "user:eighth"))
	// Intersection and exclusion.
	ts = append(ts, tk("doc:sets", "owner", "user:both"), tk("doc:sets", "viewer", "user:blocked"), tk("doc:sets", "blocked", "user:blocked"))
	// A conditional grant.
	ts = append(ts, tuple.NewTupleKeyWithCondition("doc:cond", "conditional", "user:clock", "in_hours", nil))
	// 1000 documents shared with one large group, and a document with 200 viewers.
	ts = append(ts, tk("group:big", "member", "user:lister"))
	for i := range 1000 {
		ts = append(ts, tk(fmt.Sprintf("doc:shared%d", i), "viewer", "group:big#member"))
	}
	for i := range 200 {
		ts = append(ts, tk("doc:popular", "viewer", fmt.Sprintf("user:fan%d", i)))
	}
	return ts
}

// op is one measured API call, built for a store and model.
type op struct {
	name  string
	build func(store, model string) any
}

func check(object, relation, user string, ctx map[string]any) func(store, model string) any {
	return func(store, model string) any {
		req := &openfgav1.CheckRequest{StoreId: store, AuthorizationModelId: model, TupleKey: &openfgav1.CheckRequestTupleKey{Object: object, Relation: relation, User: user}}
		if ctx != nil {
			req.Context, _ = structpb.NewStruct(ctx)
		}
		return req
	}
}

var reads = []op{
	{"check/direct", check("doc:500", "owner", "user:u500", nil)},
	{"check/direct-denied", check("doc:500", "owner", "user:u501", nil)},
	{"check/computed", check("doc:500", "viewer", "user:u500", nil)},
	{"check/nested-groups", check("doc:nested", "viewer", "user:deep", nil)},
	{"check/folder-depth-10", check("doc:leaf", "viewer", "user:root", nil)},
	{"check/wide-union-last", check("doc:wide", "wide", "user:eighth", nil)},
	{"check/wide-union-denied", check("doc:wide", "wide", "user:nobody", nil)},
	{"check/intersection", check("doc:sets", "both", "user:both", nil)},
	{"check/exclusion", check("doc:sets", "restricted", "user:blocked", nil)},
	{"check/condition", check("doc:cond", "conditional", "user:clock", map[string]any{"hour": 10})},
	{"batch-check/25", func(store, model string) any {
		req := &openfgav1.BatchCheckRequest{StoreId: store, AuthorizationModelId: model}
		for i := range 25 {
			req.Checks = append(req.Checks, &openfgav1.BatchCheckItem{
				TupleKey:      &openfgav1.CheckRequestTupleKey{Object: fmt.Sprintf("doc:%d", i*7), Relation: "viewer", User: fmt.Sprintf("user:u%d", i*11)},
				CorrelationId: fmt.Sprint(i),
			})
		}
		return req
	}},
	{"list-objects/1000", func(store, model string) any {
		return &openfgav1.ListObjectsRequest{StoreId: store, AuthorizationModelId: model, Type: "doc", Relation: "viewer", User: "user:lister"}
	}},
	{"list-users/200", func(store, model string) any {
		return &openfgav1.ListUsersRequest{StoreId: store, AuthorizationModelId: model, Object: &openfgav1.Object{Type: "doc", Id: "popular"}, Relation: "viewer", UserFilters: []*openfgav1.UserTypeFilter{{Type: "user"}}}
	}},
}
