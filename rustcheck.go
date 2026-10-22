//go:build rustcheck

package atomicfga

/*
#cgo LDFLAGS: ${SRCDIR}/rust/target/release/libatomicfga_check.a
#cgo darwin LDFLAGS: -framework CoreFoundation
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct { uint8_t *ptr; size_t len; size_t cap; } afc_buf;
typedef struct afc_engine afc_engine;
typedef struct afc_task afc_task;

afc_engine *afc_engine_new(void);
void afc_engine_free(afc_engine *e);
bool afc_engine_has_model(const afc_engine *e, const uint8_t *id, size_t len);
afc_buf afc_engine_load_model(const afc_engine *e, const uint8_t *json, size_t len);
afc_task *afc_check_new(const afc_engine *e, const uint8_t *model, size_t model_len, const uint8_t *req, size_t req_len);
afc_buf afc_check_step(afc_task *t, const uint8_t *answers, size_t len);
afc_buf afc_compile_model(const uint8_t *json, size_t len, const uint8_t *options, size_t options_len);
void afc_check_free(afc_task *t);
void afc_buf_free(afc_buf b);
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"unsafe"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	serverErrors "github.com/openfga/openfga/pkg/server/errors"
	"github.com/openfga/openfga/pkg/storage"
	"github.com/openfga/openfga/pkg/typesystem"

	"github.com/memoremix-ing/atomicfga/internal/bind"
)

// ErrRustRefused is returned by [Session.RustCheckStrict] when the Rust
// evaluator cannot be certain of OpenFGA's answer: the request uses
// something it does not implement, or OpenFGA's own answer would depend on
// goroutine timing. It carries gRPC code Unimplemented.
var ErrRustRefused = errors.New("atomicfga: the Rust evaluator refused the check")

type refusedError struct{ reason string }

func (e refusedError) Error() string { return fmt.Sprintf("%s: %s", ErrRustRefused, e.reason) }

func (e refusedError) Is(target error) bool { return target == ErrRustRefused }

func (e refusedError) GRPCStatus() *status.Status { return status.New(codes.Unimplemented, e.Error()) }

// rustState is the engine's Rust evaluator, which caches decoded models.
type rustState struct {
	once   sync.Once
	mu     sync.RWMutex
	engine *C.afc_engine
}

func (r *rustState) get() *C.afc_engine {
	r.once.Do(func() { r.engine = C.afc_engine_new() })
	return r.engine
}

func (r *rustState) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.engine != nil {
		C.afc_engine_free(r.engine)
		r.engine = nil
	}
}

func ptr(b []byte) *C.uint8_t {
	if len(b) == 0 {
		return nil
	}
	return (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(b)))
}

func take(b C.afc_buf) []byte {
	defer C.afc_buf_free(b)
	return C.GoBytes(unsafe.Pointer(b.ptr), C.int(b.len))
}

// RustCheck answers like [Session.Check], evaluating the check with the
// Rust port of OpenFGA's resolver. When the port refuses (see
// [ErrRustRefused]) it answers with OpenFGA's own Check instead.
func (s *Session) RustCheck(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	resp, err := s.RustCheckStrict(ctx, req)
	if errors.Is(err, ErrRustRefused) {
		return s.Check(ctx, req)
	}
	return resp, err
}

// RustCheckStrict is [Session.RustCheck] without the fallback: it returns
// an error matching [ErrRustRefused] when the port refuses.
func (s *Session) RustCheckStrict(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	return doModel(s, ctx, req, s.rustCheck)
}

// rustCheck mirrors server.Check up to the CheckCommand, which the Rust
// evaluator replaces.
func (s *Session) rustCheck(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	r := &s.engine.rust
	r.mu.RLock()
	defer r.mu.RUnlock()
	engine := r.get()
	if engine == nil {
		return nil, refusedError{"engine closed"}
	}
	modelID := req.GetAuthorizationModelId()
	if err := s.loadRustModel(ctx, engine, req.GetStoreId(), modelID); err != nil {
		return nil, err
	}
	reqJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		return nil, serverErrors.HandleError("", err)
	}
	id := []byte(modelID)
	task := C.afc_check_new(engine, ptr(id), C.size_t(len(id)), ptr(reqJSON), C.size_t(len(reqJSON)))
	if task == nil {
		return nil, refusedError{"panic in the evaluator"}
	}
	defer C.afc_check_free(task)

	// A pipelining transaction sends a step's reads in one round trip. Any
	// other executes them one at a time anyway: answer them one by one, so
	// reads made moot by a decided union are skipped (as OpenFGA drops the
	// queued statements of canceled branches).
	_, pipelined := s.tx.(bind.Pipeliner)
	var answers []byte
	var pending []rustRead
	for {
		out := take(C.afc_check_step(task, ptr(answers), C.size_t(len(answers))))
		var msg struct {
			Reads   []rustRead `json:"reads"`
			Cancel  []uint64   `json:"cancel"`
			Allowed *bool      `json:"allowed"`
			Error   *struct {
				Kind    string `json:"kind"`
				Message string `json:"message"`
			} `json:"error"`
			Refused *string `json:"refused"`
		}
		if err := json.Unmarshal(out, &msg); err != nil {
			return nil, serverErrors.HandleError("", err)
		}
		switch {
		case msg.Allowed != nil:
			return &openfgav1.CheckResponse{Allowed: *msg.Allowed}, nil
		case msg.Refused != nil:
			return nil, refusedError{*msg.Refused}
		case msg.Error != nil:
			return nil, rustError(msg.Error.Kind, msg.Error.Message)
		}
		pending = append(pending, msg.Reads...)
		pending = slices.DeleteFunc(pending, func(r rustRead) bool { return slices.Contains(msg.Cancel, r.ID) })
		if len(pending) == 0 {
			return nil, refusedError{"no reads to answer"}
		}
		batch := pending
		if !pipelined {
			batch = pending[:1]
		}
		pending = pending[len(batch):]
		if answers, err = s.answerReads(ctx, req, batch); err != nil {
			return nil, err
		}
	}
}

func rustError(kind, message string) error {
	switch kind {
	case "validation":
		return serverErrors.ValidationError(errors.New(message))
	case "invalid_tuple":
		return status.Error(codes.Code(openfgav1.ErrorCode_invalid_tuple), message)
	case "too_complex":
		return serverErrors.ErrAuthorizationModelResolutionTooComplex
	}
	return serverErrors.HandleError("", errors.New(message))
}

// loadRustModel hands the evaluator a model it has not decoded yet. The
// model ID is visible in the transaction (doModel checked), and models are
// immutable, so one decoded copy serves every transaction.
func (s *Session) loadRustModel(ctx context.Context, engine *C.afc_engine, store, id string) error {
	b := []byte(id)
	if C.afc_engine_has_model(engine, ptr(b), C.size_t(len(b))) {
		return nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return serverErrors.AuthorizationModelNotFound(id)
	}
	model, err := s.engine.datastore.ReadAuthorizationModel(ctx, store, id)
	if errors.Is(err, storage.ErrNotFound) {
		return serverErrors.AuthorizationModelNotFound(id)
	}
	if err != nil {
		return serverErrors.HandleError("", err)
	}
	modelJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(model)
	if err != nil {
		return serverErrors.HandleError("", err)
	}
	load := map[string]any{"model": json.RawMessage(modelJSON)}
	if alternatives, err := strategyAlternatives(model); err != nil {
		load["refuse"] = "model analysis: " + err.Error()
	} else {
		load["alternatives"] = alternatives
	}
	js, err := json.Marshal(load)
	if err != nil {
		return serverErrors.HandleError("", err)
	}
	var loaded struct {
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(take(C.afc_engine_load_model(engine, ptr(js), C.size_t(len(js)))), &loaded); err != nil {
		return serverErrors.HandleError("", err)
	}
	if loaded.Error != nil {
		return serverErrors.HandleError("", errors.New(*loaded.Error))
	}
	return nil
}

// strategyAlternatives lists the nodes OpenFGA's planner may resolve with
// its recursive or weight-2 strategies, by OpenFGA's own predicates (which
// rest on the weighted authorization model graph). The Rust evaluator
// implements the default strategy and widens its outcomes at these nodes.
func strategyAlternatives(model *openfgav1.AuthorizationModel) ([]string, error) {
	ts, err := typesystem.New(model)
	if err != nil {
		return nil, err
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
				for _, ttu := range ttus(rewrite) {
					if ts.TTUUseWeight2Resolver(td.GetType(), relation, userType, ttu) || ts.TTUUseRecursiveResolver(td.GetType(), relation, userType, ttu) {
						keys = append(keys, fmt.Sprintf("ttu:%s#%s:%s:%s@%s", td.GetType(), relation,
							ttu.GetTupleset().GetRelation(), ttu.GetComputedUserset().GetRelation(), userType))
					}
				}
			}
		}
	}
	return keys, nil
}

func ttus(u *openfgav1.Userset) []*openfgav1.TupleToUserset {
	switch x := u.GetUserset().(type) {
	case *openfgav1.Userset_TupleToUserset:
		return []*openfgav1.TupleToUserset{x.TupleToUserset}
	case *openfgav1.Userset_Union:
		var out []*openfgav1.TupleToUserset
		for _, c := range x.Union.GetChild() {
			out = append(out, ttus(c)...)
		}
		return out
	case *openfgav1.Userset_Intersection:
		var out []*openfgav1.TupleToUserset
		for _, c := range x.Intersection.GetChild() {
			out = append(out, ttus(c)...)
		}
		return out
	case *openfgav1.Userset_Difference:
		return append(ttus(x.Difference.GetBase()), ttus(x.Difference.GetSubtract())...)
	}
	return nil
}

// rustRead is a datastore read requested by the evaluator.
type rustRead struct {
	ID       uint64 `json:"id"`
	Op       string `json:"op"`
	Object   string `json:"object"`
	Relation string `json:"relation"`
	User     string `json:"user"`
	Allowed  []struct {
		Type      string `json:"type"`
		Relation  string `json:"relation"`
		Wildcard  bool   `json:"wildcard"`
		Condition string `json:"condition"`
	} `json:"allowed"`
}

type rustAnswer struct {
	ID     uint64  `json:"id"`
	Tuples [][]any `json:"tuples"`
}

// answerReads performs one step's reads concurrently, so that pgx sessions
// pipeline them into one round trip.
func (s *Session) answerReads(ctx context.Context, req *openfgav1.CheckRequest, reads []rustRead) ([]byte, error) {
	answers := make([]rustAnswer, len(reads))
	errs := make([]error, len(reads))
	var wg sync.WaitGroup
	for i := range reads {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i].ID = reads[i].ID
			answers[i].Tuples, errs[i] = s.rustRead(ctx, req, &reads[i])
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, serverErrors.HandleError("", err)
	}
	return json.Marshal(answers)
}

func (s *Session) rustRead(ctx context.Context, req *openfgav1.CheckRequest, r *rustRead) ([][]any, error) {
	ds, store := s.engine.datastore, req.GetStoreId()
	consistency := storage.ConsistencyOptions{Preference: req.GetConsistency()}
	var iter storage.TupleIterator
	var err error
	switch r.Op {
	case "user":
		t, err := ds.ReadUserTuple(ctx, store, storage.ReadUserTupleFilter{Object: r.Object, Relation: r.Relation, User: r.User},
			storage.ReadUserTupleOptions{Consistency: consistency})
		if errors.Is(err, storage.ErrNotFound) {
			return [][]any{}, nil
		}
		if err != nil {
			return nil, err
		}
		row, err := tupleRow(t.GetKey())
		return [][]any{row}, err
	case "read":
		iter, err = ds.Read(ctx, store, storage.ReadFilter{Object: r.Object, Relation: r.Relation}, storage.ReadOptions{Consistency: consistency})
	case "usersets":
		refs := make([]*openfgav1.RelationReference, len(r.Allowed))
		for i, a := range r.Allowed {
			ref := &openfgav1.RelationReference{Type: a.Type, Condition: a.Condition}
			if a.Wildcard {
				ref.RelationOrWildcard = &openfgav1.RelationReference_Wildcard{Wildcard: &openfgav1.Wildcard{}}
			} else if a.Relation != "" {
				ref.RelationOrWildcard = &openfgav1.RelationReference_Relation{Relation: a.Relation}
			}
			refs[i] = ref
		}
		iter, err = ds.ReadUsersetTuples(ctx, store, storage.ReadUsersetTuplesFilter{Object: r.Object, Relation: r.Relation, AllowedUserTypeRestrictions: refs},
			storage.ReadUsersetTuplesOptions{Consistency: consistency})
	default:
		return nil, fmt.Errorf("unknown read %q", r.Op)
	}
	if err != nil {
		return nil, err
	}
	defer iter.Stop()
	rows := [][]any{}
	for {
		t, err := iter.Next(ctx)
		if errors.Is(err, storage.ErrIteratorDone) {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		row, err := tupleRow(t.GetKey())
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
}

// tupleRow encodes a tuple as [object, relation, user, condition, context].
func tupleRow(k *openfgav1.TupleKey) ([]any, error) {
	row := []any{k.GetObject(), k.GetRelation(), k.GetUser(), nil, nil}
	if c := k.GetCondition(); c != nil && c.GetName() != "" {
		row[3] = c.GetName()
		if c.GetContext() != nil {
			b, err := protojson.Marshal(c.GetContext())
			if err != nil {
				return nil, err
			}
			row[4] = json.RawMessage(b)
		}
	}
	return row, nil
}

// CompileOptions are the OpenFGA server settings that change which models
// are accepted.
type CompileOptions struct {
	// InlineExpressions is OpenFGA's "inline_expressions" experimental flag.
	InlineExpressions bool `json:"inline_expressions"`
}

// RustCompileModel decides with the Rust model compiler whether OpenFGA's
// WriteAuthorizationModel, configured as opts says, accepts req (its store
// ID aside). It returns nil if so, an error with OpenFGA's status code if
// not, and an error matching [ErrRustRefused] if the compiler cannot decide
// (a condition outside its CEL subset).
func RustCompileModel(req *openfgav1.WriteAuthorizationModelRequest, opts CompileOptions) error {
	js, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req)
	if err != nil {
		return serverErrors.HandleError("", err)
	}
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return serverErrors.HandleError("", err)
	}
	var verdict struct {
		Valid   bool `json:"valid"`
		Invalid *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"invalid"`
		Undecided *string `json:"undecided"`
	}
	if err := json.Unmarshal(take(C.afc_compile_model(ptr(js), C.size_t(len(js)), ptr(optsJSON), C.size_t(len(optsJSON)))), &verdict); err != nil {
		return serverErrors.HandleError("", err)
	}
	switch {
	case verdict.Valid:
		return nil
	case verdict.Undecided != nil:
		return refusedError{*verdict.Undecided}
	case verdict.Invalid == nil:
		return refusedError{"no verdict"}
	}
	switch verdict.Invalid.Code {
	case "invalid_argument":
		return status.Error(codes.InvalidArgument, verdict.Invalid.Message)
	case "exceeded_entity_limit":
		return status.Error(codes.Code(openfgav1.ErrorCode_exceeded_entity_limit), verdict.Invalid.Message)
	case "validation_error":
		return serverErrors.ValidationError(errors.New(verdict.Invalid.Message))
	}
	return serverErrors.InvalidAuthorizationModelInput(errors.New(verdict.Invalid.Message))
}
