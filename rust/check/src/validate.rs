//! OpenFGA's internal/validation, for the functions Check uses.

use crate::cel::Bad;
use crate::model::{Model, RefKind, RelRef, Rewrite, Undefined};
use crate::strings::*;
use crate::Tuple;
use serde_json::{Map, Value};

pub enum VErr {
    /// The tuple is invalid (or the request malformed).
    Invalid(String),
    /// This port cannot tell.
    Refuse(String),
}

fn invalid<T>(m: impl Into<String>) -> Result<T, VErr> {
    Err(VErr::Invalid(m.into()))
}

fn undefined(u: Undefined, object_type: &str, relation: &str) -> String {
    match u {
        Undefined::Type => format!("type '{object_type}' not found"),
        Undefined::Relation => format!("relation '{object_type}#{relation}' not found"),
    }
}

pub fn validate_user(m: &Model, user: &str) -> Result<(), String> {
    if !is_valid_user(user) {
        return Err("the 'user' field is malformed".into());
    }
    let is_valid_object = is_valid_object(user);
    let is_valid_userset = is_object_relation(user);
    let (user_object, user_relation) = split_object_relation(user);
    let user_object_type = get_type(user_object);
    // Schema 1.1 and 1.2 only (Model::from_json refuses the rest).
    if !is_valid_object && !is_valid_userset {
        return Err("the 'user' field must be an object (e.g. document:1) or an 'object#relation' or a typed wildcard (e.g. group:*)".into());
    }
    if !m.has_type(user_object_type) {
        return Err(format!("type '{user_object_type}' not found"));
    }
    if is_valid_userset {
        if let Err(u) = m.get_relation(user_object_type, user_relation) {
            return Err(undefined(u, user_object_type, user_relation));
        }
    }
    Ok(())
}

pub fn validate_object(m: &Model, object: &str) -> Result<(), String> {
    if !is_valid_object(object) {
        return Err("invalid 'object' field format".into());
    }
    let (object_type, id) = split_object(object);
    if id == WILDCARD {
        return Err("the 'object' field cannot reference a typed wildcard".into());
    }
    if !m.has_type(object_type) {
        return Err(format!("type '{object_type}' not found"));
    }
    Ok(())
}

pub fn validate_relation(m: &Model, object: &str, relation: &str) -> Result<(), String> {
    if !is_valid_relation(relation) {
        return Err("the 'relation' field is malformed".into());
    }
    let object_type = get_type(object);
    m.get_relation(object_type, relation)
        .map(|_| ())
        .map_err(|u| undefined(u, object_type, relation))
}

pub fn validate_user_object_relation(
    m: &Model,
    object: &str,
    relation: &str,
    user: &str,
) -> Result<(), String> {
    validate_user(m, user)?;
    validate_object(m, object)?;
    validate_relation(m, object, relation)
}

/// `ValidateTupleForWrite`, as applied to contextual tuples.
pub fn validate_tuple_for_write(m: &Model, t: &Tuple) -> Result<(), VErr> {
    validate_user_object_relation(m, &t.object, &t.relation, &t.user).map_err(VErr::Invalid)?;
    validate_tuple_for_read(m, t)?;
    if t.condition
        .as_ref()
        .is_some_and(|c| c.name == "$expression")
    {
        return Err(VErr::Refuse("inline expression conditions".into()));
    }
    Ok(())
}

/// `ValidateTupleForRead` (and so `FilterInvalidTuples`).
pub fn validate_tuple_for_read(m: &Model, t: &Tuple) -> Result<(), VErr> {
    let object_type = get_type(&t.object);
    // validateTuplesetRestrictions
    match m.is_tupleset_relation(object_type, &t.relation) {
        Err(u) => return invalid(undefined(u, object_type, &t.relation)),
        Ok(true) => {
            let rel = m.get_relation(object_type, &t.relation).unwrap();
            if !matches!(rel.rewrite, Rewrite::This) {
                return invalid(format!(
                    "unexpected rewrite encountered with tupleset relation '{object_type}#{}'",
                    t.relation
                ));
            }
            if is_wildcard(&t.user) {
                return invalid(format!(
                    "unexpected wildcard relationship with tupleset relation '{object_type}#{}'",
                    t.relation
                ));
            }
            if !is_valid_object(&t.user) {
                return invalid(format!(
                    "unexpected user '{}' with tupleset relation '{object_type}#{}'",
                    t.user, t.relation
                ));
            }
        }
        Ok(false) => {}
    }
    // HasTypeInfo: always true for schema 1.1+.
    let restrictions = match m.get_relation(object_type, &t.relation) {
        Ok(r) => &r.directly_related,
        Err(u) => return invalid(undefined(u, object_type, &t.relation)),
    };
    validate_type_restrictions(restrictions, t, object_type)?;
    validate_condition(m, restrictions, t)
}

fn validate_type_restrictions(
    restrictions: &[RelRef],
    t: &Tuple,
    object_type: &str,
) -> Result<(), VErr> {
    let (user_object, user_rel) = split_object_relation(&t.user);
    let user_type = split_object(user_object).0;
    let user = &t.user;
    if is_object_relation(user) {
        if restrictions
            .iter()
            .any(|r| r.ty == user_type && r.relation() == user_rel)
        {
            return Ok(());
        }
        return invalid(format!(
            "'{user_type}#{user_rel}' is not an allowed type restriction for '{object_type}#{}'",
            t.relation
        ));
    }
    if is_typed_wildcard(user) {
        if restrictions
            .iter()
            .any(|r| r.ty == user_type && r.is_wildcard())
        {
            return Ok(());
        }
        return invalid(format!(
            "the typed wildcard '{user}' is not an allowed type restriction for '{object_type}#{}'",
            t.relation
        ));
    }
    if restrictions
        .iter()
        .any(|r| r.ty == user_type && !r.is_wildcard() && r.relation().is_empty())
    {
        return Ok(());
    }
    invalid(format!(
        "type '{user_type}' is not an allowed type restriction for '{object_type}#{}'",
        t.relation
    ))
}

fn facet_matches(r: &RelRef, user: &str, user_relation: &str) -> bool {
    if r.kind != RefKind::Unset {
        if !r.relation().is_empty() && r.relation() != user_relation {
            return false;
        }
        if r.is_wildcard() && !is_typed_wildcard(user) {
            return false;
        }
        return true;
    }
    !is_typed_wildcard(user) && user_relation.is_empty()
}

fn validate_condition(m: &Model, restrictions: &[RelRef], t: &Tuple) -> Result<(), VErr> {
    let user_type = get_type(&t.user);
    let user_relation = get_relation(&t.user);
    let Some(cond) = &t.condition else {
        if restrictions.iter().any(|r| {
            r.condition.is_empty() && r.ty == user_type && facet_matches(r, &t.user, user_relation)
        }) {
            return Ok(());
        }
        return invalid("condition is missing");
    };
    if contains_forbidden_chars(&cond.name) {
        return invalid("condition name contains forbidden characters");
    }
    if cond.name == "$expression" {
        return Err(VErr::Refuse("inline expression conditions".into()));
    }
    let Some(named) = m.conditions.get(&cond.name) else {
        return invalid("undefined condition");
    };
    if !restrictions.iter().any(|r| {
        r.ty == user_type && r.condition == cond.name && facet_matches(r, &t.user, user_relation)
    }) {
        return invalid("invalid condition for type restriction");
    }
    validate_struct(cond.context.as_ref()).map_err(VErr::Invalid)?;
    match named.validate_context(cond.context.as_ref()) {
        Ok(()) => Ok(()),
        Err(Bad::Error(e)) => invalid(e),
        Err(Bad::Refuse(r)) => Err(VErr::Refuse(r)),
    }
}

/// `ValidateStruct`: no control characters in keys or string values.
pub fn validate_struct(s: Option<&Map<String, Value>>) -> Result<(), String> {
    fn value(v: &Value) -> Result<(), String> {
        match v {
            Value::String(s) if contains_forbidden_chars(s) => {
                Err(format!("context value {s:?} contains forbidden characters"))
            }
            Value::Array(a) => a.iter().try_for_each(value),
            Value::Object(o) => validate_struct(Some(o)),
            _ => Ok(()),
        }
    }
    for (k, v) in s.into_iter().flatten() {
        if contains_forbidden_chars(k) {
            return Err(format!("context key {k:?} contains forbidden characters"));
        }
        value(v)?;
    }
    Ok(())
}
