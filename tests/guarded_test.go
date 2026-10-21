package tests

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

const guardDSL = `model
  schema 1.1
type user
type team
  relations
    define member: [user]
type doc
  relations
    define editor: [user, team#member]
`

// guarded is a workload of concurrent application transactions that act on
// an authorization decision made in the same transaction:
//
//   - grant/revoke change who may edit a document and log it;
//   - edit checks permission and, only if allowed, records an edit.
//
// Every transaction also increments one clock row. Under REPEATABLE READ
// or SERIALIZABLE the clock orders committed transactions consistently with
// their snapshots, so replaying the log by clock value must show that every
// edit was authorized when it committed. If the check ran outside the
// transaction (as with a separate OpenFGA server), a revocation could commit
// between the check and the edit without any conflict being detected.
type guarded struct {
	ctx      context.Context
	db       *testdb.DB
	engine   *atomicfga.Engine
	pgx      bool
	store    string
	outside  bool // check outside the transaction (the unsafe baseline)
	retries  atomic.Int64
	conflict func(error) bool
}

var (
	guardUsers = []string{"user:anne", "user:bob"}
	guardDocs  = []string{"doc:1", "doc:2"}
	guardTeam  = "team:t"
)

// tx runs fn in a transaction, retrying aborted transactions. The clock is
// incremented last, after the authorization reads.
func (g *guarded) tx(tc hegel.TestCase, op, doc, user string, fn func(*atomicfga.Session, atomicfga.Tx) (bool, error)) {
	for attempt := 0; attempt < 200; attempt++ {
		tx, commit, rollback, err := g.db.BeginTx(g.ctx, g.pgx)
		if err != nil {
			tc.Errorf("begin: %v", err)
		}
		s := g.engine.Session(tx)
		logged, err := fn(s, tx)
		if err == nil && logged {
			var res *atomicfga.Result
			if res, err = tx.Query(g.ctx, "SELECT n FROM guard_clock WHERE id = 1", nil); err == nil {
				n, _ := strconv.Atoi(fmt.Sprint(res.Rows[0][0]))
				if _, err = tx.Exec(g.ctx, fmt.Sprintf("UPDATE guard_clock SET n = %d WHERE id = 1", n+1), nil); err == nil {
					_, err = tx.Exec(g.ctx, fmt.Sprintf("INSERT INTO guard_log (store, n, op, doc, usr) VALUES ('%s', %d, '%s', '%s', '%s')", g.store, n+1, op, doc, user), nil)
				}
			}
		}
		if err == nil {
			err = commit()
		}
		if err == nil {
			return
		}
		_ = rollback()
		if !g.conflict(err) {
			tc.Errorf("%s %s %s: %v", op, doc, user, err)
		}
		g.retries.Add(1)
		// Every transaction conflicts on the clock row: back off with
		// jitter, as an application would, to avoid livelock.
		time.Sleep(time.Duration(rand.Int64N(int64(min(attempt+1, 20)) * int64(time.Millisecond))))
	}
	tc.Errorf("%s %s %s: too many retries", op, doc, user)
}

func (g *guarded) write(s *atomicfga.Session, write bool, doc, user string) error {
	req := &openfgav1.WriteRequest{StoreId: g.store}
	t := &openfgav1.TupleKey{Object: doc, Relation: "editor", User: user}
	if write {
		req.Writes = &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{t}, OnDuplicate: "ignore"}
	} else {
		req.Deletes = &openfgav1.WriteRequestDeletes{TupleKeys: []*openfgav1.TupleKeyWithoutCondition{{Object: doc, Relation: "editor", User: user}}, OnMissing: "ignore"}
	}
	_, err := s.Write(g.ctx, req)
	return err
}

func (g *guarded) RuleGrant(tc hegel.TestCase) {
	doc, user := drawFrom(tc, guardDocs), drawFrom(tc, append(slices.Clone(guardUsers), guardTeam+"#member"))
	g.tx(tc, "grant", doc, user, func(s *atomicfga.Session, _ atomicfga.Tx) (bool, error) {
		return true, g.write(s, true, doc, user)
	})
}

func (g *guarded) RuleRevoke(tc hegel.TestCase) {
	doc, user := drawFrom(tc, guardDocs), drawFrom(tc, append(slices.Clone(guardUsers), guardTeam+"#member"))
	g.tx(tc, "revoke", doc, user, func(s *atomicfga.Session, _ atomicfga.Tx) (bool, error) {
		return true, g.write(s, false, doc, user)
	})
}

func (g *guarded) RuleJoin(tc hegel.TestCase) {
	user := drawFrom(tc, guardUsers)
	g.tx(tc, "join", guardTeam, user, func(s *atomicfga.Session, _ atomicfga.Tx) (bool, error) {
		_, err := s.Write(g.ctx, &openfgav1.WriteRequest{StoreId: g.store, Writes: &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{{Object: guardTeam, Relation: "member", User: user}}, OnDuplicate: "ignore"}})
		return true, err
	})
}

func (g *guarded) RuleLeave(tc hegel.TestCase) {
	user := drawFrom(tc, guardUsers)
	g.tx(tc, "leave", guardTeam, user, func(s *atomicfga.Session, _ atomicfga.Tx) (bool, error) {
		_, err := s.Write(g.ctx, &openfgav1.WriteRequest{StoreId: g.store, Deletes: &openfgav1.WriteRequestDeletes{TupleKeys: []*openfgav1.TupleKeyWithoutCondition{{Object: guardTeam, Relation: "member", User: user}}, OnMissing: "ignore"}})
		return true, err
	})
}

func (g *guarded) RuleEdit(tc hegel.TestCase) {
	doc, user := drawFrom(tc, guardDocs), drawFrom(tc, guardUsers)
	req := &openfgav1.CheckRequest{StoreId: g.store, TupleKey: &openfgav1.CheckRequestTupleKey{Object: doc, Relation: "editor", User: user}}
	if g.outside {
		// The baseline: decide first, in a separate committed view (as a
		// call to a separately deployed OpenFGA would), then act.
		tx, done, err := g.db.BeginReader(g.ctx, g.pgx)
		if err != nil {
			tc.Errorf("begin reader: %v", err)
		}
		resp, err := g.engine.Session(tx).Check(g.ctx, req)
		_ = done()
		if err != nil {
			tc.Errorf("check: %v", err)
		}
		if resp.GetAllowed() {
			g.tx(tc, "edit", doc, user, func(*atomicfga.Session, atomicfga.Tx) (bool, error) { return true, nil })
		}
		return
	}
	g.tx(tc, "edit", doc, user, func(s *atomicfga.Session, _ atomicfga.Tx) (bool, error) {
		resp, err := s.Check(g.ctx, req)
		if err != nil || !resp.GetAllowed() {
			return false, err
		}
		return true, nil
	})
}

// InvariantEditsWereAuthorized replays the log in clock order.
func (g *guarded) InvariantEditsWereAuthorized(tc hegel.TestCase) {
	rows, err := g.db.SQL.QueryContext(g.ctx, "SELECT n, op, doc, usr FROM guard_log WHERE store = '"+g.store+"' ORDER BY n")
	if err != nil {
		tc.Errorf("read log: %v", err)
	}
	defer rows.Close()
	editors := map[string]bool{} // doc/user and doc/team grants
	members := map[string]bool{}
	var history []string
	for rows.Next() {
		var n int
		var op, doc, user string
		if err := rows.Scan(&n, &op, &doc, &user); err != nil {
			tc.Errorf("scan log: %v", err)
		}
		history = append(history, fmt.Sprintf("%d %s %s %s", n, op, doc, user))
		switch op {
		case "grant":
			editors[doc+"/"+user] = true
		case "revoke":
			delete(editors, doc+"/"+user)
		case "join":
			members[user] = true
		case "leave":
			delete(members, user)
		case "edit":
			if !editors[doc+"/"+user] && !(editors[doc+"/"+guardTeam+"#member"] && members[user]) {
				tc.Errorf("unauthorized edit committed:\n%s", strings.Join(history, "\n"))
			}
		}
	}
	if err := rows.Err(); err != nil {
		tc.Errorf("read log: %v", err)
	}
}

// conflictErr reports errors that a serializable application retries.
func conflictErr(err error) bool {
	// OpenFGA's own write conflicts (and ErrTransactionAborted) carry Aborted.
	if errors.Is(err, atomicfga.ErrTransactionAborted) || status.Code(err) == codes.Aborted {
		return true
	}
	var st interface{ SQLState() string }
	if errors.As(err, &st) && (st.SQLState() == "40001" || st.SQLState() == "40P01") {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "Deadlock found") || strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_BUSY")
}

func runGuarded(t *testing.T, outside bool) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			db := v.open(t)
			// PostgreSQL detects the read/write conflict at SERIALIZABLE.
			// SQLite and Turso write transactions (BEGIN IMMEDIATE) are serial.
			db.Isolation = sql.LevelSerializable
			if v.dialect != atomicfga.Postgres {
				db.Isolation = sql.LevelDefault
			}
			for _, stmt := range []string{
				"CREATE TABLE guard_clock (id INT PRIMARY KEY, n INT NOT NULL)",
				"INSERT INTO guard_clock VALUES (1, 0)",
				"CREATE TABLE guard_log (store VARCHAR(32), n INT, op VARCHAR(16), doc VARCHAR(64), usr VARCHAR(64))",
			} {
				if _, err := db.SQL.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			engine, err := atomicfga.New(db.Dialect)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(engine.Close)
			var retries int64
			hegel.Test(t, func(ht *hegel.T) {
				g := &guarded{ctx: context.Background(), db: db, engine: engine, pgx: v.pgx, outside: outside, conflict: conflictErr}
				tx, commit, _, err := db.BeginTx(g.ctx, v.pgx)
				if err != nil {
					ht.Fatalf("begin: %v", err)
				}
				s := engine.Session(tx)
				st, err := s.CreateStore(g.ctx, &openfgav1.CreateStoreRequest{Name: "guarded"})
				if err != nil {
					ht.Fatalf("create store: %v", err)
				}
				g.store = st.GetId()
				m := parser.MustTransformDSLToProto(guardDSL)
				if _, err := s.WriteAuthorizationModel(g.ctx, &openfgav1.WriteAuthorizationModelRequest{StoreId: g.store, SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions()}); err != nil {
					ht.Fatalf("write model: %v", err)
				}
				if err := commit(); err != nil {
					ht.Fatalf("commit: %v", err)
				}
				hegel.RunStateful(ht, g, hegel.WithConcurrency(), hegel.WithStatefulStepCount(25))
				retries += g.retries.Load()
			}, guardOptions(outside)...)
			t.Logf("transactions retried after conflicts: %d", retries)
		})
	}
}

func guardOptions(outside bool) []hegel.Option {
	opts := []hegel.Option{hegel.WithTestCases(hegelCases()), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch)}
	if outside {
		// The race is expected; report the first interleaving found rather
		// than replaying a concurrent database workload to shrink it.
		opts = append(opts, hegel.WithPhases(hegel.PhaseGenerate), hegel.WithDatabase(""))
	}
	return opts
}

// TestHegelCheckThenAct requires authorization decisions made inside a
// serializable transaction to hold when its dependent writes commit.
func TestHegelCheckThenAct(t *testing.T) { runGuarded(t, false) }

// TestHegelCheckOutsideTheTransaction is the baseline: deciding outside the
// transaction, as with a separately deployed OpenFGA, lets a revocation
// commit between the check and the edit. Hegel is expected to find such an
// interleaving (and so to fail); run it with ATOMICFGA_DEMONSTRATE_RACE=1.
func TestHegelCheckOutsideTheTransaction(t *testing.T) {
	if os.Getenv("ATOMICFGA_DEMONSTRATE_RACE") == "" {
		t.Skip("demonstration only; set ATOMICFGA_DEMONSTRATE_RACE=1")
	}
	runGuarded(t, true)
}
