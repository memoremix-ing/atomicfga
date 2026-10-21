package tests

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"

	"github.com/openfga/openfga/pkg/server"
	upstreamtests "github.com/openfga/openfga/tests"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// client adapts atomicfga to the client interface used by OpenFGA's own
// integration suites. Every call runs in a new application transaction that
// is committed when the call succeeds and rolled back when it fails.
type client struct {
	engine  *atomicfga.Engine
	db      *testdb.DB
	pgx     bool
	retries atomic.Int64
}

var _ upstreamtests.ClientInterface = (*client)(nil)

func newClient(t *testing.T, db *testdb.DB, pgx bool, opts ...server.OpenFGAServiceV1Option) *client {
	t.Helper()
	e, err := atomicfga.New(db.Dialect, atomicfga.WithServerOptions(opts...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return &client{engine: e, db: db, pgx: pgx}
}

// do runs fn in a transaction, retrying it like an application would when
// the database aborts the transaction (deadlocks, serialization failures).
func (c *client) do(ctx context.Context, fn func(*atomicfga.Session) error) error {
	for attempt := 1; ; attempt++ {
		err := c.once(ctx, fn)
		if !errors.Is(err, atomicfga.ErrTransactionAborted) || attempt == 20 {
			return err
		}
		c.retries.Add(1)
		time.Sleep(time.Duration(rand.IntN(attempt*5)) * time.Millisecond)
	}
}

func (c *client) once(ctx context.Context, fn func(*atomicfga.Session) error) error {
	tx, commit, rollback, err := c.db.BeginTx(ctx, c.pgx)
	if err != nil {
		return err
	}
	if err := fn(c.engine.Session(tx)); err != nil {
		_ = rollback()
		return err
	}
	return commit()
}

func call[Req, Resp any](c *client, ctx context.Context, method func(*atomicfga.Session, context.Context, Req) (Resp, error), req Req) (resp Resp, err error) {
	err = c.do(ctx, func(s *atomicfga.Session) error {
		resp, err = method(s, ctx, req)
		return err
	})
	return resp, err
}

func (c *client) CreateStore(ctx context.Context, in *openfgav1.CreateStoreRequest, _ ...grpc.CallOption) (*openfgav1.CreateStoreResponse, error) {
	return call(c, ctx, (*atomicfga.Session).CreateStore, in)
}

func (c *client) WriteAuthorizationModel(ctx context.Context, in *openfgav1.WriteAuthorizationModelRequest, _ ...grpc.CallOption) (*openfgav1.WriteAuthorizationModelResponse, error) {
	return call(c, ctx, (*atomicfga.Session).WriteAuthorizationModel, in)
}

func (c *client) Write(ctx context.Context, in *openfgav1.WriteRequest, _ ...grpc.CallOption) (*openfgav1.WriteResponse, error) {
	return call(c, ctx, (*atomicfga.Session).Write, in)
}

func (c *client) Check(ctx context.Context, in *openfgav1.CheckRequest, _ ...grpc.CallOption) (*openfgav1.CheckResponse, error) {
	return call(c, ctx, (*atomicfga.Session).Check, in)
}

func (c *client) ListUsers(ctx context.Context, in *openfgav1.ListUsersRequest, _ ...grpc.CallOption) (*openfgav1.ListUsersResponse, error) {
	return call(c, ctx, (*atomicfga.Session).ListUsers, in)
}

func (c *client) ListObjects(ctx context.Context, in *openfgav1.ListObjectsRequest, _ ...grpc.CallOption) (*openfgav1.ListObjectsResponse, error) {
	return call(c, ctx, (*atomicfga.Session).ListObjects, in)
}

func (c *client) StreamedListObjects(ctx context.Context, in *openfgav1.StreamedListObjectsRequest, _ ...grpc.CallOption) (openfgav1.OpenFGAService_StreamedListObjectsClient, error) {
	srv := &collectStream{ctx: ctx}
	err := c.do(ctx, func(s *atomicfga.Session) error {
		return s.StreamedListObjects(in, srv)
	})
	return &replayStream{ctx: ctx, items: srv.items, err: err}, nil
}

// collectStream is the server side of an in-process streaming call.
type collectStream struct {
	ctx   context.Context
	items []*openfgav1.StreamedListObjectsResponse
}

func (s *collectStream) Send(r *openfgav1.StreamedListObjectsResponse) error {
	s.items = append(s.items, r)
	return nil
}
func (s *collectStream) SetHeader(metadata.MD) error  { return nil }
func (s *collectStream) SendHeader(metadata.MD) error { return nil }
func (s *collectStream) SetTrailer(metadata.MD)       {}
func (s *collectStream) Context() context.Context     { return s.ctx }
func (s *collectStream) SendMsg(any) error            { return nil }
func (s *collectStream) RecvMsg(any) error            { return io.EOF }

// replayStream is the client side: it yields the collected responses, then
// the call's error (or io.EOF).
type replayStream struct {
	ctx   context.Context
	items []*openfgav1.StreamedListObjectsResponse
	err   error
}

func (s *replayStream) Recv() (*openfgav1.StreamedListObjectsResponse, error) {
	if len(s.items) > 0 {
		r := s.items[0]
		s.items = s.items[1:]
		return r, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}
func (s *replayStream) Header() (metadata.MD, error) { return nil, nil }
func (s *replayStream) Trailer() metadata.MD         { return nil }
func (s *replayStream) CloseSend() error             { return nil }
func (s *replayStream) Context() context.Context     { return s.ctx }
func (s *replayStream) SendMsg(any) error            { return nil }
func (s *replayStream) RecvMsg(any) error            { return io.EOF }
