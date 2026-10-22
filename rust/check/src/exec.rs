//! A sans-IO executor. Resolution is written as ordinary async Rust; every
//! datastore read is a future that registers a request and stays pending
//! until the host answers it. The host drives a [`Task`]: each step polls the
//! whole resolution once and hands back every read that became necessary,
//! so independent reads leave together (one pipelined round trip where the
//! host's driver supports it).
//!
//! Futures here are always re-polled from the root, so no waker is needed.
//! Combinators poll all their unfinished children on every step.

use crate::outcome::{Out, T};
use crate::Tuple;
use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

/// A datastore read, as OpenFGA's RelationshipTupleReader methods take them.
#[derive(Clone, Debug)]
pub enum ReadReq {
    /// `Read(store, {Object, Relation})`
    Read { object: String, relation: String },
    /// `ReadUserTuple(store, {Object, Relation, User})`
    User {
        object: String,
        relation: String,
        user: String,
    },
    /// `ReadUsersetTuples(store, {Object, Relation, AllowedUserTypeRestrictions})`
    Usersets {
        object: String,
        relation: String,
        allowed: Vec<crate::model::RelRef>,
    },
}

pub type Answer = Result<Vec<Tuple>, String>;

#[derive(Default)]
pub struct Io {
    next: u64,
    queued: Vec<(u64, ReadReq)>,
    answers: HashMap<u64, Answer>,
    live: HashSet<u64>,
    /// Reads handed to the host that became moot.
    cancelled: Vec<u64>,
    pub reads: u64,
}

pub type IoRef = Rc<RefCell<Io>>;

impl Io {
    pub fn take_queued(&mut self) -> Vec<(u64, ReadReq)> {
        std::mem::take(&mut self.queued)
    }

    pub fn take_cancelled(&mut self) -> Vec<u64> {
        std::mem::take(&mut self.cancelled)
    }

    /// Whether any read is queued or awaiting the host's answer.
    pub fn busy(&self) -> bool {
        !self.queued.is_empty() || !self.live.is_empty()
    }

    /// Records the host's answer; answers to abandoned reads are dropped.
    pub fn answer(&mut self, id: u64, a: Answer) {
        if self.live.contains(&id) {
            self.answers.insert(id, a);
        }
    }
}

pub struct ReadFut {
    io: IoRef,
    req: Option<ReadReq>,
    id: u64,
}

pub fn read(io: &IoRef, req: ReadReq) -> ReadFut {
    ReadFut {
        io: io.clone(),
        req: Some(req),
        id: 0,
    }
}

impl Future for ReadFut {
    type Output = Answer;
    fn poll(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Answer> {
        let this = self.get_mut();
        let mut io = this.io.borrow_mut();
        if this.id == 0 {
            io.next += 1;
            io.reads += 1;
            let id = io.next;
            io.queued.push((id, this.req.take().unwrap()));
            io.live.insert(id);
            drop(io);
            this.id = id;
            return Poll::Pending;
        }
        let id = this.id;
        match io.answers.remove(&id) {
            Some(a) => {
                io.live.remove(&id);
                drop(io);
                this.id = u64::MAX;
                Poll::Ready(a)
            }
            None => Poll::Pending,
        }
    }
}

impl Drop for ReadFut {
    // An abandoned read (a branch made moot by a decided union) is withdrawn
    // if the host has not received it yet, and reported as cancelled if it
    // has (a host answering reads one at a time can then skip it).
    fn drop(&mut self) {
        if self.id != 0 && self.id != u64::MAX {
            let mut io = self.io.borrow_mut();
            let queued = io.queued.len();
            io.queued.retain(|(id, _)| *id != self.id);
            if io.queued.len() == queued && io.live.contains(&self.id) {
                io.cancelled.push(self.id);
            }
            io.live.remove(&self.id);
            io.answers.remove(&self.id);
        }
    }
}

pub type BoxFut<'a, T> = Pin<Box<dyn Future<Output = T> + 'a>>;

/// Polls every child until all are ready.
pub struct JoinAll<'a> {
    children: Vec<Option<BoxFut<'a, Out>>>,
    results: Vec<Option<Out>>,
    /// Finish as soon as one child is certainly allowed.
    stop_on_true: bool,
}

impl Future for JoinAll<'_> {
    type Output = Result<Vec<Out>, Out>;
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let this = &mut *self;
        for (child, slot) in this.children.iter_mut().zip(this.results.iter_mut()) {
            if let Some(f) = child {
                if let Poll::Ready(out) = f.as_mut().poll(cx) {
                    *child = None;
                    if this.stop_on_true && out.bits == T {
                        return Poll::Ready(Err(out));
                    }
                    *slot = Some(out);
                }
            }
        }
        if this.results.iter().all(Option::is_some) {
            Poll::Ready(Ok(this
                .results
                .iter_mut()
                .map(|r| r.take().unwrap())
                .collect()))
        } else {
            Poll::Pending
        }
    }
}

/// Every child's outcome.
pub async fn join_all(children: Vec<BoxFut<'_, Out>>) -> Vec<Out> {
    let n = children.len();
    let j = JoinAll {
        children: children.into_iter().map(Some).collect(),
        results: vec![None; n],
        stop_on_true: false,
    };
    match j.await {
        Ok(v) => v,
        Err(_) => unreachable!(),
    }
}

/// OpenFGA's union reducer, returning as soon as one child is certainly
/// allowed (OpenFGA cancels the others then).
pub async fn join_union(children: Vec<BoxFut<'_, Out>>) -> Out {
    let n = children.len();
    let j = JoinAll {
        children: children.into_iter().map(Some).collect(),
        results: vec![None; n],
        stop_on_true: true,
    };
    match j.await {
        Ok(v) => crate::outcome::union(&v),
        Err(t) => t,
    }
}

fn noop_waker() -> Waker {
    fn clone(_: *const ()) -> RawWaker {
        RawWaker::new(std::ptr::null(), &VTABLE)
    }
    fn noop(_: *const ()) {}
    static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, noop, noop, noop);
    unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
}

/// Polls `fut` once.
pub fn poll_once<T>(fut: &mut BoxFut<'static, T>) -> Poll<T> {
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    fut.as_mut().poll(&mut cx)
}
