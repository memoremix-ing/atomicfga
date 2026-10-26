//! C ABI over `atomicfga-check` for hosts (atomicfga links it through cgo). Every message is JSON.
//! A panic never crosses the boundary: it becomes a refusal.

use atomicfga_check::{Config, Engine, Task};
use serde_json::{json, Value};
use std::panic::{catch_unwind, AssertUnwindSafe};

/// A byte buffer owned by Rust; release it with `afc_buf_free`.
#[repr(C)]
pub struct AfcBuf {
    pub ptr: *mut u8,
    pub len: usize,
    pub cap: usize,
}

fn buf(v: Value) -> AfcBuf {
    let mut bytes = std::mem::ManuallyDrop::new(serde_json::to_vec(&v).unwrap_or_default());
    AfcBuf {
        ptr: bytes.as_mut_ptr(),
        len: bytes.len(),
        cap: bytes.capacity(),
    }
}

unsafe fn bytes<'a>(ptr: *const u8, len: usize) -> &'a [u8] {
    if len == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(ptr, len)
    }
}

fn guarded(f: impl FnOnce() -> Value) -> Value {
    catch_unwind(AssertUnwindSafe(f))
        .unwrap_or_else(|_| json!({"refused": "panic in the evaluator"}))
}

/// An engine whose checks follow `config` (a [`atomicfga_check::Config`] in
/// JSON, every field required). Returns null if `config` does not decode.
///
/// # Safety
/// `config` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_engine_new(config: *const u8, len: usize) -> *mut Engine {
    match serde_json::from_slice::<Config>(bytes(config, len)) {
        Ok(c) => Box::into_raw(Box::new(Engine::new(c))),
        Err(_) => std::ptr::null_mut(),
    }
}

/// # Safety
/// `e` must come from `afc_engine_new`, with no task of it still alive.
#[no_mangle]
pub unsafe extern "C" fn afc_engine_free(e: *mut Engine) {
    if !e.is_null() {
        drop(Box::from_raw(e));
    }
}

/// # Safety
/// `e` must be a live engine; `id` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_engine_has_model(e: *const Engine, id: *const u8, len: usize) -> bool {
    let id = std::str::from_utf8(bytes(id, len)).unwrap_or("");
    (*e).has_model(id)
}

/// Loads a model (protojson, proto field names). Returns `{"id": ...}` or
/// `{"error": ...}`.
///
/// # Safety
/// `e` must be a live engine; `json` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_engine_load_model(
    e: *const Engine,
    json: *const u8,
    len: usize,
) -> AfcBuf {
    let json = bytes(json, len);
    buf(guarded(|| match (*e).load_model(json) {
        Ok(id) => json!({"id": id}),
        Err(err) => json!({"error": err}),
    }))
}

/// Starts a Check of a CheckRequest (protojson, proto field names).
///
/// # Safety
/// `e` must be a live engine; the pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_check_new(
    e: *const Engine,
    model: *const u8,
    model_len: usize,
    req: *const u8,
    req_len: usize,
) -> *mut Task {
    let model = std::str::from_utf8(bytes(model, model_len)).unwrap_or("");
    let req = bytes(req, req_len);
    match catch_unwind(AssertUnwindSafe(|| (*e).check(model, req))) {
        Ok(t) => Box::into_raw(Box::new(t)),
        Err(_) => std::ptr::null_mut(),
    }
}

/// Answers reads and advances the task (see `Task::step`).
///
/// # Safety
/// `t` must come from `afc_check_new`; `answers` must point to `len`
/// readable bytes. A task must not be stepped concurrently.
#[no_mangle]
pub unsafe extern "C" fn afc_check_step(t: *mut Task, answers: *const u8, len: usize) -> AfcBuf {
    let answers = bytes(answers, len);
    let task = &mut *t;
    buf(guarded(|| task.step(answers)))
}

/// # Safety
/// `t` must come from `afc_check_new` and not be used afterwards.
#[no_mangle]
pub unsafe extern "C" fn afc_check_free(t: *mut Task) {
    if !t.is_null() {
        drop(Box::from_raw(t));
    }
}

/// # Safety
/// `b` must come from this library and not be used afterwards.
#[no_mangle]
pub unsafe extern "C" fn afc_buf_free(b: AfcBuf) {
    if !b.ptr.is_null() {
        drop(Vec::from_raw_parts(b.ptr, b.len, b.cap));
    }
}

/// Decides whether OpenFGA accepts a model (a WriteAuthorizationModelRequest
/// body in protojson) under `options` (`{"inline_expressions": bool}`, or
/// empty). Returns `{"valid": true}`, `{"invalid": {"code": ...,
/// "message": ...}}` or `{"undecided": "..."}`.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_compile_model(
    json: *const u8,
    len: usize,
    options: *const u8,
    options_len: usize,
) -> AfcBuf {
    let json = bytes(json, len);
    let options = bytes(options, options_len);
    buf(guarded(|| match serde_json::from_slice(options) {
        Ok(o) => atomicfga_check::compile_json(json, o),
        Err(_) if options.is_empty() => atomicfga_check::compile_json(json, Default::default()),
        Err(e) => json!({"undecided": format!("options: {e}")}),
    }))
}

/// The strategy table derived for a model (protojson): `{"alternatives":
/// [...]}` or `{"error": ...}`.
///
/// # Safety
/// `json` must point to `len` readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_model_alternatives(json: *const u8, len: usize) -> AfcBuf {
    let json = bytes(json, len);
    buf(guarded(|| atomicfga_check::alternatives_json(json)))
}

/// An in-memory redb database with an [`atomicfga_check::api::Fga`], for hosts
/// and conformance tests. Each call runs in a transaction of its own, which
/// it commits only if the call succeeds.
pub struct RedbHandle {
    db: redb::Database,
    fga: atomicfga_check::api::Fga,
}

#[no_mangle]
pub extern "C" fn afc_redb_new() -> *mut RedbHandle {
    let db = redb::Database::builder()
        .create_with_backend(redb::backends::InMemoryBackend::new())
        .expect("in-memory redb");
    Box::into_raw(Box::new(RedbHandle {
        db,
        fga: Default::default(),
    }))
}

/// # Safety
/// `h` must come from `afc_redb_new` and not be used afterwards.
#[no_mangle]
pub unsafe extern "C" fn afc_redb_free(h: *mut RedbHandle) {
    if !h.is_null() {
        drop(Box::from_raw(h));
    }
}

fn in_write(h: &RedbHandle, f: impl FnOnce(&redb::WriteTransaction) -> Value) -> Value {
    let tx = match h.db.begin_write() {
        Ok(tx) => tx,
        Err(e) => return json!({"error": {"code": "internal_error", "message": e.to_string()}}),
    };
    let v = f(&tx);
    let ok = v.get("error").is_none() && v.get("refused").is_none();
    let done = if ok {
        tx.commit().map_err(|e| e.to_string())
    } else {
        tx.abort().map_err(|e| e.to_string())
    };
    match done {
        Ok(()) => v,
        Err(e) => json!({"error": {"code": "internal_error", "message": e}}),
    }
}

/// `WriteAuthorizationModel` under model ID `id`.
///
/// # Safety
/// `h` must be live; the pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_redb_write_model(
    h: *const RedbHandle,
    store: *const u8,
    store_len: usize,
    id: *const u8,
    id_len: usize,
    json: *const u8,
    len: usize,
) -> AfcBuf {
    let h = &*h;
    let store = std::str::from_utf8(bytes(store, store_len)).unwrap_or("");
    let id = std::str::from_utf8(bytes(id, id_len)).unwrap_or("");
    let json = bytes(json, len);
    buf(guarded(|| {
        in_write(h, |tx| {
            h.fga
                .write_model(&atomicfga_redb::Write(tx), store, id, json)
        })
    }))
}

/// `Write` (a WriteRequest in protojson).
///
/// # Safety
/// `h` must be live; the pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_redb_write(
    h: *const RedbHandle,
    store: *const u8,
    store_len: usize,
    json: *const u8,
    len: usize,
) -> AfcBuf {
    let h = &*h;
    let store = std::str::from_utf8(bytes(store, store_len)).unwrap_or("");
    let json = bytes(json, len);
    buf(guarded(|| {
        in_write(h, |tx| h.fga.write(&atomicfga_redb::Write(tx), store, json))
    }))
}

/// `Check` (a CheckRequest in protojson), in OpenFGA semantics.
///
/// # Safety
/// `h` must be live; the pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn afc_redb_check(
    h: *const RedbHandle,
    store: *const u8,
    store_len: usize,
    json: *const u8,
    len: usize,
) -> AfcBuf {
    use redb::ReadableDatabase;
    let h = &*h;
    let store = std::str::from_utf8(bytes(store, store_len)).unwrap_or("");
    let json = bytes(json, len);
    buf(guarded(|| match h.db.begin_read() {
        Ok(tx) => h.fga.check(
            &atomicfga_redb::Read(&tx),
            store,
            json,
            atomicfga_check::Semantics::OpenFga,
        ),
        Err(e) => json!({"error": {"code": "internal_error", "message": e.to_string()}}),
    }))
}
