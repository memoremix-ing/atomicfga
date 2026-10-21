package atomicfga

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	sq "github.com/Masterminds/squirrel"
	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	serverErrors "github.com/openfga/openfga/pkg/server/errors"

	"github.com/memoremix-ing/atomicfga/internal/bind"
)

// Session runs OpenFGA operations inside one application transaction. Its
// methods have the semantics of the corresponding OpenFGA API methods, except
// that their reads and writes happen in the bound transaction.
//
// Each tuple write runs in a savepoint: if it fails, its partial effects are
// rolled back and the transaction remains usable. Any other error from the
// database leaves the transaction in the state the database puts it in
// (PostgreSQL aborts it); roll back in that case.
type Session struct {
	openfgav1.UnimplementedOpenFGAServiceServer

	engine *Engine
	bind   *bind.Session

	mu      sync.Mutex
	visible map[string]bool // models confirmed visible in this transaction
	written map[string]bool // models written by this transaction
	writing int             // model writes in progress
}

var _ openfgav1.OpenFGAServiceServer = (*Session)(nil)

// Statements reports how many SQL statements the session has executed.
func (s *Session) Statements() int64 { return s.bind.Statements() }

// RoundTrips reports how many times the session called the transaction. It
// is lower than Statements when queries were pipelined.
func (s *Session) RoundTrips() int64 { return s.bind.RoundTrips() }

// call binds ctx to a new API call on the session. The returned function
// must be called when the API call returns; it waits for any statement the
// call still has in flight and discards the rest.
func (s *Session) call(ctx context.Context) (context.Context, func()) {
	c := s.bind.Call()
	return bind.WithCall(ctx, c), c.Finish
}

// modelRequest is an API request with an optional authorization model ID.
type modelRequest interface {
	proto.Message
	GetStoreId() string
	GetAuthorizationModelId() string
	Validate() error
}

// resolveModel pins the request to a model that is visible in this
// transaction before OpenFGA's process-wide model caches are consulted.
//
// Those caches (and the single-flight groups in front of them) are shared by
// every session, so they could otherwise serve a model that another
// transaction wrote but has not committed, or that was rolled back. Models
// are immutable and identified by unique ULIDs, so once a model ID is known
// to be visible, any cached copy of it is correct.
func resolveModel[R modelRequest](ctx context.Context, s *Session, req R) (R, error) {
	if err := req.Validate(); err != nil {
		return req, nil // let OpenFGA report the validation error
	}
	store, id := req.GetStoreId(), req.GetAuthorizationModelId()
	if id == "" {
		latest, err := s.latestModel(ctx, store)
		if err != nil {
			return req, err
		}
		if latest == "" {
			return req, serverErrors.LatestAuthorizationModelNotFound(store)
		}
		return withModelID(req, latest), nil
	}
	if _, err := ulid.Parse(id); err != nil {
		return req, nil // OpenFGA reports it as not found without a lookup
	}
	ok, err := s.modelVisible(ctx, store, id)
	if err != nil {
		return req, err
	}
	if !ok {
		return req, serverErrors.AuthorizationModelNotFound(id)
	}
	return req, nil
}

// withModelID returns a shallow copy of req with its model ID set, leaving
// the caller's request untouched.
func withModelID[R proto.Message](req R, id string) R {
	src := req.ProtoReflect()
	dst := src.New()
	src.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		dst.Set(fd, v)
		return true
	})
	dst.Set(dst.Descriptor().Fields().ByName("authorization_model_id"), protoreflect.ValueOfString(id))
	return dst.Interface().(R)
}

func (s *Session) latestModel(ctx context.Context, store string) (string, error) {
	var id string
	err := s.engine.stbl.
		Select("authorization_model_id").
		From("authorization_model").
		Where(sq.Eq{"store": store}).
		OrderBy("authorization_model_id desc").
		Limit(1).
		QueryRowContext(ctx).
		Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", serverErrors.HandleError("", err)
	}
	s.observeModel(store, id)
	return id, nil
}

func (s *Session) modelVisible(ctx context.Context, store, id string) (bool, error) {
	key := modelKey(store, id)
	if s.engine.committed.has(key) {
		return true, nil
	}
	s.mu.Lock()
	seen := s.visible[key]
	s.mu.Unlock()
	if seen {
		return true, nil
	}

	var one int
	err := s.engine.stbl.
		Select("1").
		From("authorization_model").
		Where(sq.Eq{"store": store, "authorization_model_id": id}).
		Limit(1).
		QueryRowContext(ctx).
		Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, serverErrors.HandleError("", err)
	}
	s.observeModel(store, id)
	return true, nil
}

// observeModel records a model that this transaction can see. A model the
// transaction did not write itself must have been committed by another one.
func (s *Session) observeModel(store, id string) {
	key := modelKey(store, id)
	s.mu.Lock()
	s.visible[key] = true
	ours := s.written[key] || s.writing > 0
	s.mu.Unlock()
	if !ours {
		s.engine.committed.add(key)
	}
}

// do runs one API call on the session.
func do[Req, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	ctx, done := s.call(ctx)
	defer done()
	resp, err := method(ctx, req)
	return resp, s.wrap(err)
}

// doModel runs one API call that resolves an authorization model.
func doModel[Req modelRequest, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	ctx, done := s.call(ctx)
	defer done()
	req, err := resolveModel(ctx, s, req)
	if err != nil {
		var zero Resp
		return zero, s.wrap(err)
	}
	resp, err := method(ctx, req)
	return resp, s.wrap(err)
}

// wrap replaces an error with [ErrTransactionAborted] when the database has
// aborted the transaction, whatever error OpenFGA made of it.
func (s *Session) wrap(err error) error {
	if err == nil {
		return nil
	}
	if aborted := s.bind.Aborted(); aborted != nil {
		return abortedError{aborted}
	}
	return err
}

// abortedError carries gRPC code Aborted, like OpenFGA's own transactional
// write conflicts, and matches [ErrTransactionAborted] with errors.Is.
type abortedError struct{ err error }

func (e abortedError) Error() string { return e.err.Error() }

func (e abortedError) Unwrap() error { return e.err }

func (e abortedError) GRPCStatus() *status.Status { return status.New(codes.Aborted, e.err.Error()) }

func (s *Session) Read(ctx context.Context, req *openfgav1.ReadRequest) (*openfgav1.ReadResponse, error) {
	return do(s, ctx, req, s.engine.server.Read)
}

func (s *Session) Write(ctx context.Context, req *openfgav1.WriteRequest) (*openfgav1.WriteResponse, error) {
	return doModel(s, ctx, req, s.engine.server.Write)
}

func (s *Session) Check(ctx context.Context, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	return doModel(s, ctx, req, s.engine.server.Check)
}

func (s *Session) BatchCheck(ctx context.Context, req *openfgav1.BatchCheckRequest) (*openfgav1.BatchCheckResponse, error) {
	return doModel(s, ctx, req, s.engine.server.BatchCheck)
}

func (s *Session) Expand(ctx context.Context, req *openfgav1.ExpandRequest) (*openfgav1.ExpandResponse, error) {
	return doModel(s, ctx, req, s.engine.server.Expand)
}

func (s *Session) ReadAuthorizationModels(ctx context.Context, req *openfgav1.ReadAuthorizationModelsRequest) (*openfgav1.ReadAuthorizationModelsResponse, error) {
	return do(s, ctx, req, s.engine.server.ReadAuthorizationModels)
}

func (s *Session) ReadAuthorizationModel(ctx context.Context, req *openfgav1.ReadAuthorizationModelRequest) (*openfgav1.ReadAuthorizationModelResponse, error) {
	return do(s, ctx, req, func(ctx context.Context, req *openfgav1.ReadAuthorizationModelRequest) (*openfgav1.ReadAuthorizationModelResponse, error) {
		if err := req.Validate(); err == nil {
			ok, err := s.modelVisible(ctx, req.GetStoreId(), req.GetId())
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, serverErrors.AuthorizationModelNotFound(req.GetId())
			}
		}
		return s.engine.server.ReadAuthorizationModel(ctx, req)
	})
}

func (s *Session) WriteAuthorizationModel(ctx context.Context, req *openfgav1.WriteAuthorizationModelRequest) (*openfgav1.WriteAuthorizationModelResponse, error) {
	return do(s, ctx, req, func(ctx context.Context, req *openfgav1.WriteAuthorizationModelRequest) (*openfgav1.WriteAuthorizationModelResponse, error) {
		// Until the new ID is recorded, nothing this session observes may be
		// treated as committed: it could be the model being written.
		s.mu.Lock()
		s.writing++
		s.mu.Unlock()
		resp, err := s.engine.server.WriteAuthorizationModel(ctx, req)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.writing--
		if err != nil {
			return nil, err
		}
		key := modelKey(req.GetStoreId(), resp.GetAuthorizationModelId())
		s.written[key] = true
		s.visible[key] = true
		return resp, nil
	})
}

func (s *Session) WriteAssertions(ctx context.Context, req *openfgav1.WriteAssertionsRequest) (*openfgav1.WriteAssertionsResponse, error) {
	return doModel(s, ctx, req, s.engine.server.WriteAssertions)
}

func (s *Session) ReadAssertions(ctx context.Context, req *openfgav1.ReadAssertionsRequest) (*openfgav1.ReadAssertionsResponse, error) {
	return doModel(s, ctx, req, s.engine.server.ReadAssertions)
}

func (s *Session) ReadChanges(ctx context.Context, req *openfgav1.ReadChangesRequest) (*openfgav1.ReadChangesResponse, error) {
	return do(s, ctx, req, s.engine.server.ReadChanges)
}

func (s *Session) CreateStore(ctx context.Context, req *openfgav1.CreateStoreRequest) (*openfgav1.CreateStoreResponse, error) {
	return do(s, ctx, req, s.engine.server.CreateStore)
}

func (s *Session) UpdateStore(ctx context.Context, req *openfgav1.UpdateStoreRequest) (*openfgav1.UpdateStoreResponse, error) {
	return do(s, ctx, req, s.engine.server.UpdateStore)
}

func (s *Session) DeleteStore(ctx context.Context, req *openfgav1.DeleteStoreRequest) (*openfgav1.DeleteStoreResponse, error) {
	return do(s, ctx, req, s.engine.server.DeleteStore)
}

func (s *Session) GetStore(ctx context.Context, req *openfgav1.GetStoreRequest) (*openfgav1.GetStoreResponse, error) {
	return do(s, ctx, req, s.engine.server.GetStore)
}

func (s *Session) ListStores(ctx context.Context, req *openfgav1.ListStoresRequest) (*openfgav1.ListStoresResponse, error) {
	return do(s, ctx, req, s.engine.server.ListStores)
}

func (s *Session) ListObjects(ctx context.Context, req *openfgav1.ListObjectsRequest) (*openfgav1.ListObjectsResponse, error) {
	return doModel(s, ctx, req, s.engine.server.ListObjects)
}

func (s *Session) StreamedListObjects(req *openfgav1.StreamedListObjectsRequest, srv openfgav1.OpenFGAService_StreamedListObjectsServer) error {
	_, err := doModel(s, srv.Context(), req, func(ctx context.Context, req *openfgav1.StreamedListObjectsRequest) (struct{}, error) {
		return struct{}{}, s.engine.server.StreamedListObjects(req, streamContext{srv, ctx})
	})
	return err
}

func (s *Session) ListUsers(ctx context.Context, req *openfgav1.ListUsersRequest) (*openfgav1.ListUsersResponse, error) {
	return doModel(s, ctx, req, s.engine.server.ListUsers)
}

type streamContext struct {
	openfgav1.OpenFGAService_StreamedListObjectsServer
	ctx context.Context
}

func (s streamContext) Context() context.Context { return s.ctx }
