//! OpenFGA's API (model writes, tuple writes, checks) over any
//! [`crate::store::Store`]: the store keeps bytes, this layer decides.
//!
//! The application owns the transaction the store wraps: [`Fga::write_model`]
//! and [`Fga::write`] take a [`StoreMut`], and [`Fga::check`] a [`Store`]
//! (which may be the same uncommitted write transaction). Committing or
//! aborting is up to the caller, so authorization data changes atomically
//! with application data.
//!
//! Requests and responses are OpenFGA's (protojson): models go through the
//! model compiler, writes are validated as OpenFGA's `Write` validates them,
//! and checks run on the sans-IO evaluator, whose reads are answered from the
//! store as OpenFGA's SQL datastores answer them. Validation fails before
//! anything is written; if the store itself fails midway, the caller should
//! roll back.

use crate::compile::{self, Outcome};
use crate::exec::{Answer, ReadReq};
use crate::model::RefKind;
use crate::store::{Store, StoreMut};
use crate::strings::*;
use crate::validate::{validate_tuple_for_write, VErr};
use crate::{Cond, Engine, Semantics, Tuple};
use serde::Deserialize;
use serde_json::{json, Map, Value};
use std::collections::HashSet;

/// Encodes a tuple's condition as stored: `{"name", "context"}`, or nothing.
pub fn encode_condition(c: Option<&Cond>) -> Vec<u8> {
    c.map(|c| {
        serde_json::to_vec(&json!({"name": c.name, "context": c.context})).unwrap_or_default()
    })
    .unwrap_or_default()
}

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

/// Decodes a stored condition. Only empty bytes mean no condition: anything
/// else that does not decode is an error, never an unconditional tuple.
pub fn decode_condition(bytes: &[u8]) -> Result<Option<Cond>, String> {
    if bytes.is_empty() {
        return Ok(None);
    }
    #[derive(Deserialize)]
    struct C {
        name: String,
        #[serde(default)]
        context: Option<Map<String, Value>>,
    }
    serde_json::from_slice::<C>(bytes)
        .map(|c| {
            Some(Cond {
                name: c.name,
                context: c.context,
            })
        })
        .map_err(|e| format!("stored tuple condition: {e}"))
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

fn read(tuples: &impl Store, store: &str, r: &ReadReq) -> Answer {
    let tuple = |o: &str, rel: &str, u: String, c: &[u8]| {
        Ok(Tuple {
            object: o.into(),
            relation: rel.into(),
            user: u,
            condition: decode_condition(c)?,
        })
    };
    match r {
        ReadReq::User {
            object,
            relation,
            user,
        } => tuples
            .tuple(store, object, relation, user)?
            .map(|c| tuple(object, relation, user.clone(), &c))
            .into_iter()
            .collect(),
        ReadReq::Read { object, relation } => tuples
            .tuples(store, object, relation)?
            .into_iter()
            .map(|(u, c)| tuple(object, relation, u, &c))
            .collect(),
        ReadReq::Usersets {
            object,
            relation,
            allowed,
        } => tuples
            .tuples(store, object, relation)?
            .into_iter()
            .filter(|(u, _)| userset_matches(u, allowed))
            .map(|(u, c)| tuple(object, relation, u, &c))
            .collect(),
    }
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

/// OpenFGA's API over a [`Store`]. It holds compiled models (by ID) and the
/// server settings that matter; it holds no data.
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
    pub fn write_model(&self, tx: &impl StoreMut, store: &str, id: &str, request: &[u8]) -> Value {
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
        let stored = tx.put_model(store, id, &bytes);
        match stored {
            Ok(()) => json!({"authorization_model_id": id}),
            Err(e) => api_error("internal_error", e),
        }
    }

    /// Resolves the request's model (or the latest), loading it into the
    /// evaluator. Returns its ID.
    fn resolve_model(&self, tx: &impl Store, store: &str, id: &str) -> Result<String, Value> {
        let found = if id.is_empty() {
            tx.latest_model(store)
        } else {
            tx.model(store, id).map(|m| m.map(|m| (id.to_string(), m)))
        }
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
    pub fn write(&self, tx: &impl StoreMut, store: &str, request: &[u8]) -> Value {
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
            let mut deleting = vec![];
            for k in &deletes {
                let exists = tx.tuple(store, &k.object, &k.relation, &k.user)?.is_some();
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
                let encoded = encode_condition(t.condition.as_ref());
                if let Some(existing) = tx.tuple(store, &t.object, &t.relation, &t.user)? {
                    if on_duplicate_ignore {
                        // OpenFGA compares with proto.Equal against the stored
                        // tuple as read back, whose context is never absent (an
                        // empty one if none was written). So a request without a
                        // context conflicts even with an identical tuple.
                        let same = match (decode_condition(&existing)?, &t.condition) {
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
                tx.delete_tuple(store, &k.object, &k.relation, &k.user)?;
            }
            for (t, encoded) in writing {
                tx.put_tuple(store, &t.object, &t.relation, &t.user, &encoded)?;
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
        tx: &impl Store,
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
        let v = self
            .engine
            .check_with(&model_id, request, semantics)
            .run(|r| read(tx, store, r));
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
    use crate::store::MemoryStore;

    const MODEL: &str = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
        {"type":"doc","relations":{"viewer":{"this":{}}},
         "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user"}]}}}}]}"#;

    #[test]
    fn write_then_check() {
        let s = MemoryStore::default();
        let fga = Fga::default();
        assert_eq!(
            fga.write_model(&s, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", MODEL.as_bytes())
                ["authorization_model_id"],
            "01ARZ3NDEKTSV4RRFFQ69G5FAV"
        );
        let w = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne"}]}});
        assert_eq!(
            fga.write(&s, "s", &serde_json::to_vec(&w).unwrap()),
            json!({})
        );
        let check = |user: &str| {
            let req = json!({"tuple_key": {"object": "doc:1", "relation": "viewer", "user": user}});
            fga.check(
                &s,
                "s",
                &serde_json::to_vec(&req).unwrap(),
                Semantics::OpenFga,
            )
        };
        assert_eq!(check("user:anne")["allowed"], true);
        assert_eq!(check("user:bob")["allowed"], false);
        // The same write again fails, and changes nothing.
        assert_eq!(
            fga.write(&s, "s", &serde_json::to_vec(&w).unwrap())["error"]["code"],
            "write_failed_due_to_invalid_input"
        );
    }

    #[test]
    fn ignored_duplicates_compare_as_openfga_does() {
        let model = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
            {"type":"doc","relations":{"viewer":{"this":{}}},
             "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user","condition":"cx"}]}}}}],
            "conditions":{"cx":{"name":"cx","expression":"x < 1","parameters":{"x":{"type_name":"TYPE_NAME_INT"}}}}}"#;
        let s = MemoryStore::default();
        let fga = Fga::default();
        fga.write_model(&s, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", model.as_bytes());
        let write = |cond: Value, ignore: bool| {
            let mut w = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne", "condition": cond}]}});
            if ignore {
                w["writes"]["on_duplicate"] = json!("ignore");
            }
            fga.write(&s, "s", &serde_json::to_vec(&w).unwrap())
        };
        assert_eq!(write(json!({"name": "cx"}), false), json!({}));
        // The stored tuple reads back with an empty context, which a request
        // without one does not equal (proto.Equal), but `{}` does.
        assert_eq!(
            write(json!({"name": "cx"}), true)["error"]["code"],
            "aborted"
        );
        assert_eq!(write(json!({"name": "cx", "context": {}}), true), json!({}));
        assert_eq!(
            write(json!({"name": "cx", "context": {"x": 0}}), true)["error"]["code"],
            "aborted"
        );
    }

    #[test]
    fn malformed_stored_conditions_are_errors() {
        // A guarded tuple whose guard is false denies; were malformed bytes
        // read as "no condition", the unconditional user type would grant.
        let model = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
            {"type":"doc","relations":{"viewer":{"this":{}}},
             "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user"},{"type":"user","condition":"guard"}]}}}}],
            "conditions":{"guard":{"name":"guard","expression":"ok","parameters":{"ok":{"type_name":"TYPE_NAME_BOOL"}}}}}"#;
        let s = MemoryStore::default();
        let fga = Fga::default();
        fga.write_model(&s, "s", "01ARZ3NDEKTSV4RRFFQ69G5FAV", model.as_bytes());
        let check = || {
            let req = json!({"tuple_key": {"object": "doc:1", "relation": "viewer", "user": "user:anne"}});
            fga.check(
                &s,
                "s",
                &serde_json::to_vec(&req).unwrap(),
                Semantics::OpenFga,
            )
        };
        let put = |c: &[u8]| s.put_tuple("s", "doc:1", "viewer", "user:anne", c).unwrap();
        put(br#"{"name":"guard","context":{"ok":false}}"#);
        assert_eq!(check()["allowed"], false);
        let ignored = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne"}], "on_duplicate": "ignore"}});
        for bad in [
            &b"{"[..],
            b"null",
            b"[]",
            b"\"guard\"",
            b"{}",
            br#"{"name":1}"#,
            br#"{"name":"guard","context":[]}"#,
        ] {
            put(bad);
            let got = check();
            assert_eq!(
                got["error"]["code"],
                "internal_error",
                "{}: {got}",
                String::from_utf8_lossy(bad)
            );
            let got = fga.write(&s, "s", &serde_json::to_vec(&ignored).unwrap());
            assert_eq!(
                got["error"]["code"],
                "internal_error",
                "{}: {got}",
                String::from_utf8_lossy(bad)
            );
        }
    }
}
