//! A sans-IO port of OpenFGA's Check.
//!
//! The engine evaluates; the host performs reads. A [`Task`] resolves one
//! Check request: each [`Task::step`] returns either the datastore reads it
//! needs (answered in the next step) or the result. The host answers reads
//! from inside its own transaction, which is what lets atomicfga run this
//! evaluator against the caller's uncommitted state.
//!
//! The port is fail-closed: whenever it cannot be certain what OpenFGA
//! answers (an unsupported condition feature, an input spelling it does not
//! parse exactly as Go does, or an outcome that depends on goroutine timing
//! upstream), it *refuses* instead of answering. Refusals are counted by the
//! conformance gates; wrong answers fail them.

pub mod api;
mod cel;
mod compile;
mod exec;
mod model;
mod outcome;
mod resolve;
pub mod store;
mod strings;
mod validate;
mod weighted;

pub use exec::{Answer, ReadReq};
pub use model::{RefKind, RelRef};

/// Which semantics a check follows.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub enum Semantics {
    /// OpenFGA's answer, refusing wherever OpenFGA's own answer is not
    /// determined (operand timing, planner strategies).
    #[default]
    OpenFga,
    /// The canonical semantics of `spec/check.qnt`: a relation holds iff it
    /// is derivable (least fixed point). A cycle only means "not derivable
    /// along this path"; there is no cycle flag to make an exclusion false,
    /// no planner whose strategies could disagree, and no depth limit.
    ///
    /// Within bounds: the model must be stratified (no relation depends on
    /// itself through an exclusion's subtrahend; the semantics is undefined
    /// otherwise), and a check may nest 100 deep and make 100,000
    /// resolutions. Outside them the check refuses; it never answers false
    /// for want of resources.
    Canonical,
}

/// The OpenFGA server settings a check's answer depends on, beyond the
/// model and the request. The host must pass the server's actual values (and
/// refuse to use this evaluator under settings it does not model).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    /// `server.WithResolveNodeLimit`: resolution deeper than this is too
    /// complex.
    pub resolve_node_limit: u32,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            // serverconfig.DefaultResolveNodeLimit
            resolve_node_limit: 25,
        }
    }
}

/// One step of a [`Task`].
pub enum Step {
    /// Reads to answer, and earlier reads whose answers are no longer
    /// needed.
    Reads {
        reads: Vec<(u64, ReadReq)>,
        cancelled: Vec<u64>,
    },
    /// The result: `{"allowed": bool}`, `{"error": {...}}` or
    /// `{"refused": "..."}`.
    Done(Value),
}

use exec::{poll_once, BoxFut, Io, IoRef};
use model::Model;
use outcome::{Out, C, E, F, T};
use resolve::{resolve_check, Ctx, Req, Visited};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::cell::RefCell;
use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::{Arc, Mutex};
use std::task::Poll;
use validate::{validate_struct, validate_tuple_for_write, validate_user_object_relation, VErr};

/// The model compiler's verdict as JSON (see [`compile::compile`]).
pub fn compile_json(json: &[u8], options: compile::Options) -> Value {
    match compile::compile(json, options) {
        compile::Outcome::Valid => json!({"valid": true}),
        compile::Outcome::Invalid { code, message } => {
            json!({"invalid": {"code": code, "message": message}})
        }
        compile::Outcome::Undecided(why) => json!({"undecided": why}),
    }
}

/// The strategy table the port derives for a model (see [`weighted`]):
/// `{"alternatives": [...]}` or `{"error": ...}`.
pub fn alternatives_json(model: &[u8]) -> Value {
    match Model::from_json(model) {
        Ok(m) => json!({"alternatives": m.alternatives()}),
        Err(e) => json!({"error": e}),
    }
}

/// An OpenFGA API error, by the gRPC error it maps to.
#[derive(Clone, Debug, PartialEq)]
pub enum ApiError {
    /// `validation_error`
    Validation(String),
    /// `invalid_tuple`
    InvalidTuple(String),
    /// `authorization_model_resolution_too_complex`
    TooComplex,
    /// An internal error.
    Internal(String),
}

impl ApiError {
    pub fn same_kind(&self, o: &ApiError) -> bool {
        std::mem::discriminant(self) == std::mem::discriminant(o)
    }

    fn to_json(&self) -> Value {
        let (kind, message) = match self {
            ApiError::Validation(m) => ("validation", m.as_str()),
            ApiError::InvalidTuple(m) => ("invalid_tuple", m.as_str()),
            ApiError::TooComplex => ("too_complex", ""),
            ApiError::Internal(m) => ("internal", m.as_str()),
        };
        json!({"kind": kind, "message": message})
    }
}

#[derive(Clone, Debug)]
pub struct Cond {
    pub name: String,
    pub context: Option<Map<String, Value>>,
}

#[derive(Clone, Debug)]
pub struct Tuple {
    pub object: String,
    pub relation: String,
    pub user: String,
    pub condition: Option<Cond>,
}

#[derive(Deserialize)]
struct TupleKeyJson {
    #[serde(default)]
    object: String,
    #[serde(default)]
    relation: String,
    #[serde(default)]
    user: String,
    #[serde(default)]
    condition: Option<CondJson>,
}

#[derive(Deserialize)]
struct CondJson {
    #[serde(default)]
    name: String,
    #[serde(default)]
    context: Option<Map<String, Value>>,
}

#[derive(Deserialize)]
struct ContextualJson {
    #[serde(default)]
    tuple_keys: Vec<TupleKeyJson>,
}

/// A CheckRequest in protojson with proto field names.
#[derive(Deserialize)]
struct CheckRequestJson {
    #[serde(default)]
    tuple_key: Option<TupleKeyJson>,
    #[serde(default)]
    contextual_tuples: Option<ContextualJson>,
    #[serde(default)]
    context: Option<Map<String, Value>>,
}

impl From<TupleKeyJson> for Tuple {
    fn from(t: TupleKeyJson) -> Tuple {
        Tuple {
            object: t.object,
            relation: t.relation,
            user: t.user,
            condition: t.condition.map(|c| Cond {
                name: c.name,
                context: c.context,
            }),
        }
    }
}

/// Holds decoded models by ID. Models are immutable, so a model ID's entry
/// is valid for every transaction in which the host has seen that ID.
#[derive(Default)]
pub struct Engine {
    models: Mutex<HashMap<String, Result<Arc<Model>, String>>>,
    config: Config,
}

impl Engine {
    /// An engine whose checks follow `config`.
    pub fn new(config: Config) -> Engine {
        Engine {
            models: Mutex::default(),
            config,
        }
    }

    pub fn has_model(&self, id: &str) -> bool {
        self.models.lock().unwrap().contains_key(id)
    }

    /// Decodes, compiles and caches a model: `{"model": <protojson with
    /// proto field names>}`. A model this port cannot evaluate is cached as a
    /// refusal reason.
    pub fn load_model(&self, json: &[u8]) -> Result<String, String> {
        #[derive(Deserialize)]
        struct Load {
            model: Box<serde_json::value::RawValue>,
            /// Set when the host could not provide the model.
            #[serde(default)]
            refuse: Option<String>,
        }
        #[derive(Deserialize)]
        struct Id {
            #[serde(default)]
            id: String,
        }
        let load: Load = serde_json::from_slice(json).map_err(|e| e.to_string())?;
        let model = load.model.get().as_bytes();
        let id = serde_json::from_slice::<Id>(model)
            .map_err(|e| e.to_string())?
            .id;
        let m = match load.refuse {
            Some(why) => Err(why),
            None => compile_stored(model),
        };
        self.models.lock().unwrap().insert(id.clone(), m);
        Ok(id)
    }

    pub fn check(&self, model_id: &str, request: &[u8]) -> Task {
        self.check_with(model_id, request, Semantics::OpenFga)
    }

    pub fn check_with(&self, model_id: &str, request: &[u8], semantics: Semantics) -> Task {
        match self.models.lock().unwrap().get(model_id).cloned() {
            None => Task::done(json!({"refused": format!("model {model_id} is not loaded")})),
            Some(m) => Task::new(m, request, semantics, self.config),
        }
    }
}

/// Compiles a model as stored, or gives the reason this port cannot evaluate
/// it.
pub(crate) fn compile_stored(model: &[u8]) -> Result<Arc<Model>, String> {
    // Stored models passed OpenFGA's checks under whatever flags it ran with.
    let lenient = compile::Options {
        inline_expressions: true,
    };
    match compile::compile(model, lenient) {
        compile::Outcome::Invalid { message, .. } => Err(format!("invalid model: {message}")),
        // Valid, or valid as far as this port decides (OpenFGA stored it).
        _ => Model::from_json(model).map(Arc::new),
    }
}

enum State {
    Running(BoxFut<'static, Out>),
    Done(Value),
}

pub struct Task {
    io: IoRef,
    state: State,
}

impl Task {
    /// A check of `request` against `model` (or its refusal reason).
    pub(crate) fn new(
        model: Result<Arc<Model>, String>,
        request: &[u8],
        semantics: Semantics,
        config: Config,
    ) -> Task {
        let io: IoRef = Rc::new(RefCell::new(Io::default()));
        let state = match model {
            Err(why) => State::Done(json!({"refused": format!("model: {why}")})),
            Ok(m) => match start(m, io.clone(), request, semantics, config) {
                Ok(fut) => State::Running(fut),
                Err(done) => State::Done(done),
            },
        };
        Task { io, state }
    }

    fn done(v: Value) -> Task {
        Task {
            io: Rc::new(RefCell::new(Io::default())),
            state: State::Done(v),
        }
    }
}

fn refused(why: impl std::fmt::Display) -> Value {
    json!({"refused": why.to_string()})
}

fn error(e: ApiError) -> Value {
    json!({"error": e.to_json()})
}

/// `validateCheckRequest`, then resolution of the request's tuple key.
fn start(
    model: Arc<Model>,
    io: IoRef,
    request: &[u8],
    semantics: Semantics,
    config: Config,
) -> Result<BoxFut<'static, Out>, Value> {
    let req: CheckRequestJson =
        serde_json::from_slice(request).map_err(|e| refused(format!("request JSON: {e}")))?;
    let tk = req
        .tuple_key
        .ok_or_else(|| refused("request without tuple_key"))?;
    if semantics == Semantics::Canonical {
        if let Some(why) = &model.unstratified {
            return Err(refused(format!(
                "the canonical semantics is undefined for this model: {why}"
            )));
        }
    }
    if let Err(e) = validate_user_object_relation(&model, &tk.object, &tk.relation, &tk.user) {
        return Err(error(ApiError::Validation(e)));
    }
    if let Err(e) = validate_struct(req.context.as_ref()) {
        return Err(error(ApiError::Validation(e)));
    }
    let mut contextual: Vec<Tuple> = req
        .contextual_tuples
        .map(|c| c.tuple_keys)
        .unwrap_or_default()
        .into_iter()
        .map(Tuple::from)
        .collect();
    for t in &contextual {
        match validate_tuple_for_write(&model, t) {
            Ok(()) => {}
            Err(VErr::Invalid(e)) => {
                return Err(error(ApiError::InvalidTuple(format!(
                    "Invalid tuple '{}'. Reason: {e}",
                    strings::tuple_key_string(&t.object, &t.relation, &t.user)
                ))))
            }
            Err(VErr::Refuse(r)) => return Err(refused(r)),
        }
    }
    // Go sorts contextual tuples by object with pdqsort, which is stable
    // (insertion sort) only up to 12 elements. Order matters only to which
    // of several tuples with one key ReadUserTuple returns.
    if contextual.len() > 12 {
        let mut keys = HashSet::new();
        if !contextual
            .iter()
            .all(|t| keys.insert((&t.object, &t.relation, &t.user)))
        {
            return Err(refused(
                "more than 12 contextual tuples with a repeated key",
            ));
        }
    }
    contextual.sort_by(|a, b| a.object.cmp(&b.object));
    for t in &mut contextual {
        // NewTupleKeyWithCondition drops an unnamed condition.
        if t.condition.as_ref().is_some_and(|c| c.name.is_empty()) {
            t.condition = None;
        }
    }
    let cx = Rc::new(Ctx {
        model,
        io,
        contextual,
        request_context: req.context,
        semantics,
        max_depth: config.resolve_node_limit,
        resolutions: Default::default(),
    });
    Ok(resolve_check(
        cx,
        Req {
            object: tk.object,
            relation: tk.relation,
            user: tk.user,
            depth: 0,
            visited: Rc::new(Visited::Nil),
        },
    ))
}

/// `[object, relation, user, condition name, condition context]`
type TupleRow = (
    String,
    String,
    String,
    Option<String>,
    Option<Map<String, Value>>,
);

#[derive(Deserialize)]
struct AnswerJson {
    id: u64,
    #[serde(default)]
    tuples: Vec<TupleRow>,
    #[serde(default)]
    error: Option<String>,
}

impl Task {
    /// Answers previously requested reads (a JSON array, or empty), then
    /// advances. Returns `{"reads": [...], "cancel": [ids]}` (new reads, and
    /// earlier ones whose answers are no longer needed), `{"allowed": bool}`,
    /// `{"error": {...}}` or `{"refused": "..."}`. A host may answer any
    /// subset of the outstanding reads per step.
    pub fn step(&mut self, answers: &[u8]) -> Value {
        let mut typed = vec![];
        if !answers.is_empty() {
            let answers: Vec<AnswerJson> = match serde_json::from_slice(answers) {
                Ok(a) => a,
                Err(e) => return self.finish(refused(format!("answers JSON: {e}"))),
            };
            for a in answers {
                let ans = match a.error {
                    Some(e) => Err(e),
                    None => Ok(a
                        .tuples
                        .into_iter()
                        .map(|(object, relation, user, name, context)| Tuple {
                            object,
                            relation,
                            user,
                            condition: name
                                .filter(|n| !n.is_empty())
                                .map(|name| Cond { name, context }),
                        })
                        .collect()),
                };
                typed.push((a.id, ans));
            }
        }
        match self.advance(typed) {
            Step::Done(v) => v,
            Step::Reads { reads, cancelled } => {
                let reads: Vec<Value> = reads.into_iter().map(|(id, r)| read_json(id, r)).collect();
                json!({"reads": reads, "cancel": cancelled})
            }
        }
    }

    /// [`Task::step`] without JSON.
    pub fn advance(&mut self, answers: Vec<(u64, Answer)>) -> Step {
        {
            let mut io = self.io.borrow_mut();
            for (id, a) in answers {
                io.answer(id, a);
            }
        }
        let fut = match &mut self.state {
            State::Done(v) => return Step::Done(v.clone()),
            State::Running(f) => f,
        };
        match poll_once(fut) {
            Poll::Ready(out) => {
                let reads = self.io.borrow().reads;
                let mut v = conclude(out);
                v["reads_total"] = json!(reads);
                Step::Done(self.finish(v))
            }
            Poll::Pending => {
                let mut io = self.io.borrow_mut();
                if !io.busy() {
                    drop(io);
                    return Step::Done(self.finish(refused("resolution stalled")));
                }
                Step::Reads {
                    reads: io.take_queued(),
                    cancelled: io.take_cancelled(),
                }
            }
        }
    }

    /// Runs the task to completion, answering every pending read with
    /// `read` at each step, as a host whose store has no round trips to save
    /// would.
    pub fn run(&mut self, mut read: impl FnMut(&ReadReq) -> Answer) -> Value {
        let mut pending: std::collections::VecDeque<(u64, ReadReq)> = Default::default();
        let mut answers = vec![];
        loop {
            match self.advance(std::mem::take(&mut answers)) {
                Step::Done(v) => return v,
                Step::Reads { reads, cancelled } => {
                    pending.extend(reads);
                    pending.retain(|(id, _)| !cancelled.contains(id));
                }
            }
            if pending.is_empty() {
                return self.finish(refused("no reads to answer"));
            }
            // All at once: each advance polls every live branch, so answering
            // one read per advance would cost the square of the branches.
            answers.extend(pending.drain(..).map(|(id, r)| (id, read(&r))));
        }
    }

    fn finish(&mut self, v: Value) -> Value {
        self.state = State::Done(v.clone());
        v
    }
}

/// The API result, if every possible upstream outcome agrees on it.
fn conclude(out: Out) -> Value {
    match out.bits {
        T => json!({"allowed": true}),
        b if b & !(F | C) == 0 && b != 0 => json!({"allowed": false}),
        E if !out.err_ambiguous && out.err.is_some() => error(out.err.unwrap()),
        _ => refused(
            out.refused
                .unwrap_or_else(|| "OpenFGA's outcome depends on timing".into()),
        ),
    }
}

fn read_json(id: u64, r: ReadReq) -> Value {
    match r {
        ReadReq::Read { object, relation } => {
            json!({"id": id, "op": "read", "object": object, "relation": relation})
        }
        ReadReq::User {
            object,
            relation,
            user,
        } => {
            json!({"id": id, "op": "user", "object": object, "relation": relation, "user": user})
        }
        ReadReq::Usersets {
            object,
            relation,
            allowed,
        } => {
            let allowed: Vec<Value> = allowed
                .into_iter()
                .map(|r| {
                    let mut v = json!({"type": r.ty, "condition": r.condition});
                    match r.kind {
                        RefKind::Relation(rel) => v["relation"] = json!(rel),
                        RefKind::Wildcard => v["wildcard"] = json!(true),
                        RefKind::Unset => {}
                    }
                    v
                })
                .collect();
            json!({"id": id, "op": "usersets", "object": object, "relation": relation, "allowed": allowed})
        }
    }
}
