//! A [redb](https://www.redb.org) store for the evaluator: authorization
//! models and tuples in the caller's redb transactions.
//!
//! As with atomicfga's SQL sessions, the application owns the transaction:
//! [`Fga::write_model`] and [`Fga::write`] take a `&WriteTransaction`, and
//! [`Fga::check`] takes either a `&ReadTransaction` or a `&WriteTransaction`
//! (seeing its uncommitted writes). Committing or aborting is up to the
//! caller, so authorization data changes atomically with application data.
//!
//! Requests and responses are OpenFGA's (protojson): models go through the
//! model compiler, writes are validated as OpenFGA's `Write` validates them,
//! and checks run on the sans-IO evaluator, whose reads are answered here
//! as OpenFGA's SQL datastores answer them.

use crate::compile::{self, Outcome};
use crate::exec::{Answer, ReadReq};
use crate::model::RefKind;
use crate::strings::*;
use crate::validate::{validate_tuple_for_write, VErr};
use crate::{Cond, Engine, Semantics, Tuple};
use redb::{ReadTransaction, ReadableTable, TableDefinition, TableError, WriteTransaction};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::collections::HashSet;

/// (store, object, relation, user) → condition (`{"name", "context"}`, or
/// empty).
const TUPLES: TableDefinition<(&str, &str, &str, &str), &[u8]> =
    TableDefinition::new("atomicfga_tuples");
/// (store, model ID) → the model in protojson.
const MODELS: TableDefinition<(&str, &str), &[u8]> = TableDefinition::new("atomicfga_models");

/// OpenFGA defaults.
const MAX_TUPLES_PER_WRITE: usize = 100;
const MAX_CONTEXTUAL_TUPLES: usize = 100;

fn api_error(code: &str, message: impl Into<String>) -> Value {
    json!({"error": {"code": code, "message": message.into()}})
}

fn refused(why: impl Into<String>) -> Value {
    json!({"refused": why.into()})
}

/// `^[^\s]{min,max}$` (RE2: ASCII whitespace; length in characters).
fn no_space(s: &str, min: usize, max: usize) -> bool {
    let n = s.chars().count();
    (min..=max).contains(&n)
        && !s
            .chars()
            .any(|c| matches!(c, '\t' | '\n' | '\x0c' | '\r' | ' '))
}

/// `^[^:#@\s]{1,50}$`.
fn relation_ok(s: &str) -> bool {
    no_space(s, 1, 50) && !s.contains([':', '#', '@'])
}

fn ulid_ok(s: &str) -> bool {
    s.len() == 26
        && s.bytes().all(|b| {
            b.is_ascii_digit()
                || (b.is_ascii_uppercase() && !matches!(b, b'I' | b'L' | b'O' | b'U'))
        })
}

/// Read access to the tables, in either kind of transaction.
pub trait Source {
    fn with_tuples<R>(&self, f: impl FnOnce(&dyn TupleTable) -> R) -> Result<R, String>;
    fn with_models<R>(&self, f: impl FnOnce(&dyn ModelTable) -> R) -> Result<R, String>;
}

pub trait TupleTable {
    fn scan(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String>;
    fn get(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String>;
}

pub trait ModelTable {
    fn get(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String>;
    fn latest(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String>;
}

struct Empty;

impl TupleTable for Empty {
    fn scan(&self, _: &str, _: &str, _: &str) -> Result<Vec<(String, Vec<u8>)>, String> {
        Ok(vec![])
    }
    fn get(&self, _: &str, _: &str, _: &str, _: &str) -> Result<Option<Vec<u8>>, String> {
        Ok(None)
    }
}

impl ModelTable for Empty {
    fn get(&self, _: &str, _: &str) -> Result<Option<Vec<u8>>, String> {
        Ok(None)
    }
    fn latest(&self, _: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        Ok(None)
    }
}

impl<T: ReadableTable<(&'static str, &'static str, &'static str, &'static str), &'static [u8]>>
    TupleTable for T
{
    fn scan(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        let mut out = vec![];
        for row in self
            .range((store, object, relation, "")..)
            .map_err(|e| e.to_string())?
        {
            let (k, v) = row.map_err(|e| e.to_string())?;
            let (s, o, r, u) = k.value();
            if s != store || o != object || r != relation {
                break;
            }
            out.push((u.to_string(), v.value().to_vec()));
        }
        Ok(out)
    }
    fn get(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        Ok(ReadableTable::get(self, (store, object, relation, user))
            .map_err(|e| e.to_string())?
            .map(|v| v.value().to_vec()))
    }
}

struct Models<T>(T);

impl<T: ReadableTable<(&'static str, &'static str), &'static [u8]>> ModelTable for Models<T> {
    fn get(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        Ok(self
            .0
            .get((store, id))
            .map_err(|e| e.to_string())?
            .map(|v| v.value().to_vec()))
    }
    fn latest(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        let mut last = None;
        for row in self.0.range((store, "")..).map_err(|e| e.to_string())? {
            let (k, v) = row.map_err(|e| e.to_string())?;
            let (s, id) = k.value();
            if s != store {
                break;
            }
            last = Some((id.to_string(), v.value().to_vec()));
        }
        Ok(last)
    }
}

impl Source for ReadTransaction {
    fn with_tuples<R>(&self, f: impl FnOnce(&dyn TupleTable) -> R) -> Result<R, String> {
        match self.open_table(TUPLES) {
            Ok(t) => Ok(f(&t)),
            Err(TableError::TableDoesNotExist(_)) => Ok(f(&Empty)),
            Err(e) => Err(e.to_string()),
        }
    }
    fn with_models<R>(&self, f: impl FnOnce(&dyn ModelTable) -> R) -> Result<R, String> {
        match self.open_table(MODELS) {
            Ok(t) => Ok(f(&Models(t))),
            Err(TableError::TableDoesNotExist(_)) => Ok(f(&Empty)),
            Err(e) => Err(e.to_string()),
        }
    }
}

impl Source for WriteTransaction {
    fn with_tuples<R>(&self, f: impl FnOnce(&dyn TupleTable) -> R) -> Result<R, String> {
        let t = self.open_table(TUPLES).map_err(|e| e.to_string())?;
        Ok(f(&t))
    }
    fn with_models<R>(&self, f: impl FnOnce(&dyn ModelTable) -> R) -> Result<R, String> {
        let t = self.open_table(MODELS).map_err(|e| e.to_string())?;
        Ok(f(&Models(t)))
    }
}

fn decode_condition(bytes: &[u8]) -> Option<Cond> {
    if bytes.is_empty() {
        return None;
    }
    #[derive(Deserialize)]
    struct C {
        name: String,
        #[serde(default)]
        context: Option<Map<String, Value>>,
    }
    serde_json::from_slice::<C>(bytes).ok().map(|c| Cond {
        name: c.name,
        context: c.context,
    })
}

/// `user_type = 'userset'` and the restriction filter of the SQL datastores'
/// `ReadUsersetTuples`, over the user's parts (`type:id#relation`).
fn userset_matches(user: &str, allowed: &[crate::RelRef]) -> bool {
    if !is_userset_user_type(user) {
        return false;
    }
    let (object, relation) = split_object_relation(user);
    let (ty, id) = split_object(object);
    allowed.iter().any(|a| match &a.kind {
        RefKind::Relation(r) => ty == a.ty && relation == r,
        RefKind::Wildcard => ty == a.ty && id == WILDCARD,
        RefKind::Unset => false,
    })
}

fn read(tuples: &dyn TupleTable, store: &str, r: &ReadReq) -> Answer {
    let tuple = |o: &str, rel: &str, u: String, c: &[u8]| Tuple {
        object: o.into(),
        relation: rel.into(),
        user: u,
        condition: decode_condition(c),
    };
    Ok(match r {
        ReadReq::User {
            object,
            relation,
            user,
        } => tuples
            .get(store, object, relation, user)?
            .map(|c| tuple(object, relation, user.clone(), &c))
            .into_iter()
            .collect(),
        ReadReq::Read { object, relation } => tuples
            .scan(store, object, relation)?
            .into_iter()
            .map(|(u, c)| tuple(object, relation, u, &c))
            .collect(),
        ReadReq::Usersets {
            object,
            relation,
            allowed,
        } => tuples
            .scan(store, object, relation)?
            .into_iter()
            .filter(|(u, _)| userset_matches(u, allowed))
            .map(|(u, c)| tuple(object, relation, u, &c))
            .collect(),
    })
}

#[derive(Deserialize)]
struct KeyJson {
    #[serde(default)]
    object: String,
    #[serde(default)]
    relation: String,
    #[serde(default)]
    user: String,
    #[serde(default)]
    condition: Option<CondJson>,
}

#[derive(Deserialize, Clone)]
struct CondJson {
    #[serde(default)]
    name: String,
    #[serde(default)]
    context: Option<Map<String, Value>>,
}

#[derive(Deserialize)]
struct WritesJson {
    #[serde(default)]
    tuple_keys: Vec<KeyJson>,
    #[serde(default)]
    on_duplicate: String,
}

#[derive(Deserialize)]
struct DeletesJson {
    #[serde(default)]
    tuple_keys: Vec<KeyJson>,
    #[serde(default)]
    on_missing: String,
}

#[derive(Deserialize)]
struct WriteRequestJson {
    #[serde(default)]
    authorization_model_id: String,
    #[serde(default)]
    writes: Option<WritesJson>,
    #[serde(default)]
    deletes: Option<DeletesJson>,
}

/// TupleKey's (and TupleKeyWithoutCondition's) protobuf rules.
fn key_proto_ok(k: &KeyJson) -> bool {
    k.user.len() <= 512
        && (k.relation.is_empty() || relation_ok(&k.relation))
        && (k.object.is_empty() || no_space(&k.object, 2, 256))
        && k.condition
            .as_ref()
            .is_none_or(|c| no_space(&c.name, 2, 256))
}

/// OpenFGA's API, over redb transactions.
#[derive(Default)]
pub struct Fga {
    engine: Engine,
    /// Whether models may use `$expression` conditions (OpenFGA's
    /// `inline_expressions` experimental flag).
    pub inline_expressions: bool,
}


impl Fga {
    /// `WriteAuthorizationModel`, under a model ID the caller chose (a ULID:
    /// the latest model is the greatest ID). Returns `{"authorization_model_id"}`
    /// or `{"error": {code, message}}` (or `{"refused"}` when the model
    /// compiler cannot decide).
    pub fn write_model(
        &self,
        tx: &WriteTransaction,
        store: &str,
        id: &str,
        request: &[u8],
    ) -> Value {
        let options = compile::Options {
            inline_expressions: self.inline_expressions,
        };
        match compile::compile(request, options) {
            Outcome::Valid => {}
            Outcome::Invalid { code, message } => return api_error(code, message),
            Outcome::Undecided(why) => return refused(why),
        }
        // Store the model in protojson with its ID, as the evaluator loads it.
        let mut model: Value = match serde_json::from_slice(request) {
            Ok(v) => v,
            Err(e) => return api_error("invalid_argument", e.to_string()),
        };
        if let Value::Object(m) = &mut model {
            m.remove("store_id");
            m.insert("id".into(), json!(id));
        }
        let bytes = serde_json::to_vec(&model).unwrap_or_default();
        let stored = (|| -> Result<(), String> {
            let mut t = tx.open_table(MODELS).map_err(|e| e.to_string())?;
            t.insert((store, id), bytes.as_slice())
                .map_err(|e| e.to_string())?;
            Ok(())
        })();
        match stored {
            Ok(()) => json!({"authorization_model_id": id}),
            Err(e) => api_error("internal_error", e),
        }
    }

    /// Resolves the request's model (or the latest), loading it into the
    /// evaluator. Returns its ID.
    fn resolve_model(&self, tx: &impl Source, store: &str, id: &str) -> Result<String, Value> {
        let found = tx
            .with_models(|t| {
                if id.is_empty() {
                    t.latest(store)
                } else {
                    t.get(store, id).map(|m| m.map(|m| (id.to_string(), m)))
                }
            })
            .and_then(|r| r)
            .map_err(|e| api_error("internal_error", e))?;
        let Some((id, model)) = found else {
            return Err(if id.is_empty() {
                api_error(
                    "latest_authorization_model_not_found",
                    format!("No authorization models found for store '{store}'"),
                )
            } else {
                api_error(
                    "authorization_model_not_found",
                    format!("Authorization Model '{id}' not found"),
                )
            });
        };
        if !self.engine.has_model(&id) {
            let js = serde_json::to_vec(
                &json!({"model": serde_json::from_slice::<Value>(&model).unwrap_or_default()}),
            )
            .unwrap_or_default();
            self.engine
                .load_model(&js)
                .map_err(|e| api_error("internal_error", e))?;
        }
        Ok(id)
    }

    /// `Write`: deletes, then writes, validated as OpenFGA validates them.
    /// Returns `{}` or `{"error": {code, message}}`; on an error nothing is
    /// changed. A write whose validity this port cannot decide (a condition
    /// parameter of an unsupported type) is `{"refused"}`.
    pub fn write(&self, tx: &WriteTransaction, store: &str, request: &[u8]) -> Value {
        let req: WriteRequestJson = match serde_json::from_slice(request) {
            Ok(r) => r,
            Err(e) => return api_error("invalid_argument", format!("write request: {e}")),
        };
        // Protobuf rules.
        let invalid = |m: &str| api_error("invalid_argument", m.to_string());
        if !req.authorization_model_id.is_empty() && !ulid_ok(&req.authorization_model_id) {
            return invalid("AuthorizationModelId does not match the pattern");
        }
        for (keys, present) in [
            (
                req.writes.as_ref().map(|w| &w.tuple_keys),
                req.writes.is_some(),
            ),
            (
                req.deletes.as_ref().map(|d| &d.tuple_keys),
                req.deletes.is_some(),
            ),
        ] {
            let keys = keys.map(Vec::as_slice).unwrap_or_default();
            if present && keys.is_empty() {
                return invalid("TupleKeys: value must contain at least 1 item(s)");
            }
            if !keys.iter().all(key_proto_ok) {
                return invalid("invalid TupleKey");
            }
        }
        if req
            .deletes
            .as_ref()
            .is_some_and(|d| d.tuple_keys.iter().any(|k| k.condition.is_some()))
        {
            return invalid("deletes take no condition");
        }
        let model_id = match self.resolve_model(tx, store, &req.authorization_model_id) {
            Ok(id) => id,
            Err(e) => return e,
        };
        let writes: Vec<Tuple> = req
            .writes
            .as_ref()
            .map(|w| {
                w.tuple_keys
                    .iter()
                    .map(|k| Tuple {
                        object: k.object.clone(),
                        relation: k.relation.clone(),
                        user: k.user.clone(),
                        condition: k.condition.clone().map(|c| Cond {
                            name: c.name,
                            context: c.context,
                        }),
                    })
                    .collect()
            })
            .unwrap_or_default();
        if !self.inline_expressions
            && writes.iter().any(|t| {
                t.condition
                    .as_ref()
                    .is_some_and(|c| c.name == "$expression")
            })
        {
            return api_error(
                "validation_error",
                "$expression requires the \"inline_expressions\" experimental feature flag",
            );
        }
        let deletes: Vec<&KeyJson> = req
            .deletes
            .as_ref()
            .map(|d| d.tuple_keys.iter().collect())
            .unwrap_or_default();
        if writes.is_empty() && deletes.is_empty() {
            return api_error(
                "invalid_write_input",
                "Invalid input. Make sure you provide at least one write, or at least one delete",
            );
        }
        let on_duplicate_ignore = match req.writes.as_ref().map_or("", |w| w.on_duplicate.as_str())
        {
            "" | "error" => false,
            "ignore" => true,
            other => {
                return api_error(
                    "validation_error",
                    format!("invalid on_duplicate option: {other}"),
                )
            }
        };
        let on_missing_ignore = match req.deletes.as_ref().map_or("", |d| d.on_missing.as_str()) {
            "" | "error" => false,
            "ignore" => true,
            other => {
                return api_error(
                    "validation_error",
                    format!("invalid on_missing option: {other}"),
                )
            }
        };
        if !writes.is_empty() {
            let Some(Ok(model)) = self.engine.model(&model_id) else {
                return refused("model could not be loaded");
            };
            for t in &writes {
                match validate_tuple_for_write(&model, t) {
                    Ok(()) => {}
                    Err(VErr::Invalid(e)) => return api_error("validation_error", e),
                    Err(VErr::Refuse(r)) => return refused(r),
                }
                if is_self_defining(&t.object, &t.relation, &t.user) {
                    return api_error("validation_error", "cannot write a tuple that is implicit");
                }
                // OpenFGA limits the protobuf size of the context to 32 KiB,
                // at most about four times its compact JSON.
                if let Some(ctx) = t.condition.as_ref().and_then(|c| c.context.as_ref()) {
                    if serde_json::to_vec(ctx).map_or(usize::MAX, |v| v.len()) > 8 * 1024 {
                        return refused("condition context may exceed the size limit");
                    }
                }
            }
        }
        if deletes.iter().any(|k| !is_valid_user(&k.user)) {
            return api_error("validation_error", "the 'user' field is malformed");
        }
        let mut seen = HashSet::new();
        for (o, r, u) in deletes
            .iter()
            .map(|k| (&k.object, &k.relation, &k.user))
            .chain(writes.iter().map(|t| (&t.object, &t.relation, &t.user)))
        {
            if !seen.insert((o.clone(), r.clone(), u.clone())) {
                return api_error(
                    "cannot_allow_duplicate_tuples_in_one_request",
                    format!(
                        "duplicate tuple in write: user: '{u}', relation: '{r}', object: '{o}'"
                    ),
                );
            }
        }
        if seen.len() > MAX_TUPLES_PER_WRITE {
            return api_error("exceeded_entity_limit", format!("The number of write operations exceeds the allowed limit of {MAX_TUPLES_PER_WRITE}"));
        }
        // Apply all or nothing: check first, then mutate.
        let applied = (|| -> Result<Option<Value>, String> {
            let mut table = tx.open_table(TUPLES).map_err(|e| e.to_string())?;
            let mut deleting = vec![];
            for k in &deletes {
                let exists =
                    TupleTable::get(&table, store, &k.object, &k.relation, &k.user)?.is_some();
                if !exists {
                    if on_missing_ignore {
                        continue;
                    }
                    return Ok(Some(api_error(
                        "write_failed_due_to_invalid_input",
                        format!("cannot delete a tuple which does not exist: user: '{}', relation: '{}', object: '{}'", k.user, k.relation, k.object),
                    )));
                }
                deleting.push(*k);
            }
            let mut writing = vec![];
            for t in &writes {
                let encoded = t
                    .condition
                    .as_ref()
                    .map(|c| {
                        serde_json::to_vec(&json!({"name": c.name, "context": c.context}))
                            .unwrap_or_default()
                    })
                    .unwrap_or_default();
                if let Some(existing) =
                    TupleTable::get(&table, store, &t.object, &t.relation, &t.user)?
                {
                    if on_duplicate_ignore {
                        // OpenFGA compares with proto.Equal against the stored
                        // tuple as read back, whose context is never absent (an
                        // empty one if none was written). So a request without a
                        // context conflicts even with an identical tuple.
                        let same = match (decode_condition(&existing), &t.condition) {
                            (None, None) => true,
                            (Some(a), Some(b)) => {
                                a.name == b.name
                                    && b.context.as_ref() == Some(&a.context.unwrap_or_default())
                            }
                            _ => false,
                        };
                        if same {
                            continue;
                        }
                        return Ok(Some(api_error("aborted", "attempted to write a tuple which already exists with a different condition")));
                    }
                    return Ok(Some(api_error(
                        "write_failed_due_to_invalid_input",
                        format!("cannot write a tuple which already exists: user: '{}', relation: '{}', object: '{}'", t.user, t.relation, t.object),
                    )));
                }
                writing.push((t, encoded));
            }
            for k in deleting {
                table
                    .remove((
                        store,
                        k.object.as_str(),
                        k.relation.as_str(),
                        k.user.as_str(),
                    ))
                    .map_err(|e| e.to_string())?;
            }
            for (t, encoded) in writing {
                table
                    .insert(
                        (
                            store,
                            t.object.as_str(),
                            t.relation.as_str(),
                            t.user.as_str(),
                        ),
                        encoded.as_slice(),
                    )
                    .map_err(|e| e.to_string())?;
            }
            Ok(None)
        })();
        match applied {
            Ok(None) => json!({}),
            Ok(Some(e)) => e,
            Err(e) => api_error("internal_error", e),
        }
    }

    /// `Check` (a CheckRequest in protojson; its store ID is ignored),
    /// reading the transaction's tuples.
    pub fn check(
        &self,
        tx: &impl Source,
        store: &str,
        request: &[u8],
        semantics: Semantics,
    ) -> Value {
        #[derive(Deserialize)]
        struct Req {
            #[serde(default)]
            authorization_model_id: String,
            #[serde(default)]
            tuple_key: Option<KeyJson>,
            #[serde(default)]
            contextual_tuples: Option<Ctx>,
        }
        #[derive(Deserialize)]
        struct Ctx {
            #[serde(default)]
            tuple_keys: Vec<KeyJson>,
        }
        let req: Req = match serde_json::from_slice(request) {
            Ok(r) => r,
            Err(e) => return api_error("invalid_argument", format!("check request: {e}")),
        };
        // CheckRequest's protobuf rules.
        let invalid = |m: &str| api_error("invalid_argument", m.to_string());
        let Some(tk) = &req.tuple_key else {
            return invalid("TupleKey: value is required");
        };
        if !no_space(&tk.user, 2, 512)
            || !relation_ok(&tk.relation)
            || !no_space(&tk.object, 2, 256)
        {
            return invalid("invalid CheckRequestTupleKey");
        }
        if !req.authorization_model_id.is_empty() && !ulid_ok(&req.authorization_model_id) {
            return invalid("AuthorizationModelId does not match the pattern");
        }
        let contextual = req
            .contextual_tuples
            .as_ref()
            .map(|c| c.tuple_keys.as_slice())
            .unwrap_or_default();
        if contextual.len() > MAX_CONTEXTUAL_TUPLES || !contextual.iter().all(key_proto_ok) {
            return invalid("invalid ContextualTupleKeys");
        }
        let model_id = match self.resolve_model(tx, store, &req.authorization_model_id) {
            Ok(id) => id,
            Err(e) => return e,
        };
        let outcome = tx.with_tuples(|tuples| {
            self.engine
                .check_with(&model_id, request, semantics)
                .run(|r| read(tuples, store, r))
        });
        let v = match outcome {
            Ok(v) => v,
            Err(e) => return api_error("internal_error", e),
        };
        // The evaluator reports errors by kind; give them OpenFGA's codes.
        match v.get("error") {
            Some(e) => {
                let code = match e["kind"].as_str() {
                    Some("validation") => "validation_error",
                    Some("invalid_tuple") => "invalid_tuple",
                    Some("too_complex") => "authorization_model_resolution_too_complex",
                    _ => "internal_error",
                };
                api_error(code, e["message"].as_str().unwrap_or_default())
            }
            None => v,
        }
    }
}

impl Engine {
    pub(crate) fn model(
        &self,
        id: &str,
    ) -> Option<Result<std::sync::Arc<crate::model::Model>, String>> {
        self.models.lock().unwrap().get(id).cloned()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use redb::{backends::InMemoryBackend, Database, ReadableDatabase};

    const MODEL: &str = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
        {"type":"doc","relations":{"viewer":{"this":{}}},
         "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user"}]}}}}]}"#;

    fn check(fga: &Fga, tx: &impl Source, user: &str) -> Value {
        let req = json!({"tuple_key": {"object": "doc:1", "relation": "viewer", "user": user}});
        fga.check(
            tx,
            "s",
            &serde_json::to_vec(&req).unwrap(),
            Semantics::OpenFga,
        )
    }

    #[test]
    fn caller_owned_transactions() {
        let db = Database::builder()
            .create_with_backend(InMemoryBackend::new())
            .unwrap();
        let fga = Fga::default();
        let tx = db.begin_write().unwrap();
        assert_eq!(
            fga.write_model(&tx, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", MODEL.as_bytes())
                ["authorization_model_id"],
            "01ARZ3NDEKTSV4RRFFQ69G5FAV"
        );
        let w = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne"}]}});
        assert_eq!(
            fga.write(&tx, "s", &serde_json::to_vec(&w).unwrap()),
            json!({})
        );
        // Read-your-writes inside the uncommitted transaction.
        assert_eq!(check(&fga, &tx, "user:anne")["allowed"], true);
        assert_eq!(check(&fga, &tx, "user:bob")["allowed"], false);
        // The same write again fails, and changes nothing.
        assert_eq!(
            fga.write(&tx, "s", &serde_json::to_vec(&w).unwrap())["error"]["code"],
            "write_failed_due_to_invalid_input"
        );
        tx.abort().unwrap();
        // Rolled back: no model, no tuple.
        let r = db.begin_read().unwrap();
        assert_eq!(
            check(&fga, &r, "user:anne")["error"]["code"],
            "latest_authorization_model_not_found"
        );
        drop(r);

        let tx = db.begin_write().unwrap();
        fga.write_model(&tx, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", MODEL.as_bytes());
        fga.write(&tx, "s", &serde_json::to_vec(&w).unwrap());
        tx.commit().unwrap();
        let r = db.begin_read().unwrap();
        assert_eq!(check(&fga, &r, "user:anne")["allowed"], true);
    }

    #[test]
    fn ignored_duplicates_compare_as_openfga_does() {
        let model = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
            {"type":"doc","relations":{"viewer":{"this":{}}},
             "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user","condition":"cx"}]}}}}],
            "conditions":{"cx":{"name":"cx","expression":"x < 1","parameters":{"x":{"type_name":"TYPE_NAME_INT"}}}}}"#;
        let db = Database::builder().create_with_backend(InMemoryBackend::new()).unwrap();
        let fga = Fga::default();
        let tx = db.begin_write().unwrap();
        fga.write_model(&tx, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", model.as_bytes());
        let write = |cond: Value, ignore: bool| {
            let mut w = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne", "condition": cond}]}});
            if ignore {
                w["writes"]["on_duplicate"] = json!("ignore");
            }
            fga.write(&tx, "s", &serde_json::to_vec(&w).unwrap())
        };
        assert_eq!(write(json!({"name": "cx"}), false), json!({}));
        // The stored tuple reads back with an empty context, which a request
        // without one does not equal (proto.Equal), but `{}` does.
        assert_eq!(write(json!({"name": "cx"}), true)["error"]["code"], "aborted");
        assert_eq!(write(json!({"name": "cx", "context": {}}), true), json!({}));
        assert_eq!(write(json!({"name": "cx", "context": {"x": 0}}), true)["error"]["code"], "aborted");
    }
}
