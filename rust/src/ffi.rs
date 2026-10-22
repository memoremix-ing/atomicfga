//! C ABI for hosts (atomicfga links it through cgo). Every message is JSON.
//! A panic never crosses the boundary: it becomes a refusal.

use crate::{Engine, Task};
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

#[no_mangle]
pub extern "C" fn afc_engine_new() -> *mut Engine {
    Box::into_raw(Box::default())
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
