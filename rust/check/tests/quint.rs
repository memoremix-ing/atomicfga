//! Replays traces of `spec/check.qnt` against the evaluator in canonical
//! semantics: after every step, the last check's answer must equal the
//! spec's least-fixed-point answer. Every check also runs in OpenFGA
//! semantics; where that answer differs from the canonical one, the
//! divergence is reported (OpenFGA's cycle flag can deny access the
//! canonical semantics grants).

use atomicfga_check::{Answer, Engine, ReadReq, RefKind, Semantics, Tuple};
use quint_connect::*;
use serde::Deserialize;
use serde_json::json;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Mutex;

/// The model of spec/check.qnt, in protojson.
fn model() -> serde_json::Value {
    let direct = |types: &[serde_json::Value]| json!({"directly_related_user_types": types});
    let user = json!({"type": "user"});
    let wildcard = json!({"type": "user", "wildcard": {}});
    let member = json!({"type": "group", "relation": "member"});
    let folder = json!({"type": "folder"});
    let this = json!({"this": {}});
    let computed = |r: &str| json!({"computed_userset": {"relation": r}});
    let ttu = |ts: &str, cu: &str| json!({"tuple_to_userset": {"tupleset": {"relation": ts}, "computed_userset": {"relation": cu}}});
    json!({
        "id": "01M5QUNTSPEC000000000000000",
        "schema_version": "1.1",
        "type_definitions": [
            {"type": "user"},
            {"type": "group", "relations": {"member": this},
             "metadata": {"relations": {"member": direct(&[user.clone(), wildcard, member.clone()])}}},
            {"type": "folder", "relations": {
                "parent": this,
                "owner": this,
                "viewer": {"union": {"child": [this, computed("owner"), ttu("parent", "viewer")]}}},
             "metadata": {"relations": {
                "parent": direct(std::slice::from_ref(&folder)),
                "owner": direct(&[user.clone(), member.clone()]),
                "viewer": direct(&[user.clone(), member.clone()])}}},
            {"type": "doc", "relations": {
                "parent": this,
                "blocked": this,
                "editor": this,
                "public": this,
                "viewer": {"difference": {
                    "base": {"union": {"child": [computed("editor"), ttu("parent", "viewer")]}},
                    "subtract": computed("blocked")}},
                "restricted": {"intersection": {"child": [computed("editor"), ttu("parent", "viewer")]}}},
             "metadata": {"relations": {
                "parent": direct(&[folder]),
                "blocked": direct(&[user.clone(), member]),
                "editor": direct(&[user]),
                "public": direct(&[json!({"type": "user", "wildcard": {}})])}}},
        ],
    })
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
struct TupleRec {
    object: String,
    relation: String,
    user: String,
}

#[derive(Debug, Deserialize, PartialEq, Eq)]
struct CheckResult {
    object: String,
    relation: String,
    user: String,
    allowed: bool,
}

#[derive(Debug, Deserialize, PartialEq, Eq)]
struct SpecState {
    result: CheckResult,
}

impl State<CheckDriver> for SpecState {
    fn from_driver(d: &CheckDriver) -> Result<Self> {
        Ok(SpecState {
            result: CheckResult {
                object: d.result.0.clone(),
                relation: d.result.1.clone(),
                user: d.result.2.clone(),
                allowed: d.result.3,
            },
        })
    }
}

struct CheckDriver {
    engine: Engine,
    model: String,
    stored: Vec<TupleRec>,
    result: (String, String, String, bool),
}

impl Default for CheckDriver {
    fn default() -> Self {
        let engine = Engine::default();
        let js = serde_json::to_vec(&json!({"model": model()})).unwrap();
        let model = engine.load_model(&js).expect("model loads");
        CheckDriver {
            engine,
            model,
            stored: vec![],
            result: Default::default(),
        }
    }
}

static CHECKS: AtomicUsize = AtomicUsize::new(0);
static DIVERGENCES: Mutex<Vec<String>> = Mutex::new(vec![]);

impl CheckDriver {
    /// `ReadUserTuple`, `Read` and `ReadUsersetTuples` over the stored tuples,
    /// as OpenFGA's SQL datastores answer them.
    fn read(&self, r: &ReadReq) -> Answer {
        let tuple = |t: &TupleRec| Tuple {
            object: t.object.clone(),
            relation: t.relation.clone(),
            user: t.user.clone(),
            condition: None,
        };
        Ok(match r {
            ReadReq::User {
                object,
                relation,
                user,
            } => self
                .stored
                .iter()
                .filter(|t| t.object == *object && t.relation == *relation && t.user == *user)
                .take(1)
                .map(tuple)
                .collect(),
            ReadReq::Read { object, relation } => self
                .stored
                .iter()
                .filter(|t| t.object == *object && t.relation == *relation)
                .map(tuple)
                .collect(),
            ReadReq::Usersets {
                object,
                relation,
                allowed,
            } => self
                .stored
                .iter()
                .filter(|t| t.object == *object && t.relation == *relation)
                .filter(|t| {
                    allowed.iter().any(|a| match &a.kind {
                        RefKind::Wildcard => t.user == format!("{}:*", a.ty),
                        RefKind::Relation(rel) => {
                            t.user.starts_with(&format!("{}:", a.ty))
                                && t.user.ends_with(&format!("#{rel}"))
                        }
                        RefKind::Unset => false,
                    })
                })
                .map(tuple)
                .collect(),
        })
    }

    fn run(&self, q: &TupleRec, semantics: Semantics) -> serde_json::Value {
        let req = serde_json::to_vec(
            &json!({"tuple_key": {"object": q.object, "relation": q.relation, "user": q.user}}),
        )
        .unwrap();
        self.engine
            .check_with(&self.model, &req, semantics)
            .run(|r| self.read(r))
    }

    fn check(&mut self, q: TupleRec) {
        let canonical = self.run(&q, Semantics::Canonical);
        let allowed = canonical["allowed"]
            .as_bool()
            .unwrap_or_else(|| panic!("canonical check of {q:?} did not answer: {canonical}"));
        CHECKS.fetch_add(1, Ordering::Relaxed);
        let openfga = self.run(&q, Semantics::OpenFga);
        if let Some(b) = openfga["allowed"].as_bool() {
            if b != allowed {
                DIVERGENCES.lock().unwrap().push(format!(
                    "{}#{}@{}: canonical {allowed}, OpenFGA {b}, with tuples {:?}",
                    q.object,
                    q.relation,
                    q.user,
                    self.stored
                        .iter()
                        .map(|t| format!("{}#{}@{}", t.object, t.relation, t.user))
                        .collect::<Vec<_>>()
                ));
            }
        }
        self.result = (q.object, q.relation, q.user, allowed);
    }
}

impl Driver for CheckDriver {
    type State = SpecState;

    fn step(&mut self, step: &Step) -> Result {
        switch!(step {
            init => {
                self.stored.clear();
                self.result = Default::default();
            },
            WriteTuple(t) => {
                if !self.stored.contains(&t) {
                    self.stored.push(t);
                }
            },
            DeleteTuple(t) => self.stored.retain(|s| *s != t),
            Check(q) => self.check(q),
        })
    }
}

#[quint_run(spec = "../../spec/check.qnt", max_samples = 200, max_steps = 40)]
fn canonical_check_matches_spec() -> impl Driver {
    CheckDriver::default()
}

static REPORTED: AtomicUsize = AtomicUsize::new(0);

impl Drop for CheckDriver {
    /// Reports divergences found by the trace just replayed.
    fn drop(&mut self) {
        let d = DIVERGENCES.lock().unwrap();
        let from = REPORTED.swap(d.len(), Ordering::Relaxed);
        for line in &d[from.min(d.len())..] {
            eprintln!(
                "   OpenFGA semantics diverge from canonical ({} checks so far): {line}",
                CHECKS.load(Ordering::Relaxed)
            );
        }
    }
}
