package tests

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/openfga/openfga/pkg/server"
	"github.com/openfga/openfga/pkg/server/config"
	"github.com/openfga/openfga/tests/check"
	"github.com/openfga/openfga/tests/listobjects"
	"github.com/openfga/openfga/tests/listusers"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// variant is a database and transaction kind to run a suite against.
type variant struct {
	name      string
	dialect   atomicfga.Dialect
	pgx       bool
	isolation sql.IsolationLevel
}

// variants lists every dialect and transaction kind.
func variants() []variant {
	var vs []variant
	for _, d := range testdb.Dialects() {
		for _, b := range testdb.Backends(d) {
			vs = append(vs, variant{name: fmt.Sprintf("%s/%s", d, b), dialect: d, pgx: b.Pgx})
		}
	}
	return vs
}

func (v variant) open(t *testing.T) *testdb.DB {
	db := testdb.Open(t, v.dialect)
	db.Isolation = v.isolation
	return db
}

// Upstream runs OpenFGA's YAML assertion suites (consolidated_1_1_tests.yaml
// and abac_tests.yaml) for Check, ListObjects and ListUsers through
// atomicfga, with the server configuration upstream uses for them.
func TestUpstreamAssertions(t *testing.T) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			c := newClient(t, v.open(t), v.pgx,
				server.WithExperimentals(config.ExperimentalCheckOptimizations, config.ExperimentalInlineExpressions),
				server.WithListObjectsDeadline(0),
				server.WithListUsersDeadline(0),
			)
			t.Cleanup(func() { t.Logf("transactions retried after aborts: %d", c.retries.Load()) })
			check.RunAllTests(t, c)
			listobjects.RunAllTests(t, c)
			listusers.RunAllTests(t, c)
		})
	}
}

// TestUpstreamMatrix runs OpenFGA's generated Check and ListObjects
// matrices with each set of experimental resolution strategies upstream
// tests them with.
func TestUpstreamMatrix(t *testing.T) {
	checkCases := map[string][]string{
		"with optimizations":    {config.ExperimentalCheckOptimizations, config.ExperimentalInlineExpressions},
		"without optimizations": {config.ExperimentalInlineExpressions},
		"with weighted graph":   {config.ExperimentalWeightedGraphCheck, config.ExperimentalInlineExpressions},
	}
	listObjectsCases := map[string][]string{
		"with optimizations":    {config.ExperimentalCheckOptimizations, config.ExperimentalListObjectsOptimizations, config.ExperimentalInlineExpressions},
		"without optimizations": {config.ExperimentalInlineExpressions},
	}
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			db := v.open(t)
			for name, experimentals := range checkCases {
				c := newClient(t, db, v.pgx, server.WithExperimentals(experimentals...), server.WithListObjectsDeadline(0), server.WithListUsersDeadline(0))
				check.RunMatrixTests(t, string(v.dialect), name, c)
			}
			for name, experimentals := range listObjectsCases {
				c := newClient(t, db, v.pgx, server.WithExperimentals(experimentals...), server.WithListObjectsDeadline(0), server.WithListUsersDeadline(0))
				listobjects.RunMatrixTests(t, string(v.dialect), name, c)
			}
		})
	}
}
