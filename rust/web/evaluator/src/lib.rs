//! `atomicfga-check` as a WebAssembly module with no storage of its own.
//!
//! Storage is imported (module `store`): the host wires these functions to
//! any store (the redb module, SQLite, IndexedDB…), within a transaction the
//! host opened. Keys arrive as UTF-8 strings in this module's memory; values
//! are opaque bytes. A store function returns the length of its result
//! (fetched into this module's memory with `fetch`), `-1` for "absent", or
//! `-2 - n` for an error whose message, `n` bytes long, `fetch` delivers.
//!
//! The API is exported: `write_model`, `write`, `check` and `compile` take
//! protojson and return the length of a JSON response at `result_ptr()`.
//! Allocate argument buffers with `alloc` and release them with `dealloc`.

use atomicfga_check::api::Fga;
use atomicfga_check::store::{Store, StoreMut};
use atomicfga_check::Semantics;
use std::cell::RefCell;

#[link(wasm_import_module = "store")]
extern "C" {
    fn tuple_get(
        s: *const u8,
        sl: usize,
        o: *const u8,
        ol: usize,
        r: *const u8,
        rl: usize,
        u: *const u8,
        ul: usize,
    ) -> i32;
    fn tuple_scan(s: *const u8, sl: usize, o: *const u8, ol: usize, r: *const u8, rl: usize)
        -> i32;
    fn tuple_put(
        s: *const u8,
        sl: usize,
        o: *const u8,
        ol: usize,
        r: *const u8,
        rl: usize,
        u: *const u8,
        ul: usize,
        v: *const u8,
        vl: usize,
    ) -> i32;
    fn tuple_delete(
        s: *const u8,
        sl: usize,
        o: *const u8,
        ol: usize,
        r: *const u8,
        rl: usize,
        u: *const u8,
        ul: usize,
    ) -> i32;
    fn model_get(s: *const u8, sl: usize, i: *const u8, il: usize) -> i32;
    fn model_latest(s: *const u8, sl: usize) -> i32;
    fn model_put(s: *const u8, sl: usize, i: *const u8, il: usize, m: *const u8, ml: usize) -> i32;
    /// Copies the last store function's result to `dst`.
    fn fetch(dst: *mut u8);
}

/// Collects a store function's result: `Ok(None)` if absent.
fn result(n: i32) -> Result<Option<Vec<u8>>, String> {
    let take = |len: usize| {
        let mut v = vec![0u8; len];
        unsafe { fetch(v.as_mut_ptr()) };
        v
    };
    match n {
        -1 => Ok(None),
        n if n >= 0 => Ok(Some(take(n as usize))),
        _ => {
            let len = (-n - 2) as usize;
            Err(String::from_utf8_lossy(&take(len)).into_owned())
        }
    }
}

fn status(n: i32) -> Result<(), String> {
    result(n).map(|_| ())
}

/// The host's store, through the imports.
struct Imported;

macro_rules! s {
    ($x:expr) => {
        ($x.as_ptr(), $x.len())
    };
}

fn u32le(b: &[u8], at: &mut usize) -> Result<usize, String> {
    let w = b.get(*at..*at + 4).ok_or("truncated scan result")?;
    *at += 4;
    Ok(u32::from_le_bytes([w[0], w[1], w[2], w[3]]) as usize)
}

impl Store for Imported {
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        let ((sp, sl), (op, ol), (rp, rl), (up, ul)) =
            (s!(store), s!(object), s!(relation), s!(user));
        result(unsafe { tuple_get(sp, sl, op, ol, rp, rl, up, ul) })
    }
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        let ((sp, sl), (op, ol), (rp, rl)) = (s!(store), s!(object), s!(relation));
        let Some(b) = result(unsafe { tuple_scan(sp, sl, op, ol, rp, rl) })? else {
            return Ok(vec![]);
        };
        // (u32 LE user length, user, u32 LE value length, value)*
        let mut out = vec![];
        let mut at = 0;
        while at < b.len() {
            let n = u32le(&b, &mut at)?;
            let user =
                String::from_utf8(b.get(at..at + n).ok_or("truncated scan result")?.to_vec())
                    .map_err(|e| e.to_string())?;
            at += n;
            let n = u32le(&b, &mut at)?;
            let value = b.get(at..at + n).ok_or("truncated scan result")?.to_vec();
            at += n;
            out.push((user, value));
        }
        Ok(out)
    }
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        let ((sp, sl), (ip, il)) = (s!(store), s!(id));
        result(unsafe { model_get(sp, sl, ip, il) })
    }
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        let (sp, sl) = s!(store);
        let Some(b) = result(unsafe { model_latest(sp, sl) })? else {
            return Ok(None);
        };
        // (u32 LE ID length, ID, model)
        let mut at = 0;
        let n = u32le(&b, &mut at)?;
        let id = String::from_utf8(b.get(at..at + n).ok_or("truncated model")?.to_vec())
            .map_err(|e| e.to_string())?;
        Ok(Some((id, b[at + n..].to_vec())))
    }
}

impl StoreMut for Imported {
    fn put_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
        value: &[u8],
    ) -> Result<(), String> {
        let ((sp, sl), (op, ol), (rp, rl), (up, ul), (vp, vl)) =
            (s!(store), s!(object), s!(relation), s!(user), s!(value));
        status(unsafe { tuple_put(sp, sl, op, ol, rp, rl, up, ul, vp, vl) })
    }
    fn delete_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<(), String> {
        let ((sp, sl), (op, ol), (rp, rl), (up, ul)) =
            (s!(store), s!(object), s!(relation), s!(user));
        status(unsafe { tuple_delete(sp, sl, op, ol, rp, rl, up, ul) })
    }
    fn put_model(&self, store: &str, id: &str, model: &[u8]) -> Result<(), String> {
        let ((sp, sl), (ip, il), (mp, ml)) = (s!(store), s!(id), s!(model));
        status(unsafe { model_put(sp, sl, ip, il, mp, ml) })
    }
}

thread_local! {
    static FGA: RefCell<Fga> = RefCell::new(Fga::default());
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

unsafe fn arg<'a>(p: *const u8, l: usize) -> &'a [u8] {
    if l == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(p, l)
    }
}

unsafe fn text<'a>(p: *const u8, l: usize) -> &'a str {
    std::str::from_utf8(arg(p, l)).unwrap_or("")
}

fn respond(v: serde_json::Value) -> usize {
    let bytes = serde_json::to_vec(&v).unwrap_or_default();
    let n = bytes.len();
    RESULT.with(|r| *r.borrow_mut() = bytes);
    n
}

#[no_mangle]
pub extern "C" fn alloc(len: usize) -> *mut u8 {
    let mut v = std::mem::ManuallyDrop::new(Vec::<u8>::with_capacity(len.max(1)));
    v.as_mut_ptr()
}

/// # Safety
/// `p` must come from `alloc(len)`.
#[no_mangle]
pub unsafe extern "C" fn dealloc(p: *mut u8, len: usize) {
    drop(Vec::from_raw_parts(p, 0, len.max(1)));
}

#[no_mangle]
pub extern "C" fn result_ptr() -> *const u8 {
    RESULT.with(|r| r.borrow().as_ptr())
}

/// Sets OpenFGA's `inline_expressions` flag (0 or 1).
#[no_mangle]
pub extern "C" fn set_inline_expressions(on: i32) {
    FGA.with(|f| f.borrow_mut().inline_expressions = on != 0);
}

/// `WriteAuthorizationModel` under model ID `id`, in the host's write
/// transaction.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn write_model(
    sp: *const u8,
    sl: usize,
    ip: *const u8,
    il: usize,
    jp: *const u8,
    jl: usize,
) -> usize {
    respond(FGA.with(|f| {
        f.borrow()
            .write_model(&Imported, text(sp, sl), text(ip, il), arg(jp, jl))
    }))
}

/// `Write`, in the host's write transaction.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn write(sp: *const u8, sl: usize, jp: *const u8, jl: usize) -> usize {
    respond(FGA.with(|f| f.borrow().write(&Imported, text(sp, sl), arg(jp, jl))))
}

/// `Check` in OpenFGA (0) or canonical (1) semantics.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn check(
    sp: *const u8,
    sl: usize,
    jp: *const u8,
    jl: usize,
    canonical: i32,
) -> usize {
    let semantics = if canonical != 0 {
        Semantics::Canonical
    } else {
        Semantics::OpenFga
    };
    respond(FGA.with(|f| {
        f.borrow()
            .check(&Imported, text(sp, sl), arg(jp, jl), semantics)
    }))
}

/// The model compiler's verdict on a WriteAuthorizationModelRequest.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn compile(jp: *const u8, jl: usize, inline_expressions: i32) -> usize {
    let options =
        serde_json::from_value(serde_json::json!({"inline_expressions": inline_expressions != 0}))
            .unwrap_or_default();
    respond(atomicfga_check::compile_json(arg(jp, jl), options))
}
