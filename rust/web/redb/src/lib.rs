//! A redb store as a WebAssembly module, independent of the evaluator: the
//! host passes values between the two (see web/atomicfga.js).
//!
//! The database lives in memory, or in a file of the browser's Origin
//! Private File System: the host implements redb's storage backend with a
//! `FileSystemSyncAccessHandle` (imports `opfs_*`, in a worker).
//!
//! One transaction is open at a time: `begin_write` or `begin_read`, then
//! `commit` or `abort` (errors when none is open). Beginning while one is
//! open is an error, and leaves the open one intact. The tuple and model
//! functions mirror the evaluator's store imports: keys are UTF-8 in this
//! module's memory, the result is at `result_ptr()`, and they return its
//! length, `-1` (absent) or `-2 - n` (an error message of `n` bytes).

use atomicfga_check::store::{Store, StoreMut};
use atomicfga_redb::{Read, Write};
use redb::{
    backends::InMemoryBackend, Database, ReadTransaction, ReadableDatabase, StorageBackend,
    WriteTransaction,
};
use std::cell::RefCell;
use std::io;

#[link(wasm_import_module = "opfs")]
extern "C" {
    fn opfs_len() -> f64;
    fn opfs_read(offset: f64, dst: *mut u8, len: usize) -> i32;
    fn opfs_write(offset: f64, src: *const u8, len: usize) -> i32;
    fn opfs_set_len(len: f64) -> i32;
    fn opfs_sync() -> i32;
}

/// redb's storage on an OPFS file, through the host.
#[derive(Debug)]
struct Opfs;

fn io_status(n: i32, what: &str) -> io::Result<()> {
    if n == 0 {
        Ok(())
    } else {
        Err(io::Error::other(format!("OPFS {what} failed")))
    }
}

impl StorageBackend for Opfs {
    fn len(&self) -> io::Result<u64> {
        Ok(unsafe { opfs_len() } as u64)
    }
    fn read(&self, offset: u64, out: &mut [u8]) -> io::Result<()> {
        io_status(
            unsafe { opfs_read(offset as f64, out.as_mut_ptr(), out.len()) },
            "read",
        )
    }
    fn set_len(&self, len: u64) -> io::Result<()> {
        io_status(unsafe { opfs_set_len(len as f64) }, "truncate")
    }
    fn sync_data(&self) -> io::Result<()> {
        io_status(unsafe { opfs_sync() }, "flush")
    }
    fn write(&self, offset: u64, data: &[u8]) -> io::Result<()> {
        io_status(
            unsafe { opfs_write(offset as f64, data.as_ptr(), data.len()) },
            "write",
        )
    }
}

enum Tx {
    None,
    Read(Box<ReadTransaction>),
    Write(Box<WriteTransaction>),
}

thread_local! {
    static DB: RefCell<Option<Database>> = const { RefCell::new(None) };
    static TX: RefCell<Tx> = const { RefCell::new(Tx::None) };
    static RESULT: RefCell<Vec<u8>> = const { RefCell::new(Vec::new()) };
}

fn set_result(bytes: Vec<u8>) -> i32 {
    let n = bytes.len() as i32;
    RESULT.with(|r| *r.borrow_mut() = bytes);
    n
}

fn error(e: impl std::fmt::Display) -> i32 {
    -2 - set_result(e.to_string().into_bytes())
}

fn answer(r: Result<Option<Vec<u8>>, String>) -> i32 {
    match r {
        Ok(Some(b)) => set_result(b),
        Ok(None) => -1,
        Err(e) => error(e),
    }
}

fn done(r: Result<(), String>) -> i32 {
    match r {
        Ok(()) => set_result(vec![]),
        Err(e) => error(e),
    }
}

/// Runs `f` on the open transaction as a store.
fn with_store<T>(f: impl FnOnce(&dyn Store) -> Result<T, String>) -> Result<T, String> {
    TX.with(|t| match &*t.borrow() {
        Tx::Read(tx) => f(&Read(tx)),
        Tx::Write(tx) => f(&Write(tx)),
        Tx::None => Err("no transaction is open".into()),
    })
}

fn with_write<T>(f: impl FnOnce(&Write) -> Result<T, String>) -> Result<T, String> {
    TX.with(|t| match &*t.borrow() {
        Tx::Write(tx) => f(&Write(tx)),
        Tx::Read(_) => Err("no write transaction is open".into()),
        Tx::None => Err("no transaction is open".into()),
    })
}

unsafe fn text<'a>(p: *const u8, l: usize) -> &'a str {
    if l == 0 {
        ""
    } else {
        std::str::from_utf8(std::slice::from_raw_parts(p, l)).unwrap_or("")
    }
}

unsafe fn bytes<'a>(p: *const u8, l: usize) -> &'a [u8] {
    if l == 0 {
        &[]
    } else {
        std::slice::from_raw_parts(p, l)
    }
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

/// Opens the database: in memory (0) or on the host's OPFS file (1).
#[no_mangle]
pub extern "C" fn open(persistent: i32) -> i32 {
    let builder = Database::builder();
    let db = if persistent != 0 {
        builder.create_with_backend(Opfs)
    } else {
        builder.create_with_backend(InMemoryBackend::new())
    };
    match db {
        Ok(db) => {
            DB.with(|d| *d.borrow_mut() = Some(db));
            0
        }
        Err(e) => error(e),
    }
}

/// Closes the database (and so its file), aborting an open transaction.
#[no_mangle]
pub extern "C" fn close() -> i32 {
    if let Tx::Write(tx) = TX.with(|t| std::mem::replace(&mut *t.borrow_mut(), Tx::None)) {
        let _ = tx.abort();
    }
    DB.with(|d| d.borrow_mut().take());
    0
}

/// Opens a transaction. The slot is checked first: asking redb for a second
/// write transaction would wait forever for its writer lock.
fn begin(write: bool) -> i32 {
    TX.with(|t| {
        let mut t = t.borrow_mut();
        if !matches!(*t, Tx::None) {
            return error("a transaction is already open");
        }
        let tx = DB.with(|d| {
            let d = d.borrow();
            let db = d
                .as_ref()
                .ok_or_else(|| "database is not open".to_string())?;
            if write {
                db.begin_write()
                    .map(|t| Tx::Write(Box::new(t)))
                    .map_err(|e| e.to_string())
            } else {
                db.begin_read()
                    .map(|t| Tx::Read(Box::new(t)))
                    .map_err(|e| e.to_string())
            }
        });
        match tx {
            Ok(tx) => {
                *t = tx;
                0
            }
            Err(e) => error(e),
        }
    })
}

#[no_mangle]
pub extern "C" fn begin_write() -> i32 {
    begin(true)
}

#[no_mangle]
pub extern "C" fn begin_read() -> i32 {
    begin(false)
}

/// Ends the open transaction, committing it or not. The transaction ends
/// even if committing fails (its changes are then discarded).
fn end(commit: bool) -> i32 {
    let r = match TX.with(|t| std::mem::replace(&mut *t.borrow_mut(), Tx::None)) {
        Tx::None => Err("no transaction is open".to_string()),
        Tx::Read(_) => Ok(()),
        Tx::Write(tx) if commit => tx.commit().map_err(|e| e.to_string()),
        Tx::Write(tx) => tx.abort().map_err(|e| e.to_string()),
    };
    match r {
        Ok(()) => 0,
        Err(e) => error(e),
    }
}

/// Commits the open transaction (a read transaction just ends).
#[no_mangle]
pub extern "C" fn commit() -> i32 {
    end(true)
}

/// Aborts the open transaction.
#[no_mangle]
pub extern "C" fn abort() -> i32 {
    end(false)
}

/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn tuple_get(
    s: *const u8,
    sl: usize,
    o: *const u8,
    ol: usize,
    r: *const u8,
    rl: usize,
    u: *const u8,
    ul: usize,
) -> i32 {
    answer(with_store(|st| {
        st.tuple(text(s, sl), text(o, ol), text(r, rl), text(u, ul))
    }))
}

/// Encodes `(u32 LE user length, user, u32 LE value length, value)*`.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn tuple_scan(
    s: *const u8,
    sl: usize,
    o: *const u8,
    ol: usize,
    r: *const u8,
    rl: usize,
) -> i32 {
    answer(
        with_store(|st| st.tuples(text(s, sl), text(o, ol), text(r, rl))).map(|rows| {
            let mut out = Vec::new();
            for (u, v) in rows {
                out.extend_from_slice(&(u.len() as u32).to_le_bytes());
                out.extend_from_slice(u.as_bytes());
                out.extend_from_slice(&(v.len() as u32).to_le_bytes());
                out.extend_from_slice(&v);
            }
            Some(out)
        }),
    )
}

/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn tuple_put(
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
) -> i32 {
    done(with_write(|st| {
        st.put_tuple(
            text(s, sl),
            text(o, ol),
            text(r, rl),
            text(u, ul),
            bytes(v, vl),
        )
    }))
}

/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn tuple_delete(
    s: *const u8,
    sl: usize,
    o: *const u8,
    ol: usize,
    r: *const u8,
    rl: usize,
    u: *const u8,
    ul: usize,
) -> i32 {
    done(with_write(|st| {
        st.delete_tuple(text(s, sl), text(o, ol), text(r, rl), text(u, ul))
    }))
}

/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn model_get(s: *const u8, sl: usize, i: *const u8, il: usize) -> i32 {
    answer(with_store(|st| st.model(text(s, sl), text(i, il))))
}

/// Encodes `(u32 LE ID length, ID, model)`.
///
/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn model_latest(s: *const u8, sl: usize) -> i32 {
    answer(with_store(|st| st.latest_model(text(s, sl))).map(|m| {
        m.map(|(id, model)| {
            let mut out = (id.len() as u32).to_le_bytes().to_vec();
            out.extend_from_slice(id.as_bytes());
            out.extend_from_slice(&model);
            out
        })
    }))
}

/// # Safety
/// The pointers must point to readable bytes.
#[no_mangle]
pub unsafe extern "C" fn model_put(
    s: *const u8,
    sl: usize,
    i: *const u8,
    il: usize,
    m: *const u8,
    ml: usize,
) -> i32 {
    done(with_write(|st| {
        st.put_model(text(s, sl), text(i, il), bytes(m, ml))
    }))
}
