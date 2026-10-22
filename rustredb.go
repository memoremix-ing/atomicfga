//go:build rustcheck

package atomicfga

/*
#include <stddef.h>
#include <stdint.h>

typedef struct { uint8_t *ptr; size_t len; size_t cap; } afc_buf;
typedef struct afc_redb afc_redb;

afc_redb *afc_redb_new(void);
void afc_redb_free(afc_redb *h);
afc_buf afc_redb_write_model(const afc_redb *h, const uint8_t *store, size_t store_len, const uint8_t *id, size_t id_len, const uint8_t *json, size_t len);
afc_buf afc_redb_write(const afc_redb *h, const uint8_t *store, size_t store_len, const uint8_t *json, size_t len);
afc_buf afc_redb_check(const afc_redb *h, const uint8_t *store, size_t store_len, const uint8_t *json, size_t len);
void afc_buf_free(afc_buf b);
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	openfgav1 "github.com/openfga/api/proto/openfga/v1"
)

// RedbStore is an in-memory redb database served by the Rust port alone
// (models, writes and checks), each call in a redb transaction of its own.
// It exists to hold the redb adapter to OpenFGA's behavior in conformance
// tests; applications embed the Rust crate (feature "redb") directly.
type RedbStore struct{ h *C.afc_redb }

// NewRedbStore opens an empty in-memory store. Close it when done.
func NewRedbStore() *RedbStore { return &RedbStore{C.afc_redb_new()} }

// Close releases the store.
func (r *RedbStore) Close() {
	if r.h != nil {
		C.afc_redb_free(r.h)
		r.h = nil
	}
}

func redbBytes(b []byte) (*C.uint8_t, C.size_t) {
	if len(b) == 0 {
		return nil, 0
	}
	return (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(b))), C.size_t(len(b))
}

func redbTake(b C.afc_buf) []byte {
	defer C.afc_buf_free(b)
	return C.GoBytes(unsafe.Pointer(b.ptr), C.int(b.len))
}

// redbResult decodes a response: an OpenFGA error code becomes a status
// error with that code, a refusal [ErrRustRefused].
func redbResult(out []byte) (map[string]any, error) {
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, err
	}
	if r, ok := v["refused"].(string); ok {
		return nil, refusedError{r}
	}
	e, ok := v["error"].(map[string]any)
	if !ok {
		return v, nil
	}
	name, _ := e["code"].(string)
	msg, _ := e["message"].(string)
	switch name {
	case "invalid_argument":
		return nil, status.Error(codes.InvalidArgument, msg)
	case "aborted":
		return nil, status.Error(codes.Aborted, msg)
	}
	if code, ok := openfgav1.ErrorCode_value[name]; ok {
		return nil, status.Error(codes.Code(code), msg)
	}
	if code, ok := openfgav1.InternalErrorCode_value[name]; ok {
		return nil, status.Error(codes.Code(code), msg)
	}
	return nil, fmt.Errorf("unknown error code %q: %s", name, msg)
}

func marshal(m proto.Message) ([]byte, error) {
	return protojson.MarshalOptions{UseProtoNames: true}.Marshal(m)
}

// WriteAuthorizationModel stores a model under the given ID (a ULID).
func (r *RedbStore) WriteAuthorizationModel(store, id string, req *openfgav1.WriteAuthorizationModelRequest) error {
	js, err := marshal(req)
	if err != nil {
		return err
	}
	sp, sl := redbBytes([]byte(store))
	ip, il := redbBytes([]byte(id))
	jp, jl := redbBytes(js)
	_, err = redbResult(redbTake(C.afc_redb_write_model(r.h, sp, sl, ip, il, jp, jl)))
	return err
}

// Write applies a WriteRequest.
func (r *RedbStore) Write(store string, req *openfgav1.WriteRequest) error {
	js, err := marshal(req)
	if err != nil {
		return err
	}
	sp, sl := redbBytes([]byte(store))
	jp, jl := redbBytes(js)
	_, err = redbResult(redbTake(C.afc_redb_write(r.h, sp, sl, jp, jl)))
	return err
}

// Check answers a CheckRequest in OpenFGA semantics.
func (r *RedbStore) Check(store string, req *openfgav1.CheckRequest) (*openfgav1.CheckResponse, error) {
	js, err := marshal(req)
	if err != nil {
		return nil, err
	}
	sp, sl := redbBytes([]byte(store))
	jp, jl := redbBytes(js)
	v, err := redbResult(redbTake(C.afc_redb_check(r.h, sp, sl, jp, jl)))
	if err != nil {
		return nil, err
	}
	allowed, ok := v["allowed"].(bool)
	if !ok {
		return nil, errors.New("no answer")
	}
	return &openfgav1.CheckResponse{Allowed: allowed}, nil
}
