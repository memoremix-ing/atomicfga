package atomicfga_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
	parser "github.com/openfga/language/pkg/go/transformer"

	"github.com/memoremix-ing/atomicfga"
	"github.com/memoremix-ing/atomicfga/internal/testdb"
)

// writeModelAt stores the fixture's model under an ID minted at t, as a
// server whose clock reads t would: straight to the datastore.
func (f *fixture) writeModelAt(s *atomicfga.Session, t time.Time) string {
	f.t.Helper()
	m := parser.MustTransformDSLToProto(docModel)
	m.Id = ulid.MustNew(ulid.Timestamp(t), ulid.DefaultEntropy()).String()
	ctx, done := s.Bind(context.Background())
	defer done()
	if err := f.engine.Datastore().WriteAuthorizationModel(ctx, f.store, m); err != nil {
		f.t.Fatal(err)
	}
	return m.GetId()
}

// latestModel is the model ReadAuthorizationModels lists first.
func (f *fixture) latestModel(s *atomicfga.Session) string {
	f.t.Helper()
	r, err := s.ReadAuthorizationModels(context.Background(), &openfgav1.ReadAuthorizationModelsRequest{StoreId: f.store})
	if err != nil {
		f.t.Fatal(err)
	}
	return r.GetAuthorizationModels()[0].GetId()
}

func TestModelIDsIncreaseWhenTheClockIsBehind(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			// A model from a server whose clock is an hour ahead of ours.
			ahead := f.writeModelAt(s, time.Now().Add(time.Hour))
			v2 := f.writeModel(s, docModelV2)
			if v2 <= ahead {
				return fmt.Errorf("new model %s ranks below %s", v2, ahead)
			}
			if got, want := ulid.MustParse(v2).Time(), ulid.MustParse(ahead).Time()+1; got != want {
				return fmt.Errorf("new model's time is %d, want the latest's plus one: %d", got, want)
			}
			if got := f.latestModel(s); got != v2 {
				return fmt.Errorf("latest model is %s, want the one just written, %s", got, v2)
			}
			// It serves requests and reads back under the ID the response
			// named (only v2 has editor).
			if _, err := f.check(s, v2, "doc:1", "editor", "user:anne"); err != nil {
				return err
			}
			if _, err := s.ReadAuthorizationModel(context.Background(), &openfgav1.ReadAuthorizationModelRequest{StoreId: f.store, Id: v2}); err != nil {
				return err
			}
			// A clock that is not behind keeps the ID OpenFGA minted.
			before := ulid.Now()
			v3 := f.writeModel(s, docModelV2)
			if ulid.MustParse(v3).Time() <= ulid.MustParse(v2).Time() || ulid.MustParse(v3).Time() < before {
				return fmt.Errorf("model %s was not minted from the clock", v3)
			}
			return nil
		})
	})
}

// Two transactions writing models to one store: the second waits for the
// first to end, then ranks its model above the first's, however their
// clocks compare.
func TestConcurrentModelWritesRankInCommitOrder(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		if db.Dialect != atomicfga.Postgres {
			t.Skip("SQLite runs one writer at a time")
		}
		f := newFixture(t, db, pgx)
		first, _, commit, rollback := f.begin()
		t.Cleanup(func() { _ = rollback() }) // after a commit, a no-op error
		f.writeModel(first, docModel)

		type result struct {
			id  string
			err error
		}
		done := make(chan result, 1)
		second, _, commitSecond, rollbackSecond := f.begin()
		t.Cleanup(func() { _ = rollbackSecond() })
		go func() {
			m := parser.MustTransformDSLToProto(docModelV2)
			r, err := second.WriteAuthorizationModel(context.Background(), &openfgav1.WriteAuthorizationModelRequest{
				StoreId: f.store, SchemaVersion: m.GetSchemaVersion(), TypeDefinitions: m.GetTypeDefinitions(),
			})
			done <- result{r.GetAuthorizationModelId(), err}
		}()
		select {
		case r := <-done:
			_ = rollbackSecond()
			t.Fatalf("second model write did not wait for the first transaction: %+v", r)
		case <-time.After(200 * time.Millisecond):
		}
		// The first transaction also writes a model from a clock an hour
		// ahead, then commits.
		ahead := f.writeModelAt(first, time.Now().Add(time.Hour))
		if err := commit(); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil {
			_ = rollbackSecond()
			t.Fatal(r.err)
		}
		if err := commitSecond(); err != nil {
			t.Fatal(err)
		}
		if r.id <= ahead {
			t.Fatalf("second transaction's model %s ranks below the first's %s", r.id, ahead)
		}
	})
}

func TestModelIDRequired(t *testing.T) {
	each(t, func(t *testing.T, db *testdb.DB, pgx bool) {
		f := newFixture(t, db, pgx)
		ctx := context.Background()
		f.tx(func(s *atomicfga.Session, _ atomicfga.Tx) error {
			key := &openfgav1.CheckRequestTupleKey{Object: "doc:1", Relation: "viewer", User: "user:anne"}
			calls := map[string]func() error{
				"Write": func() error {
					_, err := s.Write(ctx, &openfgav1.WriteRequest{StoreId: f.store, Writes: &openfgav1.WriteRequestWrites{TupleKeys: []*openfgav1.TupleKey{tk("doc:1", "owner", "user:anne")}}})
					return err
				},
				"Check": func() error {
					_, err := s.Check(ctx, &openfgav1.CheckRequest{StoreId: f.store, TupleKey: key})
					return err
				},
				"BatchCheck": func() error {
					_, err := s.BatchCheck(ctx, &openfgav1.BatchCheckRequest{StoreId: f.store, Checks: []*openfgav1.BatchCheckItem{{TupleKey: key, CorrelationId: "1"}}})
					return err
				},
				"Expand": func() error {
					_, err := s.Expand(ctx, &openfgav1.ExpandRequest{StoreId: f.store, TupleKey: &openfgav1.ExpandRequestTupleKey{Object: "doc:1", Relation: "viewer"}})
					return err
				},
				"ListObjects": func() error {
					_, err := s.ListObjects(ctx, &openfgav1.ListObjectsRequest{StoreId: f.store, Type: "doc", Relation: "viewer", User: "user:anne"})
					return err
				},
				"StreamedListObjects": func() error {
					return s.StreamedListObjects(&openfgav1.StreamedListObjectsRequest{StoreId: f.store, Type: "doc", Relation: "viewer", User: "user:anne"}, nullStream{ctx})
				},
				"ListUsers": func() error {
					_, err := s.ListUsers(ctx, &openfgav1.ListUsersRequest{StoreId: f.store, Object: &openfgav1.Object{Type: "doc", Id: "1"}, Relation: "viewer",
						UserFilters: []*openfgav1.UserTypeFilter{{Type: "user"}}})
					return err
				},
			}
			for name, call := range calls {
				if err := call(); !errors.Is(err, atomicfga.ErrModelIDRequired) || status.Code(err) != codes.InvalidArgument {
					return fmt.Errorf("%s naming no model: got %v, want ErrModelIDRequired", name, err)
				}
			}
			// An invalid request still gets OpenFGA's own error.
			if _, err := s.Check(ctx, &openfgav1.CheckRequest{StoreId: f.store}); errors.Is(err, atomicfga.ErrModelIDRequired) || err == nil {
				return fmt.Errorf("invalid check: %v", err)
			}
			return nil
		})
	})
}

// nullStream discards a StreamedListObjects response.
type nullStream struct{ ctx context.Context }

func (s nullStream) Send(*openfgav1.StreamedListObjectsResponse) error { return nil }
func (s nullStream) SetHeader(metadata.MD) error                       { return nil }
func (s nullStream) SendHeader(metadata.MD) error                      { return nil }
func (s nullStream) SetTrailer(metadata.MD)                            {}
func (s nullStream) Context() context.Context                          { return s.ctx }
func (s nullStream) SendMsg(any) error                                 { return nil }
func (s nullStream) RecvMsg(any) error                                 { return nil }
