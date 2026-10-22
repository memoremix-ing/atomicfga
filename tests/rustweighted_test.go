//go:build rustcheck

package tests

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"
	"github.com/openfga/openfga/pkg/typesystem"

	"github.com/memoremix-ing/atomicfga"
)

// openfgaAlternatives is the strategy table by OpenFGA's own typesystem
// predicates, keyed like atomicfga.RustModelAlternatives.
func openfgaAlternatives(t testing.TB, model *openfgav1.AuthorizationModel) []string {
	ts, err := typesystem.New(model)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	types := model.GetTypeDefinitions()
	for _, td := range types {
		for relation, rewrite := range td.GetRelations() {
			usersets, _ := ts.DirectlyRelatedUsersets(td.GetType(), relation)
			for _, ut := range types {
				userType := ut.GetType()
				alt := ts.UsersetUseRecursiveResolver(td.GetType(), relation, userType)
				for _, u := range usersets {
					alt = alt || ts.UsersetUseWeight2Resolver(td.GetType(), relation, userType, u)
				}
				if alt {
					keys = append(keys, fmt.Sprintf("userset:%s#%s@%s", td.GetType(), relation, userType))
				}
				for _, ttu := range flattenTTUs(rewrite) {
					if ts.TTUUseWeight2Resolver(td.GetType(), relation, userType, ttu) || ts.TTUUseRecursiveResolver(td.GetType(), relation, userType, ttu) {
						keys = append(keys, fmt.Sprintf("ttu:%s#%s:%s:%s@%s", td.GetType(), relation,
							ttu.GetTupleset().GetRelation(), ttu.GetComputedUserset().GetRelation(), userType))
					}
				}
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func flattenTTUs(u *openfgav1.Userset) []*openfgav1.TupleToUserset {
	switch x := u.GetUserset().(type) {
	case *openfgav1.Userset_TupleToUserset:
		return []*openfgav1.TupleToUserset{x.TupleToUserset}
	case *openfgav1.Userset_Union:
		var out []*openfgav1.TupleToUserset
		for _, c := range x.Union.GetChild() {
			out = append(out, flattenTTUs(c)...)
		}
		return out
	case *openfgav1.Userset_Intersection:
		var out []*openfgav1.TupleToUserset
		for _, c := range x.Intersection.GetChild() {
			out = append(out, flattenTTUs(c)...)
		}
		return out
	case *openfgav1.Userset_Difference:
		return append(flattenTTUs(x.Difference.GetBase()), flattenTTUs(x.Difference.GetSubtract())...)
	}
	return nil
}

// TestHegelWeightedGraphMatchesOpenFGA compares the strategy table the Rust
// port derives (from its port of the weighted model graph) with OpenFGA's,
// on valid random models of both generators: the Check generator's
// (recursive parents, nested memberships, exclusions) and the model
// compiler's.
func TestHegelWeightedGraphMatchesOpenFGA(t *testing.T) {
	var models, nonempty, keys int
	t.Cleanup(func() {
		t.Logf("%d valid models (%d with a nonempty table, %d table entries) compared", models, nonempty, keys)
	})
	hegel.Test(t, func(ht *hegel.T) {
		var m *openfgav1.AuthorizationModel
		if hegel.Draw(ht, hegel.Integers(0, 2)) == 0 {
			parsed, err := parser.TransformDSLToProto(drawRecursiveModel(ht))
			if err != nil {
				return
			}
			m = parsed
		} else if hegel.Draw(ht, hegel.Booleans()) {
			parsed, err := parser.TransformDSLToProto(drawRustModel(ht).dsl)
			if err != nil {
				return
			}
			m = parsed
		} else {
			req := drawCompileModel(ht)
			m = &openfgav1.AuthorizationModel{SchemaVersion: req.GetSchemaVersion(), TypeDefinitions: req.GetTypeDefinitions(), Conditions: req.GetConditions()}
		}
		if _, err := typesystem.NewAndValidate(t.Context(), m); err != nil {
			return
		}
		want := openfgaAlternatives(t, m)
		got, err := atomicfga.RustModelAlternatives(m)
		if err != nil {
			ht.Fatalf("rust: %v", err)
		}
		models++
		keys += len(want)
		if len(want) > 0 {
			nonempty++
		}
		if !slices.Equal(want, got) {
			ht.Fatalf("strategy tables differ for %v\n--- openfga\n%s\n--- rust\n%s", m, strings.Join(want, "\n"), strings.Join(got, "\n"))
		}
	}, hegel.WithTestCases(hegelCases()*50), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch))
}

// drawRecursiveModel draws models whose relations recurse along several
// paths at once: through their own usersets, through parents, and through
// other types' relations, combined by any operator.
func drawRecursiveModel(tc hegel.TestCase) string {
	pick := func(options []string) string {
		var parts []string
		for _, o := range options {
			if hegel.Draw(tc, hegel.Booleans()) {
				parts = append(parts, o)
			}
		}
		if len(parts) == 0 {
			parts = options[:1]
		}
		return strings.Join(parts, ", ")
	}
	terms := func(direct string, others []string) string {
		expr := "[" + direct + "]"
		for _, o := range others {
			if hegel.Draw(tc, hegel.Integers(0, 2)) == 0 {
				expr = fmt.Sprintf("(%s %s %s)", expr, drawFrom(tc, []string{"or", "or", "and", "but not"}), o)
			}
		}
		return expr
	}
	member := terms(pick([]string{"user", "group#member", "user:*", "folder#viewer"}), []string{"member from parent", "owner", "viewer from team"})
	viewer := terms(pick([]string{"user", "folder#viewer", "group#member", "user:*"}), []string{"viewer from parent", "member from team", "owner", "editor"})
	editor := terms(pick([]string{"user", "folder#editor", "group#member"}), []string{"editor from parent", "viewer", "owner"})
	return fmt.Sprintf(`model
  schema 1.1
type user
type group
  relations
    define parent: [group]
    define owner: [user]
    define team: [folder]
    define member: %s
    define viewer: [user]
type folder
  relations
    define parent: [folder]
    define team: [group]
    define owner: [user, group#member]
    define viewer: %s
    define editor: %s
`, member, viewer, editor)
}
