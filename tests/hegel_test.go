package tests

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"hegel.dev/go/hegel"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/openfga/openfga/pkg/server"
	serverErrors "github.com/openfga/openfga/pkg/server/errors"
	"github.com/openfga/openfga/pkg/tuple"
	"github.com/openfga/openfga/pkg/typesystem"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// The model under test. Hegel generates the rewrite of doc#derived for each
// model version; every other relation (and so every tuple in the pool) is
// valid in every version.
const machineDSL = `model
  schema 1.1
type user
type group
  relations
    define member: [user, user:*, group#member]
type folder
  relations
    define parent: [folder]
    define owner: [user, group#member]
    define viewer: [user, user:*, group#member] or owner or viewer from parent
type doc
  relations
    define parent: [folder]
    define owner: [user, group#member]
    define editor: [user, group#member, user with level_ok]
    define viewer: [user, user:*, group#member]
    define blocked: [user, group#member]
    define derived: owner

condition level_ok(level: int, required: int) {
  level >= required
}
`

var (
	machineModel = parser.MustTransformDSLToProto(machineDSL)
	users        = []string{"user:anne", "user:bob", "user:carl"}
	groups       = []string{"group:eng", "group:ops"}
	folders      = []string{"folder:a", "folder:b"}
	docs         = []string{"doc:1", "doc:2"}
	relations    = map[string][]string{
		"doc":    {"owner", "editor", "viewer", "blocked", "derived", "parent"},
		"folder": {"parent", "owner", "viewer"},
		"group":  {"member"},
	}
	objectTypes = []string{"doc", "folder", "group"}
	subjects    = []string{"user:anne", "user:bob", "user:carl", "user:*", "group:eng#member", "group:ops#member", "folder:a"}
	tuplePool   = buildPool()
)

func tk(object, relation, user string) *openfgav1.TupleKey {
	return &openfgav1.TupleKey{Object: object, Relation: relation, User: user}
}

func buildPool() []*openfgav1.TupleKey {
	var pool []*openfgav1.TupleKey
	for i, g := range groups {
		for _, u := range append(slices.Clone(users), "user:*", groups[1-i]+"#member") {
			pool = append(pool, tk(g, "member", u))
		}
	}
	pool = append(pool, tk("folder:a", "parent", "folder:b"), tk("folder:b", "parent", "folder:a"))
	for _, f := range folders {
		for _, u := range append(slices.Clone(users), groups[0]+"#member") {
			pool = append(pool, tk(f, "owner", u))
		}
		for _, u := range append(slices.Clone(users), "user:*", groups[1]+"#member") {
			pool = append(pool, tk(f, "viewer", u))
		}
	}
	for i, d := range docs {
		pool = append(pool, tk(d, "parent", folders[i]))
		for _, u := range append(slices.Clone(users), groups[i]+"#member") {
			pool = append(pool, tk(d, "owner", u), tk(d, "blocked", u))
		}
		for _, u := range append(slices.Clone(users), "user:*", groups[1-i]+"#member") {
			pool = append(pool, tk(d, "viewer", u))
		}
		for _, u := range append(slices.Clone(users), groups[0]+"#member") {
			pool = append(pool, tk(d, "editor", u))
		}
		for _, u := range users {
			// Same tuple key as the unconditional editor tuple, with a condition.
			pool = append(pool, tuple.NewTupleKeyWithCondition(d, "editor", u, "level_ok",
				&structpb.Struct{Fields: map[string]*structpb.Value{"required": structpb.NewNumberValue(2)}}))
		}
	}
	return pool
}

// genRewrite draws a userset rewrite over doc's other relations.
func genRewrite(tc hegel.TestCase, depth int) *openfgav1.Userset {
	if depth == 0 || hegel.Draw(tc, hegel.Integers(0, 2)) == 0 {
		switch hegel.Draw(tc, hegel.Integers(0, 5)) {
		case 0:
			return typesystem.ComputedUserset("owner")
		case 1:
			return typesystem.ComputedUserset("editor")
		case 2:
			return typesystem.ComputedUserset("viewer")
		case 3:
			return typesystem.ComputedUserset("blocked")
		case 4:
			return typesystem.TupleToUserset("parent", "viewer")
		default:
			return typesystem.TupleToUserset("parent", "owner")
		}
	}
	a, b := genRewrite(tc, depth-1), genRewrite(tc, depth-1)
	switch hegel.Draw(tc, hegel.Integers(0, 2)) {
	case 0:
		return typesystem.Union(a, b)
	case 1:
		return typesystem.Intersection(a, b)
	default:
		return typesystem.Difference(a, b)
	}
}

func genModel(tc hegel.TestCase) *openfgav1.AuthorizationModel {
	m := proto.Clone(machineModel).(*openfgav1.AuthorizationModel)
	for _, td := range m.GetTypeDefinitions() {
		if td.GetType() == "doc" {
			td.Relations["derived"] = genRewrite(tc, 3)
		}
	}
	return m
}

func key(k *openfgav1.TupleKey) string { return tuple.TupleKeyToString(k) }

// state is the reference model of one transaction's view.
type state struct {
	tuples map[string]*openfgav1.TupleKey
	models []*openfgav1.AuthorizationModel // in write order; IDs are atomicfga's
	app    int
}

func (s state) clone() state {
	return state{tuples: maps.Clone(s.tuples), models: slices.Clone(s.models), app: s.app}
}

func (s state) sortedTuples() []string {
	out := make([]string, 0, len(s.tuples))
	for _, t := range s.tuples {
		out = append(out, tupleKeyString(t))
	}
	slices.Sort(out)
	return out
}

// mirror is a store in the oracle holding a copy of a state.
type mirror struct {
	store  string
	models map[string]string // atomicfga model ID -> oracle model ID
}

// machine drives atomicfga inside one long-lived application transaction
// at a time and compares it with the original OpenFGA on a mirror of the
// transaction's working state.
type machine struct {
	ctx    context.Context
	db     *testdb.DB
	engine *atomicfga.Engine
	oracle *server.Server
	pgx    bool

	mu       sync.Mutex // serializes rules that mutate state
	store    string
	tx       atomicfga.Tx
	commit   func() error
	rollback func() error
	session  *atomicfga.Session

	committed, working   state
	oWorking, oCommitted mirror
	committedMirrorStale bool
	appSeq               int
}

func (m *machine) fail(tc hegel.TestCase, format string, args ...any) {
	tc.Errorf(format, args...)
}

func (m *machine) begin(tc hegel.TestCase) {
	tx, commit, rollback, err := m.db.BeginTx(m.ctx, m.pgx)
	if err != nil {
		m.fail(tc, "begin: %v", err)
	}
	m.tx, m.commit, m.rollback = tx, commit, rollback
	m.session = m.engine.Session(tx)
}

func (m *machine) close() {
	if m.rollback != nil {
		_ = m.rollback()
	}
}

// writeModel writes a model version through atomicfga. Model IDs order
// "latest" by ULID, so each write gets its own millisecond.
func (m *machine) writeModel(tc hegel.TestCase, s *atomicfga.Session, model *openfgav1.AuthorizationModel) *openfgav1.AuthorizationModel {
	time.Sleep(2 * time.Millisecond)
	resp, err := s.WriteAuthorizationModel(m.ctx, &openfgav1.WriteAuthorizationModelRequest{
		StoreId: m.store, SchemaVersion: model.GetSchemaVersion(), TypeDefinitions: model.GetTypeDefinitions(), Conditions: model.GetConditions(),
	})
	if err != nil {
		m.fail(tc, "write model: %v", err)
	}
	model = proto.Clone(model).(*openfgav1.AuthorizationModel)
	model.Id = resp.GetAuthorizationModelId()
	return model
}

// buildMirror writes a state into a fresh oracle store.
func (m *machine) buildMirror(tc hegel.TestCase, s state) mirror {
	st, err := m.oracle.CreateStore(m.ctx, &openfgav1.CreateStoreRequest{Name: "mirror"})
	if err != nil {
		m.fail(tc, "oracle create store: %v", err)
	}
	mr := mirror{store: st.GetId(), models: map[string]string{}}
	for _, model := range s.models {
		time.Sleep(2 * time.Millisecond)
		resp, err := m.oracle.WriteAuthorizationModel(m.ctx, &openfgav1.WriteAuthorizationModelRequest{
			StoreId: mr.store, SchemaVersion: model.GetSchemaVersion(), TypeDefinitions: model.GetTypeDefinitions(), Conditions: model.GetConditions(),
		})
		if err != nil {
			m.fail(tc, "oracle write model: %v", err)
		}
		mr.models[model.GetId()] = resp.GetAuthorizationModelId()
	}
	tuples := slices.Collect(maps.Values(s.tuples))
	for len(tuples) > 0 {
		n := min(len(tuples), 50)
		if _, err := m.oracle.Write(m.ctx, &openfgav1.WriteRequest{StoreId: mr.store, Writes: &openfgav1.WriteRequestWrites{TupleKeys: tuples[:n]}}); err != nil {
			m.fail(tc, "oracle write tuples: %v", err)
		}
		tuples = tuples[n:]
	}
	return mr
}

func (m *machine) committedMirror(tc hegel.TestCase) mirror {
	if m.committedMirrorStale {
		m.oCommitted = m.buildMirror(tc, m.committed)
		m.committedMirrorStale = false
	}
	return m.oCommitted
}

// drawModel picks the model to pin a request to: "" (latest) or a version.
func (m *machine) drawModel(tc hegel.TestCase, s state) string {
	i := hegel.Draw(tc, hegel.Integers(0, len(s.models)))
	if i == 0 {
		return ""
	}
	return s.models[i-1].GetId()
}

func drawFrom[T any](tc hegel.TestCase, xs []T) T {
	return xs[hegel.Draw(tc, hegel.Integers(0, len(xs)-1))]
}

func drawContext(tc hegel.TestCase) *structpb.Struct {
	if hegel.Draw(tc, hegel.Booleans()) {
		return nil
	}
	return &structpb.Struct{Fields: map[string]*structpb.Value{"level": structpb.NewNumberValue(float64(hegel.Draw(tc, hegel.Integers(0, 3))))}}
}

func drawContextualTuples(tc hegel.TestCase) *openfgav1.ContextualTupleKeys {
	n := hegel.Draw(tc, hegel.Integers(0, 2))
	if n == 0 {
		return nil
	}
	ct := &openfgav1.ContextualTupleKeys{}
	for range n {
		ct.TupleKeys = append(ct.TupleKeys, drawFrom(tc, tuplePool))
	}
	return ct
}

func drawObject(tc hegel.TestCase) (string, string) {
	typ := drawFrom(tc, objectTypes)
	var object string
	switch typ {
	case "doc":
		object = drawFrom(tc, docs)
	case "folder":
		object = drawFrom(tc, folders)
	default:
		object = drawFrom(tc, groups)
	}
	return object, drawFrom(tc, relations[typ])
}

// pin maps a request's model to the oracle mirror's copy of it.
func pin(mr mirror, model string) string {
	if model == "" {
		return ""
	}
	return mr.models[model]
}

// --- Rules that change state. They run alone (see the rule groups).

func (m *machine) RuleWriteTuples(tc hegel.TestCase) {
	tc.Event("RuleWriteTuples")
	m.mu.Lock()
	defer m.mu.Unlock()
	req := &openfgav1.WriteRequest{StoreId: m.store}
	for range hegel.Draw(tc, hegel.Integers(1, 3)) {
		t := drawFrom(tc, tuplePool)
		if hegel.Draw(tc, hegel.Booleans()) {
			if req.Deletes == nil {
				req.Deletes = &openfgav1.WriteRequestDeletes{}
			}
			req.Deletes.TupleKeys = append(req.Deletes.TupleKeys, tuple.TupleKeyToTupleKeyWithoutCondition(t))
		} else {
			if req.Writes == nil {
				req.Writes = &openfgav1.WriteRequestWrites{}
			}
			req.Writes.TupleKeys = append(req.Writes.TupleKeys, t)
		}
	}
	if req.Writes != nil && hegel.Draw(tc, hegel.Booleans()) {
		req.Writes.OnDuplicate = "ignore"
	}
	if req.Deletes != nil && hegel.Draw(tc, hegel.Booleans()) {
		req.Deletes.OnMissing = "ignore"
	}
	req.AuthorizationModelId = m.drawModel(tc, m.working)

	oreq := proto.Clone(req).(*openfgav1.WriteRequest)
	oreq.StoreId, oreq.AuthorizationModelId = m.oWorking.store, pin(m.oWorking, req.GetAuthorizationModelId())
	_, err := m.session.Write(m.ctx, req)
	_, oerr := m.oracle.Write(m.ctx, oreq)
	if outcome(err) != outcome(oerr) {
		m.fail(tc, "Write %v: atomicfga %q, OpenFGA %q", req, outcome(err), outcome(oerr))
	}
	if err != nil {
		tc.Event("write rejected")
		return
	}
	tc.Event("write applied")
	for _, d := range req.GetDeletes().GetTupleKeys() {
		delete(m.working.tuples, key(tuple.TupleKeyWithoutConditionToTupleKey(d)))
	}
	for _, w := range req.GetWrites().GetTupleKeys() {
		if _, ok := m.working.tuples[key(w)]; !ok {
			m.working.tuples[key(w)] = w
		}
	}
}

func (m *machine) RuleWriteModel(tc hegel.TestCase) {
	tc.Event("RuleWriteModel")
	m.mu.Lock()
	defer m.mu.Unlock()
	model := m.writeModel(tc, m.session, genModel(tc))
	resp, err := m.oracle.WriteAuthorizationModel(m.ctx, &openfgav1.WriteAuthorizationModelRequest{
		StoreId: m.oWorking.store, SchemaVersion: model.GetSchemaVersion(), TypeDefinitions: model.GetTypeDefinitions(), Conditions: model.GetConditions(),
	})
	if err != nil {
		m.fail(tc, "oracle write model: %v", err)
	}
	m.oWorking.models[model.GetId()] = resp.GetAuthorizationModelId()
	m.working.models = append(m.working.models, model)
}

func (m *machine) RuleAssertions(tc hegel.TestCase) {
	tc.Event("RuleAssertions")
	m.mu.Lock()
	defer m.mu.Unlock()
	model := drawFrom(tc, m.working.models).GetId()
	object, relation := drawObject(tc)
	assertion := &openfgav1.Assertion{TupleKey: &openfgav1.AssertionTupleKey{Object: object, Relation: relation, User: drawFrom(tc, subjects)}, Expectation: hegel.Draw(tc, hegel.Booleans())}
	_, err := m.session.WriteAssertions(m.ctx, &openfgav1.WriteAssertionsRequest{StoreId: m.store, AuthorizationModelId: model, Assertions: []*openfgav1.Assertion{assertion}})
	_, oerr := m.oracle.WriteAssertions(m.ctx, &openfgav1.WriteAssertionsRequest{StoreId: m.oWorking.store, AuthorizationModelId: pin(m.oWorking, model), Assertions: []*openfgav1.Assertion{assertion}})
	if outcome(err) != outcome(oerr) {
		m.fail(tc, "WriteAssertions: atomicfga %q, OpenFGA %q", outcome(err), outcome(oerr))
	}
	got, err := m.session.ReadAssertions(m.ctx, &openfgav1.ReadAssertionsRequest{StoreId: m.store, AuthorizationModelId: model})
	want, oerr := m.oracle.ReadAssertions(m.ctx, &openfgav1.ReadAssertionsRequest{StoreId: m.oWorking.store, AuthorizationModelId: pin(m.oWorking, model)})
	if outcome(err) != outcome(oerr) || !proto.Equal(&openfgav1.Assertions{Assertions: got.GetAssertions()}, &openfgav1.Assertions{Assertions: want.GetAssertions()}) {
		m.fail(tc, "ReadAssertions: atomicfga %v %q, OpenFGA %v %q", got, outcome(err), want, outcome(oerr))
	}
}

func (m *machine) RuleApplicationRow(tc hegel.TestCase) {
	tc.Event("RuleApplicationRow")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appSeq++
	if _, err := m.tx.Exec(m.ctx, "INSERT INTO app_audit (store, id) VALUES ('"+m.store+"', '"+strconv.Itoa(m.appSeq)+"')", nil); err != nil {
		m.fail(tc, "application insert: %v", err)
	}
	m.working.app++
}

func (m *machine) RuleCommit(tc hegel.TestCase) {
	tc.Event("RuleCommit")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.commit(); err != nil {
		m.fail(tc, "commit: %v", err)
	}
	m.rollback = nil
	m.committed = m.working.clone()
	m.committedMirrorStale = true
	m.begin(tc)
}

func (m *machine) RuleRollback(tc hegel.TestCase) {
	tc.Event("RuleRollback")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.rollback(); err != nil {
		m.fail(tc, "rollback: %v", err)
	}
	m.rollback = nil
	m.working = m.committed.clone()
	m.oWorking = m.buildMirror(tc, m.working)
	m.begin(tc)
}

// RuleDiscardedSavepoint makes changes through atomicfga and the
// application inside an application savepoint, then rolls back to it.
func (m *machine) RuleDiscardedSavepoint(tc hegel.TestCase) {
	tc.Event("RuleDiscardedSavepoint")
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.tx.Exec(m.ctx, "SAVEPOINT app_savepoint", nil); err != nil {
		m.fail(tc, "savepoint: %v", err)
	}
	for range hegel.Draw(tc, hegel.Integers(1, 3)) {
		t := drawFrom(tc, tuplePool)
		req := &openfgav1.WriteRequest{StoreId: m.store}
		if _, ok := m.working.tuples[key(t)]; ok {
			req.Deletes = &openfgav1.WriteRequestDeletes{TupleKeys: []*openfgav1.TupleKeyWithoutCondition{tuple.TupleKeyToTupleKeyWithoutCondition(t)}, OnMissing: "ignore"}
		} else {
			req.Writes = &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{t}, OnDuplicate: "ignore"}
		}
		_, _ = m.session.Write(m.ctx, req)
	}
	if hegel.Draw(tc, hegel.Booleans()) {
		m.writeModel(tc, m.session, genModel(tc))
	}
	if _, err := m.tx.Exec(m.ctx, "INSERT INTO app_audit (store, id) VALUES ('"+m.store+"', 'discarded')", nil); err != nil {
		m.fail(tc, "application insert: %v", err)
	}
	if _, err := m.tx.Exec(m.ctx, "ROLLBACK TO SAVEPOINT app_savepoint", nil); err != nil {
		m.fail(tc, "rollback to savepoint: %v", err)
	}
	if _, err := m.tx.Exec(m.ctx, "RELEASE SAVEPOINT app_savepoint", nil); err != nil {
		m.fail(tc, "release savepoint: %v", err)
	}
}

// --- Rules that only read. They may run concurrently on one session.

func (m *machine) snapshot() (state, mirror) {
	// Reads never overlap mutations (rule groups), so no lock is needed;
	// take one anyway for the race detector's benefit.
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.working, m.oWorking
}

func (m *machine) drawCheck(tc hegel.TestCase, s state) *openfgav1.CheckRequest {
	object, relation := drawObject(tc)
	req := &openfgav1.CheckRequest{
		StoreId:              m.store,
		AuthorizationModelId: m.drawModel(tc, s),
		TupleKey:             &openfgav1.CheckRequestTupleKey{Object: object, Relation: relation, User: drawFrom(tc, subjects)},
		Context:              drawContext(tc),
		ContextualTuples:     drawContextualTuples(tc),
	}
	if hegel.Draw(tc, hegel.Booleans()) {
		req.Consistency = openfgav1.ConsistencyPreference_HIGHER_CONSISTENCY
	}
	return req
}

func (m *machine) RuleCheck(tc hegel.TestCase) {
	tc.Event("RuleCheck")
	s, mr := m.snapshot()
	req := m.drawCheck(tc, s)
	oreq := proto.Clone(req).(*openfgav1.CheckRequest)
	oreq.StoreId, oreq.AuthorizationModelId = mr.store, pin(mr, req.GetAuthorizationModelId())
	got, err := m.session.Check(m.ctx, req)
	want, oerr := m.oracle.Check(m.ctx, oreq)
	if outcome(err) != outcome(oerr) || got.GetAllowed() != want.GetAllowed() {
		m.fail(tc, "Check %v: atomicfga %v %q, OpenFGA %v %q", req, got.GetAllowed(), outcome(err), want.GetAllowed(), outcome(oerr))
	}
	tc.Event(fmt.Sprintf("check allowed=%v error=%v", got.GetAllowed(), err != nil))
}

func (m *machine) RuleBatchCheck(tc hegel.TestCase) {
	tc.Event("RuleBatchCheck")
	s, mr := m.snapshot()
	model := m.drawModel(tc, s)
	req := &openfgav1.BatchCheckRequest{StoreId: m.store, AuthorizationModelId: model}
	for i := range hegel.Draw(tc, hegel.Integers(1, 4)) {
		c := m.drawCheck(tc, s)
		req.Checks = append(req.Checks, &openfgav1.BatchCheckItem{
			TupleKey: c.GetTupleKey(), ContextualTuples: c.GetContextualTuples(), Context: c.GetContext(), CorrelationId: strconv.Itoa(i),
		})
	}
	oreq := proto.Clone(req).(*openfgav1.BatchCheckRequest)
	oreq.StoreId, oreq.AuthorizationModelId = mr.store, pin(mr, model)
	got, err := m.session.BatchCheck(m.ctx, req)
	want, oerr := m.oracle.BatchCheck(m.ctx, oreq)
	if outcome(err) != outcome(oerr) {
		m.fail(tc, "BatchCheck %v: atomicfga %q, OpenFGA %q", req, outcome(err), outcome(oerr))
	}
	for id, w := range want.GetResult() {
		g := got.GetResult()[id]
		if g.GetAllowed() != w.GetAllowed() || (g.GetError() == nil) != (w.GetError() == nil) || g.GetError().GetMessage() != w.GetError().GetMessage() {
			m.fail(tc, "BatchCheck %v item %s: atomicfga %v, OpenFGA %v", req, id, g, w)
		}
	}
}

func (m *machine) RuleListObjects(tc hegel.TestCase) {
	tc.Event("RuleListObjects")
	s, mr := m.snapshot()
	typ := drawFrom(tc, objectTypes)
	req := &openfgav1.ListObjectsRequest{
		StoreId: m.store, AuthorizationModelId: m.drawModel(tc, s), Type: typ, Relation: drawFrom(tc, relations[typ]),
		User: drawFrom(tc, subjects), Context: drawContext(tc), ContextualTuples: drawContextualTuples(tc),
	}
	oreq := proto.Clone(req).(*openfgav1.ListObjectsRequest)
	oreq.StoreId, oreq.AuthorizationModelId = mr.store, pin(mr, req.GetAuthorizationModelId())
	got, err := m.session.ListObjects(m.ctx, req)
	want, oerr := m.oracle.ListObjects(m.ctx, oreq)
	g, w := slices.Sorted(slices.Values(got.GetObjects())), slices.Sorted(slices.Values(want.GetObjects()))
	if joinedOutcome(err) != joinedOutcome(oerr) || !slices.Equal(g, w) {
		m.fail(tc, "ListObjects %v: atomicfga %v %q, OpenFGA %v %q", req, g, joinedOutcome(err), w, joinedOutcome(oerr))
	}
	tc.Event(fmt.Sprintf("list objects nonempty=%v error=%v", len(g) > 0, err != nil))
}

func (m *machine) RuleListUsers(tc hegel.TestCase) {
	tc.Event("RuleListUsers")
	s, mr := m.snapshot()
	object, relation := drawObject(tc)
	typ, id := tuple.SplitObject(object)
	filter := drawFrom(tc, []*openfgav1.UserTypeFilter{{Type: "user"}, {Type: "group", Relation: "member"}, {Type: "folder"}})
	req := &openfgav1.ListUsersRequest{
		StoreId: m.store, AuthorizationModelId: m.drawModel(tc, s), Object: &openfgav1.Object{Type: typ, Id: id}, Relation: relation,
		UserFilters: []*openfgav1.UserTypeFilter{filter}, Context: drawContext(tc), ContextualTuples: drawContextualTuples(tc).GetTupleKeys(),
	}
	oreq := proto.Clone(req).(*openfgav1.ListUsersRequest)
	oreq.StoreId, oreq.AuthorizationModelId = mr.store, pin(mr, req.GetAuthorizationModelId())
	got, err := m.session.ListUsers(m.ctx, req)
	want, oerr := m.oracle.ListUsers(m.ctx, oreq)
	g, w := sortedUsers(got.GetUsers()), sortedUsers(want.GetUsers())
	if joinedOutcome(err) != joinedOutcome(oerr) || !slices.Equal(g, w) {
		m.fail(tc, "ListUsers %v: atomicfga %v %q, OpenFGA %v %q", req, g, joinedOutcome(err), w, joinedOutcome(oerr))
	}
	tc.Event(fmt.Sprintf("list users nonempty=%v error=%v", len(g) > 0, err != nil))
}

func (m *machine) RuleExpand(tc hegel.TestCase) {
	tc.Event("RuleExpand")
	s, mr := m.snapshot()
	object, relation := drawObject(tc)
	req := &openfgav1.ExpandRequest{StoreId: m.store, AuthorizationModelId: m.drawModel(tc, s), TupleKey: &openfgav1.ExpandRequestTupleKey{Object: object, Relation: relation}}
	oreq := proto.Clone(req).(*openfgav1.ExpandRequest)
	oreq.StoreId, oreq.AuthorizationModelId = mr.store, pin(mr, req.GetAuthorizationModelId())
	got, err := m.session.Expand(m.ctx, req)
	want, oerr := m.oracle.Expand(m.ctx, oreq)
	g, w := expandString(got.GetTree().GetRoot()), expandString(want.GetTree().GetRoot())
	if outcome(err) != outcome(oerr) || g != w {
		m.fail(tc, "Expand %v: atomicfga %s %q, OpenFGA %s %q", req, g, outcome(err), w, outcome(oerr))
	}
}

func (m *machine) RuleRead(tc hegel.TestCase) {
	tc.Event("RuleRead")
	_, mr := m.snapshot()
	var filter *openfgav1.ReadRequestTupleKey
	object, relation := drawObject(tc)
	typ, _ := tuple.SplitObject(object)
	switch hegel.Draw(tc, hegel.Integers(0, 4)) {
	case 1:
		filter = &openfgav1.ReadRequestTupleKey{Object: object}
	case 2:
		filter = &openfgav1.ReadRequestTupleKey{Object: object, Relation: relation}
	case 3:
		filter = &openfgav1.ReadRequestTupleKey{Object: typ + ":", User: drawFrom(tc, subjects)}
	case 4:
		filter = &openfgav1.ReadRequestTupleKey{Object: object, Relation: relation, User: drawFrom(tc, subjects)}
	}
	req := &openfgav1.ReadRequest{StoreId: m.store, TupleKey: filter, PageSize: wrapInt32(hegel.Draw(tc, hegel.Integers(1, 60)))}
	oreq := proto.Clone(req).(*openfgav1.ReadRequest)
	oreq.StoreId = mr.store
	got, err := readAll(m.ctx, m.session.Read, req)
	want, oerr := readAll(m.ctx, m.oracle.Read, oreq)
	g, w := sortedTupleKeys(got), sortedTupleKeys(want)
	if outcome(err) != outcome(oerr) || !slices.Equal(g, w) {
		m.fail(tc, "Read %v: atomicfga %v %q, OpenFGA %v %q", req, g, outcome(err), w, outcome(oerr))
	}
}

// --- Invariants.

// InvariantWorkingState requires the transaction's tuples and application
// rows to match the reference model.
func (m *machine) InvariantWorkingState(tc hegel.TestCase) {
	got, err := readAll(m.ctx, m.session.Read, &openfgav1.ReadRequest{StoreId: m.store, PageSize: wrapInt32(100)})
	if err != nil {
		m.fail(tc, "read working state: %v", err)
	}
	if g, w := sortedTupleKeys(got), m.working.sortedTuples(); !slices.Equal(g, w) {
		m.fail(tc, "working tuples: atomicfga %v, model %v", g, w)
	}
	res, err := m.tx.Query(m.ctx, "SELECT id FROM app_audit WHERE store = '"+m.store+"'", nil)
	if err != nil {
		m.fail(tc, "count application rows: %v", err)
	}
	if len(res.Rows) != m.working.app {
		m.fail(tc, "application rows: %d, model %d", len(res.Rows), m.working.app)
	}
	models, err := m.session.ReadAuthorizationModels(m.ctx, &openfgav1.ReadAuthorizationModelsRequest{StoreId: m.store, PageSize: wrapInt32(100)})
	if err != nil || len(models.GetAuthorizationModels()) != len(m.working.models) {
		m.fail(tc, "working models: %d %v, model %d", len(models.GetAuthorizationModels()), err, len(m.working.models))
	}
}

// InvariantIsolation requires another transaction to observe exactly the
// committed state: uncommitted tuples, models and application rows of the
// open transaction stay invisible, including through OpenFGA's shared caches.
func (m *machine) InvariantIsolation(tc hegel.TestCase) {
	tx, done, err := m.db.BeginReader(m.ctx, m.pgx)
	if err != nil {
		m.fail(tc, "begin reader: %v", err)
	}
	defer func() { _ = done() }()
	reader := m.engine.Session(tx)
	got, err := readAll(m.ctx, reader.Read, &openfgav1.ReadRequest{StoreId: m.store, PageSize: wrapInt32(100)})
	if err != nil {
		m.fail(tc, "reader read: %v", err)
	}
	if g, w := sortedTupleKeys(got), m.committed.sortedTuples(); !slices.Equal(g, w) {
		m.fail(tc, "reader tuples: atomicfga %v, committed %v", g, w)
	}
	res, err := tx.Query(m.ctx, "SELECT id FROM app_audit WHERE store = '"+m.store+"'", nil)
	if err != nil || len(res.Rows) != m.committed.app {
		m.fail(tc, "reader application rows: %d %v, committed %d", len(res.Rows), err, m.committed.app)
	}
	mr := m.committedMirror(tc)
	for _, model := range append([]string{""}, modelIDs(m.working.models)...) {
		for _, d := range docs {
			for _, u := range users {
				req := &openfgav1.CheckRequest{StoreId: m.store, AuthorizationModelId: model, TupleKey: &openfgav1.CheckRequestTupleKey{Object: d, Relation: "derived", User: u}}
				oreq := proto.Clone(req).(*openfgav1.CheckRequest)
				oreq.StoreId, oreq.AuthorizationModelId = mr.store, mr.models[model]
				if model != "" && oreq.AuthorizationModelId == "" {
					// Not committed: it must be invisible to the reader.
					if _, err := reader.Check(m.ctx, req); outcome(err) != outcome(errModelNotFound(model)) {
						m.fail(tc, "reader resolved uncommitted model %s: %q", model, outcome(err))
					}
					continue
				}
				g, err := reader.Check(m.ctx, req)
				w, oerr := m.oracle.Check(m.ctx, oreq)
				if outcome(err) != outcome(oerr) || g.GetAllowed() != w.GetAllowed() {
					m.fail(tc, "reader Check %v: atomicfga %v %q, committed OpenFGA %v %q", req, g.GetAllowed(), outcome(err), w.GetAllowed(), outcome(oerr))
				}
			}
		}
	}
}

func modelIDs(ms []*openfgav1.AuthorizationModel) []string {
	ids := make([]string, len(ms))
	for i, m := range ms {
		ids[i] = m.GetId()
	}
	return ids
}

func wrapInt32(n int) *wrapperspb.Int32Value { return wrapperspb.Int32(int32(n)) }

func hegelCases() int {
	if v, err := strconv.Atoi(os.Getenv("ATOMICFGA_HEGEL_CASES")); err == nil && v > 0 {
		return v
	}
	return 40
}

// runMachine runs the state machine against every variant.
func runMachine(t *testing.T, concurrent bool) {
	for _, v := range variants() {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			db := v.open(t)
			if _, err := db.SQL.Exec("CREATE TABLE app_audit (store VARCHAR(32), id VARCHAR(32))"); err != nil {
				t.Fatal(err)
			}
			engine, err := atomicfga.New(db.Dialect)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(engine.Close)
			oracle := newOracle(t, db.Dialect)

			opts := []hegel.StateMachineOption{
				hegel.WithStatefulStepCount(30),
				hegel.WithAlwaysCheckInvariants("InvariantWorkingState"),
				hegel.WithRuleGroup("mutate", "RuleWriteTuples", "RuleWriteModel", "RuleAssertions", "RuleApplicationRow", "RuleCommit", "RuleRollback", "RuleDiscardedSavepoint"),
			}
			if concurrent {
				opts = append(opts, hegel.WithBoundedConcurrency(4))
			}
			hegel.Test(t, func(ht *hegel.T) {
				m := &machine{ctx: context.Background(), db: db, engine: engine, oracle: oracle, pgx: v.pgx}
				defer m.close()
				m.begin(ht)
				st, err := m.session.CreateStore(m.ctx, &openfgav1.CreateStoreRequest{Name: "machine"})
				if err != nil {
					ht.Fatalf("create store: %v", err)
				}
				m.store = st.GetId()
				m.working = state{tuples: map[string]*openfgav1.TupleKey{}}
				m.working.models = append(m.working.models, m.writeModel(ht, m.session, genModel(ht)))
				// A committed baseline makes grants (and so allowed checks) common.
				for range hegel.Draw(ht, hegel.Integers(0, 20)) {
					t := drawFrom(ht, tuplePool)
					if _, ok := m.working.tuples[key(t)]; !ok {
						m.working.tuples[key(t)] = t
					}
				}
				if baseline := slices.Collect(maps.Values(m.working.tuples)); len(baseline) > 0 {
					if _, err := m.session.Write(m.ctx, &openfgav1.WriteRequest{StoreId: m.store, Writes: &openfgav1.WriteRequestWrites{TupleKeys: baseline}}); err != nil {
						ht.Fatalf("write baseline: %v", err)
					}
				}
				m.RuleCommit(ht)
				m.oWorking = m.buildMirror(ht, m.working)
				hegel.RunStateful(ht, m, opts...)
			}, hegel.WithTestCases(hegelCases()), hegel.SuppressHealthCheck(hegel.TooSlow, hegel.FilterTooMuch))
		})
	}
}

// TestHegelTransactions runs the machine one rule at a time.
func TestHegelTransactions(t *testing.T) { runMachine(t, false) }

// TestHegelConcurrentReads lets read rules run concurrently on the
// session, between mutations.
func TestHegelConcurrentReads(t *testing.T) { runMachine(t, true) }

func errModelNotFound(id string) error { return serverErrors.AuthorizationModelNotFound(id) }
