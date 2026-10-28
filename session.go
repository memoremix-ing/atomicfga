package atomicfga

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	sq "github.com/Masterminds/squirrel"
	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

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
	tx     Tx
	*txState
}

// txState is shared by every Session of one transaction. It does not
// reference the transaction, so the engine can keep it exactly as long as
// the transaction is reachable.
type txState struct {
	bind *bind.Session
}

func newTxState() *txState {
	return &txState{bind: bind.NewSession()}
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
func (s *Session) call(ctx context.Context) (context.Context, *bind.Call) {
	c := s.bind.Call(s.tx)
	return bind.WithCall(ctx, c), c
}

// modelRequest is an API request with an optional authorization model ID.
type modelRequest interface {
	proto.Message
	GetStoreId() string
	GetAuthorizationModelId() string
	Validate() error
}

// resolveModel requires the request to name a model ([ErrModelIDRequired])
// and checks that the model is visible in this transaction before OpenFGA's
// process-wide model caches are consulted.
//
// Those caches (and the single-flight groups in front of them) are shared by
// every session, so they could otherwise serve a model that another
// transaction wrote but has not committed, or that was rolled back. Models
// are immutable and identified by unique ULIDs, so once a model ID is known
// to be visible, any cached copy of it is correct.
//
// Visibility is queried in the transaction on every request. Neither another
// transaction's observation (this one's snapshot may predate the commit) nor
// an earlier one in this transaction (the caller may since have rolled back
// to a savepoint) can stand in for it.
func resolveModel[R modelRequest](ctx context.Context, s *Session, req R) (R, error) {
	if err := req.Validate(); err != nil {
		return req, nil // let OpenFGA report the validation error
	}
	store, id := req.GetStoreId(), req.GetAuthorizationModelId()
	if id == "" {
		return req, ErrModelIDRequired
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
	return id, nil
}

func (s *Session) visibilityQuery(store, id string) sq.SelectBuilder {
	return s.engine.stbl.
		Select("1").
		From("authorization_model").
		Where(sq.Eq{"store": store, "authorization_model_id": id}).
		Limit(1)
}

// modelVisible reports whether the model exists in the bound transaction.
func (s *Session) modelVisible(ctx context.Context, store, id string) (bool, error) {
	var one int
	err := s.visibilityQuery(store, id).QueryRowContext(ctx).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, serverErrors.HandleError("", err)
	}
	return true, nil
}

// do runs one API call on the session.
func do[Req, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	ctx, c := s.call(ctx)
	defer c.Finish()
	resp, err := method(ctx, req)
	return resp, s.wrap(err)
}

// doModel runs one API call that resolves an authorization model.
func doModel[Req modelRequest, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	ctx, c := s.call(ctx)
	defer c.Finish()
	return resolveAndRun(s, ctx, req, method)
}

func resolveAndRun[Req modelRequest, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	req, err := resolveModel(ctx, s, req)
	if err != nil {
		var zero Resp
		return zero, s.wrap(err)
	}
	resp, err := method(ctx, req)
	return resp, s.wrap(err)
}

// doRead runs one API call that resolves an authorization model and has no
// effects. When it can, it runs the call while the model's visibility query
// is still on its way (see [Session.speculate]).
func doRead[Req modelRequest, Resp any](s *Session, ctx context.Context, req Req, method func(context.Context, Req) (Resp, error)) (Resp, error) {
	ctx, c := s.call(ctx)
	defer c.Finish()
	pinned, visible, ok := s.speculate(ctx, c, req)
	if !ok {
		return resolveAndRun(s, ctx, req, method)
	}
	resp, err := method(pinned, req)
	if verr := visible(); verr != nil {
		var zero Resp
		return zero, s.wrap(verr)
	}
	return resp, s.wrap(err)
}

// speculate prepares a read-only call to run before its model's visibility
// is known, saving the round trip the visibility query would otherwise take
// on its own. It does so only when the call names a model whose content is
// known, and the backend pipelines: the visibility query is then deferred to
// ride along with the call's first statements. The call runs with the model
// pinned to its context, so OpenFGA never reads the model in a transaction
// that may not see it (see [modelDatastore]); a call whose model turns out
// not to be visible returns authorization_model_not_found, as if it had not
// run.
func (s *Session) speculate(ctx context.Context, c *bind.Call, req modelRequest) (context.Context, func() error, bool) {
	if req.Validate() != nil {
		return nil, nil, false
	}
	store, id := req.GetStoreId(), req.GetAuthorizationModelId()
	if id == "" {
		return nil, nil, false
	}
	if _, err := ulid.Parse(id); err != nil {
		return nil, nil, false
	}
	m := s.engine.models.get(store, id)
	if m == nil {
		return nil, nil, false
	}
	query, args, err := s.visibilityQuery(store, id).ToSql()
	if err != nil {
		return nil, nil, false
	}
	await, ok := c.Defer(ctx, query, args)
	if !ok {
		return nil, nil, false
	}
	return withPinnedModel(ctx, store, id, m), func() error {
		res, err := await()
		if err != nil {
			return serverErrors.HandleError("", err)
		}
		if len(res.Rows) == 0 {
			return serverErrors.AuthorizationModelNotFound(id)
		}
		return nil
	}, true
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
	return doRead(s, ctx, req, s.engine.server.Check)
}

func (s *Session) BatchCheck(ctx context.Context, req *openfgav1.BatchCheckRequest) (*openfgav1.BatchCheckResponse, error) {
	return doRead(s, ctx, req, s.engine.server.BatchCheck)
}

func (s *Session) Expand(ctx context.Context, req *openfgav1.ExpandRequest) (*openfgav1.ExpandResponse, error) {
	return doRead(s, ctx, req, s.engine.server.Expand)
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

// WriteAuthorizationModel writes a model as OpenFGA does, except that its ID
// is always greater than every model ID of the store the transaction sees.
// OpenFGA mints the ID from the clock, and the latest model is the one with
// the greatest ID; when the clock is behind the one that minted the store's
// latest model (another server's, or this one's before it was set back), the
// new model would rank below it, and requests naming no model would keep
// using the old one. Such an ID is replaced by the latest's millisecond plus
// one, with fresh entropy; the response names the ID stored.
//
// On PostgreSQL, model writes to one store also take a transaction-scoped
// advisory lock, so a second transaction writing a model to the store waits
// for the first to end, then sees its model (at READ COMMITTED, the default;
// SERIALIZABLE aborts one of them instead; REPEATABLE READ does neither,
// so write models at READ COMMITTED). SQLite runs one writer at a time.
func (s *Session) WriteAuthorizationModel(ctx context.Context, req *openfgav1.WriteAuthorizationModelRequest) (*openfgav1.WriteAuthorizationModelResponse, error) {
	return do(s, ctx, req, s.writeAuthorizationModel)
}

func (s *Session) writeAuthorizationModel(ctx context.Context, req *openfgav1.WriteAuthorizationModelRequest) (*openfgav1.WriteAuthorizationModelResponse, error) {
	if req.Validate() != nil {
		return s.engine.server.WriteAuthorizationModel(ctx, req) // let OpenFGA report the validation error
	}
	store := req.GetStoreId()
	if s.engine.dialect == Postgres {
		_, err := s.engine.stbl.Select().
			Column(sq.Expr("pg_advisory_xact_lock(hashtextextended(?, 0))", "atomicfga/authorization_model/"+store)).
			ExecContext(ctx)
		if err != nil {
			return nil, serverErrors.HandleError("", err)
		}
	}
	latest, err := s.latestModel(ctx, store)
	if err != nil {
		return nil, err
	}
	if latest != "" {
		floor, err := ulid.ParseStrict(latest)
		if err != nil {
			return nil, serverErrors.HandleError("", fmt.Errorf("store %s: latest model ID %q: %w", store, latest, err))
		}
		ctx = withModelFloor(ctx, store, floor)
	}
	resp, err := s.engine.server.WriteAuthorizationModel(ctx, req)
	if err == nil && resp.GetAuthorizationModelId() <= latest {
		// Unreachable unless OpenFGA stops answering with the ID it stored.
		return nil, status.Errorf(codes.Internal, "atomicfga: model %s was written below the store's latest model %s; roll back", resp.GetAuthorizationModelId(), latest)
	}
	return resp, err
}

func (s *Session) WriteAssertions(ctx context.Context, req *openfgav1.WriteAssertionsRequest) (*openfgav1.WriteAssertionsResponse, error) {
	return doModel(s, ctx, req, s.engine.server.WriteAssertions)
}

func (s *Session) ReadAssertions(ctx context.Context, req *openfgav1.ReadAssertionsRequest) (*openfgav1.ReadAssertionsResponse, error) {
	return doRead(s, ctx, req, s.engine.server.ReadAssertions)
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
	return doRead(s, ctx, req, s.engine.server.ListObjects)
}

func (s *Session) StreamedListObjects(req *openfgav1.StreamedListObjectsRequest, srv openfgav1.OpenFGAService_StreamedListObjectsServer) error {
	_, err := doModel(s, srv.Context(), req, func(ctx context.Context, req *openfgav1.StreamedListObjectsRequest) (struct{}, error) {
		return struct{}{}, s.engine.server.StreamedListObjects(req, streamContext{srv, ctx})
	})
	return err
}

func (s *Session) ListUsers(ctx context.Context, req *openfgav1.ListUsersRequest) (*openfgav1.ListUsersResponse, error) {
	return doRead(s, ctx, req, s.engine.server.ListUsers)
}

type streamContext struct {
	openfgav1.OpenFGAService_StreamedListObjectsServer
	ctx context.Context
}

func (s streamContext) Context() context.Context { return s.ctx }
