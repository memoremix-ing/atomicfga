//! The model compiler: decides whether OpenFGA accepts a model when it is
//! written (`WriteAuthorizationModel`), from its JSON alone.
//!
//! Checks run in OpenFGA's order of error codes:
//!
//! 1. the request's protobuf validation rules (`invalid_argument`);
//! 2. the type count and model size limits (`exceeded_entity_limit`);
//! 3. `typesystem.NewAndValidate` (`invalid_authorization_model`): schema
//!    version, duplicate and reserved names, each relation's rewrite, type
//!    restrictions, entrypoints and computed-relation cycles (in sorted type
//!    and relation order), then conditions.
//!
//! Within one stage every failure has the same code, so the verdict does not
//! depend on which failure OpenFGA reports first. A condition expression
//! outside the CEL subset of [`crate::cel`] cannot be decided; the model is
//! then *undecided*, unless another check fails for certain.

use crate::cel::{compile_verdict, Verdict};
use crate::model::{ConditionJson, ParamTypeJson};
use serde::Deserialize;
use serde_json::Value;
use std::cell::RefCell;
use std::collections::{BTreeMap, HashMap, HashSet};
use std::rc::Rc;

/// serverconfig.DefaultMaxTypesPerAuthorizationModel
pub const MAX_TYPES: usize = 100;
/// serverconfig.DefaultMaxAuthorizationModelSizeInBytes (of the protobuf
/// encoding).
pub const MAX_SIZE: usize = 256 * 1024;

#[derive(Clone, Debug, PartialEq)]
pub enum Outcome {
    Valid,
    /// An OpenFGA error code name and a message.
    Invalid {
        code: &'static str,
        message: String,
    },
    Undecided(String),
}

const INVALID_ARGUMENT: &str = "invalid_argument";
const LIMIT: &str = "exceeded_entity_limit";
const INVALID_MODEL: &str = "invalid_authorization_model";

// --- JSON (protojson; proto field names, or lowerCamelCase) ---------------

#[derive(Deserialize)]
struct RequestJson {
    #[serde(default, alias = "schemaVersion")]
    schema_version: String,
    #[serde(default, alias = "typeDefinitions")]
    type_definitions: Vec<TypeDefJson>,
    #[serde(default)]
    conditions: BTreeMap<String, CondJson>,
}

#[derive(Deserialize)]
struct TypeDefJson {
    #[serde(rename = "type", default)]
    ty: String,
    #[serde(default)]
    relations: BTreeMap<String, UsersetJson>,
    #[serde(default)]
    metadata: Option<MetadataJson>,
}

#[derive(Deserialize, Default)]
struct SourceInfoJson {
    #[serde(default)]
    file: String,
}

#[derive(Deserialize)]
struct MetadataJson {
    #[serde(default)]
    relations: BTreeMap<String, RelationMetadataJson>,
    #[serde(default)]
    module: String,
    #[serde(default, alias = "sourceInfo")]
    source_info: Option<SourceInfoJson>,
}

#[derive(Deserialize)]
struct RelationMetadataJson {
    #[serde(default, alias = "directlyRelatedUserTypes")]
    directly_related_user_types: Vec<RelRefJson>,
    #[serde(default)]
    module: String,
    #[serde(default, alias = "sourceInfo")]
    source_info: Option<SourceInfoJson>,
}

#[derive(Deserialize, Clone)]
struct RelRefJson {
    #[serde(rename = "type", default)]
    ty: String,
    #[serde(default)]
    relation: Option<String>,
    #[serde(default)]
    wildcard: Option<Value>,
    #[serde(default)]
    condition: String,
}

#[derive(Deserialize, Default)]
struct ObjectRelationJson {
    #[serde(default)]
    object: String,
    #[serde(default)]
    relation: String,
}

#[derive(Deserialize, Default)]
struct UsersetJson {
    #[serde(default)]
    this: Option<Value>,
    #[serde(default, alias = "computedUserset")]
    computed_userset: Option<ObjectRelationJson>,
    #[serde(default, alias = "tupleToUserset")]
    tuple_to_userset: Option<TtuJson>,
    #[serde(default)]
    union: Option<UsersetsJson>,
    #[serde(default)]
    intersection: Option<UsersetsJson>,
    #[serde(default)]
    difference: Option<DifferenceJson>,
}

#[derive(Deserialize)]
struct TtuJson {
    #[serde(default)]
    tupleset: Option<ObjectRelationJson>,
    #[serde(default, alias = "computedUserset")]
    computed_userset: Option<ObjectRelationJson>,
}

#[derive(Deserialize)]
struct UsersetsJson {
    #[serde(default)]
    child: Vec<UsersetJson>,
}

#[derive(Deserialize)]
struct DifferenceJson {
    #[serde(default)]
    base: Option<Box<UsersetJson>>,
    #[serde(default)]
    subtract: Option<Box<UsersetJson>>,
}

#[derive(Deserialize)]
struct CondJson {
    #[serde(default)]
    name: String,
    #[serde(default)]
    expression: String,
    #[serde(default)]
    parameters: BTreeMap<String, ParamJson>,
    #[serde(default)]
    metadata: Option<CondMetadataJson>,
}

#[derive(Deserialize)]
struct CondMetadataJson {
    #[serde(default)]
    module: String,
    #[serde(default, alias = "sourceInfo")]
    source_info: Option<SourceInfoJson>,
}

#[derive(Deserialize, Clone)]
struct ParamJson {
    #[serde(default, alias = "typeName")]
    type_name: Value,
    #[serde(default, alias = "genericTypes")]
    generic_types: Vec<ParamJson>,
}

impl ParamJson {
    fn to_model(&self) -> ParamTypeJson {
        ParamTypeJson {
            type_name: self.type_name.clone(),
            generic_types: self.generic_types.iter().map(ParamJson::to_model).collect(),
        }
    }
}

// --- rewrites --------------------------------------------------------------

enum U {
    /// No rewrite set (a nil or empty Userset).
    Nil,
    This,
    Computed(String),
    Ttu {
        tupleset: String,
        computed: String,
    },
    Union(Vec<U>),
    Intersection(Vec<U>),
    Difference(Box<U>, Box<U>),
}

/// Converts a userset whose protobuf rules already passed.
fn rewrite(u: &UsersetJson) -> U {
    if u.this.is_some() {
        U::This
    } else if let Some(c) = &u.computed_userset {
        U::Computed(c.relation.clone())
    } else if let Some(t) = &u.tuple_to_userset {
        U::Ttu {
            tupleset: t
                .tupleset
                .as_ref()
                .map(|o| o.relation.clone())
                .unwrap_or_default(),
            computed: t
                .computed_userset
                .as_ref()
                .map(|o| o.relation.clone())
                .unwrap_or_default(),
        }
    } else if let Some(c) = &u.union {
        U::Union(c.child.iter().map(rewrite).collect())
    } else if let Some(c) = &u.intersection {
        U::Intersection(c.child.iter().map(rewrite).collect())
    } else if let Some(d) = &u.difference {
        let side =
            |s: &Option<Box<UsersetJson>>| Box::new(s.as_deref().map(rewrite).unwrap_or(U::Nil));
        U::Difference(side(&d.base), side(&d.subtract))
    } else {
        U::Nil
    }
}

// --- 1. protobuf validation --------------------------------------------------

/// `^[^:#@\s]{1,max}$` as RE2 reads it (\s is ASCII whitespace; length in
/// characters).
fn name_ok(s: &str, max: usize) -> bool {
    let n = s.chars().count();
    (1..=max).contains(&n)
        && !s
            .chars()
            .any(|c| matches!(c, ':' | '#' | '@' | '\t' | '\n' | '\x0c' | '\r' | ' '))
}

fn source_info_ok(s: &Option<SourceInfoJson>) -> bool {
    let Some(s) = s else { return true };
    if s.file.is_empty() {
        return true;
    }
    // ^[a-zA-Z0-9_\-\/]{1,100}\.fga$
    let Some(stem) = s.file.strip_suffix(".fga") else {
        return false;
    };
    (1..=100).contains(&stem.len())
        && stem
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'_' | b'-' | b'/'))
}

fn userset_proto_ok(u: &UsersetJson) -> Result<(), String> {
    let set = [
        u.this.is_some(),
        u.computed_userset.is_some(),
        u.tuple_to_userset.is_some(),
        u.union.is_some(),
        u.intersection.is_some(),
        u.difference.is_some(),
    ];
    if set.iter().filter(|b| **b).count() > 1 {
        return Err("userset sets more than one rewrite".into());
    }
    let object_relation = |o: &ObjectRelationJson| -> Result<(), String> {
        if o.object.len() > 256 || o.relation.len() > 50 {
            return Err("object or relation too long".into());
        }
        Ok(())
    };
    if let Some(c) = &u.computed_userset {
        object_relation(c)?;
    }
    if let Some(t) = &u.tuple_to_userset {
        let ts = t
            .tupleset
            .as_ref()
            .ok_or("TupleToUserset.Tupleset: value is required")?;
        object_relation(ts)?;
        let cu = t
            .computed_userset
            .as_ref()
            .ok_or("TupleToUserset.ComputedUserset: value is required")?;
        object_relation(cu)?;
    }
    for c in [&u.union, &u.intersection].into_iter().flatten() {
        c.child.iter().try_for_each(userset_proto_ok)?;
    }
    if let Some(d) = &u.difference {
        userset_proto_ok(
            d.base
                .as_deref()
                .ok_or("Difference.Base: value is required")?,
        )?;
        userset_proto_ok(
            d.subtract
                .as_deref()
                .ok_or("Difference.Subtract: value is required")?,
        )?;
    }
    Ok(())
}

fn param_proto_ok(p: &ParamJson) -> Result<(), String> {
    match &p.type_name {
        Value::Null => {}
        Value::String(s)
            if matches!(
                s.as_str(),
                "TYPE_NAME_UNSPECIFIED"
                    | "TYPE_NAME_ANY"
                    | "TYPE_NAME_BOOL"
                    | "TYPE_NAME_STRING"
                    | "TYPE_NAME_INT"
                    | "TYPE_NAME_UINT"
                    | "TYPE_NAME_DOUBLE"
                    | "TYPE_NAME_DURATION"
                    | "TYPE_NAME_TIMESTAMP"
                    | "TYPE_NAME_MAP"
                    | "TYPE_NAME_LIST"
                    | "TYPE_NAME_IPADDRESS"
            ) => {}
        other => return Err(format!("unknown parameter type {other}")),
    }
    if p.generic_types.len() > 5 {
        return Err("more than 5 generic types".into());
    }
    p.generic_types.iter().try_for_each(param_proto_ok)
}

fn proto_ok(r: &RequestJson) -> Result<(), String> {
    if r.type_definitions.is_empty() {
        return Err("TypeDefinitions: value must contain at least 1 item(s)".into());
    }
    for td in &r.type_definitions {
        if !name_ok(&td.ty, 254) {
            return Err(format!(
                "TypeDefinition.Type {:?} does not match the pattern",
                td.ty
            ));
        }
        for (name, u) in &td.relations {
            if !name_ok(name, 50) {
                return Err(format!("relation name {name:?} does not match the pattern"));
            }
            userset_proto_ok(u)?;
        }
        if let Some(md) = &td.metadata {
            if !md.module.is_empty() && !name_ok(&md.module, 50) || !source_info_ok(&md.source_info)
            {
                return Err("invalid type metadata".into());
            }
            for rm in md.relations.values() {
                if !rm.module.is_empty() && !name_ok(&rm.module, 50)
                    || !source_info_ok(&rm.source_info)
                {
                    return Err("invalid relation metadata".into());
                }
                for rr in &rm.directly_related_user_types {
                    if !name_ok(&rr.ty, 254)
                        || !rr.condition.is_empty() && !name_ok(&rr.condition, 50)
                        || rr
                            .relation
                            .as_ref()
                            .is_some_and(|r| !r.is_empty() && !name_ok(r, 50))
                    {
                        return Err(format!("invalid relation reference to {:?}", rr.ty));
                    }
                    if rr.relation.is_some() && rr.wildcard.is_some() {
                        return Err("relation reference sets both relation and wildcard".into());
                    }
                }
            }
        }
    }
    if !matches!(r.schema_version.as_str(), "1.0" | "1.1" | "1.2") {
        return Err("SchemaVersion: value must be in list [1.0 1.1 1.2]".into());
    }
    if r.conditions.len() > 25 {
        return Err("Conditions: value must contain no more than 25 pair(s)".into());
    }
    for (key, c) in &r.conditions {
        if !name_ok(key, 50) || !name_ok(&c.name, 50) {
            return Err(format!("condition name {key:?} does not match the pattern"));
        }
        if c.expression.len() > 512 {
            return Err("Condition.Expression: value length must be at most 512 bytes".into());
        }
        if c.parameters.len() > 25 {
            return Err("Condition.Parameters: value must contain no more than 25 pair(s)".into());
        }
        for (p, t) in &c.parameters {
            if !name_ok(p, 50) {
                return Err(format!("parameter name {p:?} does not match the pattern"));
            }
            param_proto_ok(t)?;
        }
        if let Some(md) = &c.metadata {
            if !md.module.is_empty() && !name_ok(&md.module, 50) || !source_info_ok(&md.source_info)
            {
                return Err("invalid condition metadata".into());
            }
        }
    }
    Ok(())
}

// --- 3. typesystem.NewAndValidate ------------------------------------------

struct Rel {
    rewrite: U,
    types: Vec<RelRefJson>,
}

struct Ts<'a> {
    relations: HashMap<&'a str, HashMap<&'a str, Rel>>,
    /// Tupleset relations of each type's tuple-to-userset rewrites.
    tuplesets: HashMap<&'a str, HashSet<String>>,
    conditions: &'a BTreeMap<String, CondJson>,
}

impl Ts<'_> {
    fn rel(&self, t: &str, r: &str) -> Option<&Rel> {
        self.relations.get(t)?.get(r)
    }
}

fn collect_tuplesets(u: &U, out: &mut HashSet<String>) {
    match u {
        U::Ttu { tupleset, .. } => {
            out.insert(tupleset.clone());
        }
        U::Union(c) | U::Intersection(c) => c.iter().for_each(|c| collect_tuplesets(c, out)),
        U::Difference(a, b) => {
            collect_tuplesets(a, out);
            collect_tuplesets(b, out);
        }
        _ => {}
    }
}

fn invalid<T>(m: impl Into<String>) -> Result<T, String> {
    Err(m.into())
}

/// `isUsersetRewriteValid`.
fn rewrite_valid(ts: &Ts, t: &str, r: &str, u: &U) -> Result<(), String> {
    match u {
        U::Nil => invalid(format!(
            "invalid relation '{t}#{r}': invalid userset rewrite"
        )),
        U::This => Ok(()),
        U::Computed(c) => {
            if c == r || ts.rel(t, c).is_none() {
                return invalid(format!("invalid computed userset '{c}' on '{t}#{r}'"));
            }
            Ok(())
        }
        U::Ttu { tupleset, computed } => {
            let Some(trel) = ts.rel(t, tupleset) else {
                return invalid(format!("'{t}#{tupleset}' undefined"));
            };
            if !matches!(trel.rewrite, U::This) {
                return invalid(format!("the '{t}#{tupleset}' relation is referenced in at least one tupleset and thus must be a direct relation"));
            }
            if trel
                .types
                .iter()
                .any(|rr| ts.rel(&rr.ty, computed).is_some())
            {
                return Ok(());
            }
            invalid(format!("{computed} does not appear as a relation in any of the directly related user types"))
        }
        U::Union(c) | U::Intersection(c) => c.iter().try_for_each(|c| rewrite_valid(ts, t, r, c)),
        U::Difference(a, b) => {
            rewrite_valid(ts, t, r, a)?;
            rewrite_valid(ts, t, r, b)
        }
    }
}

fn contains_this(u: &U) -> bool {
    match u {
        U::This => true,
        U::Union(c) | U::Intersection(c) => c.iter().any(contains_this),
        U::Difference(a, b) => contains_this(a) || contains_this(b),
        _ => false,
    }
}

/// `validateTypeRestrictions`.
fn type_restrictions_valid(ts: &Ts, t: &str, r: &str) -> Result<(), String> {
    let rel = ts.rel(t, r).unwrap();
    let assignable = contains_this(&rel.rewrite);
    if assignable && rel.types.is_empty() {
        return invalid(format!("the assignable relation '{r}' in object type '{t}' must contain at least one relation type"));
    }
    if !assignable && !rel.types.is_empty() {
        return invalid(format!("the non-assignable relation '{r}' in object type '{t}' should not contain a relation type"));
    }
    let is_tupleset = ts.tuplesets.get(t).is_some_and(|s| s.contains(r));
    for rr in &rel.types {
        if !ts.relations.contains_key(rr.ty.as_str()) {
            return invalid(format!("invalid type restriction '{}' on '{t}#{r}'", rr.ty));
        }
        if rr.relation.is_some() || rr.wildcard.is_some() {
            if is_tupleset {
                return invalid(format!(
                    "invalid type restriction '{}' on tupleset '{t}#{r}'",
                    rr.ty
                ));
            }
            match &rr.relation {
                None => {} // wildcard
                Some(rel) if !rel.is_empty() => {
                    if ts.rel(&rr.ty, rel).is_none() {
                        return invalid(format!(
                            "invalid type restriction '{}#{rel}' on '{t}#{r}'",
                            rr.ty
                        ));
                    }
                }
                Some(_) => {
                    return invalid(format!(
                        "empty relation reference to '{}' on '{t}#{r}'",
                        rr.ty
                    ))
                }
            }
        }
        if !rr.condition.is_empty()
            && rr.condition != "$expression"
            && !ts.conditions.contains_key(&rr.condition)
        {
            return invalid(format!(
                "condition '{}' is undefined for relation '{r}'",
                rr.condition
            ));
        }
    }
    Ok(())
}

/// `hasEntrypoints`' visited map: Go clones the outer map but shares the
/// inner ones, so a branch's marks can be seen by its siblings. Which models
/// are accepted depends on that sharing, so it is reproduced.
type Visited = HashMap<String, Rc<RefCell<HashMap<String, bool>>>>;

/// `hasEntrypoints`: (has entrypoints, loop).
fn has_entrypoints(
    ts: &Ts,
    t: &str,
    r: &str,
    u: &U,
    visited: &Visited,
) -> Result<(bool, bool), String> {
    let mut v = visited.clone();
    match v.get(t) {
        Some(inner) => {
            inner.borrow_mut().insert(r.to_string(), false);
        }
        None => {
            v.insert(
                t.to_string(),
                Rc::new(RefCell::new(HashMap::from([(r.to_string(), false)]))),
            );
        }
    }
    let Some(rel) = ts.rel(t, r) else {
        return invalid(format!("undefined type definition for '{t}#{r}'"));
    };
    let seen = |v: &Visited, t: &str, r: &str| v.get(t).and_then(|m| m.borrow().get(r).copied());
    match u {
        U::This => {
            for at in &rel.types {
                if at.relation.is_none() || at.wildcard.is_some() {
                    v[t].borrow_mut().insert(r.to_string(), true);
                    return Ok((true, false));
                }
                let ar = at.relation.as_deref().unwrap_or("");
                let Some(arel) = ts.rel(&at.ty, ar) else {
                    return invalid(format!("undefined type definition for '{}#{ar}'", at.ty));
                };
                if seen(&v, &at.ty, ar).is_some() {
                    continue;
                }
                if has_entrypoints(ts, &at.ty, ar, &arel.rewrite, &v)?.0 {
                    return Ok((true, false));
                }
            }
            Ok((false, false))
        }
        U::Computed(c) => {
            let Some(crel) = ts.rel(t, c) else {
                return invalid(format!("undefined type definition for '{t}#{c}'"));
            };
            if let Some(has) = seen(&v, t, c) {
                return Ok((has, true));
            }
            has_entrypoints(ts, t, c, &crel.rewrite, &v)
        }
        U::Ttu { tupleset, computed } => {
            let Some(trel) = ts.rel(t, tupleset) else {
                return invalid(format!("undefined type definition for '{t}#{tupleset}'"));
            };
            for at in &trel.types {
                if let Some(arel) = ts.rel(&at.ty, computed) {
                    if let Some(has) = seen(&v, &at.ty, computed) {
                        if has {
                            return Ok((true, false));
                        }
                        continue;
                    }
                    if has_entrypoints(ts, &at.ty, computed, &arel.rewrite, &v)?.0 {
                        return Ok((true, false));
                    }
                }
            }
            Ok((false, false))
        }
        U::Union(children) => {
            if children.len() < 2 {
                return invalid(format!("'{t}#{r}' as union has less than 2 children"));
            }
            let mut looped = false;
            for c in children {
                let (has, l) = has_entrypoints(ts, t, r, c, visited)?;
                if has {
                    return Ok((true, false));
                }
                looped |= l;
            }
            Ok((false, looped))
        }
        U::Intersection(children) => {
            if children.len() < 2 {
                return invalid(format!(
                    "'{t}#{r}' as intersection has less than 2 children"
                ));
            }
            for c in children {
                let (has, l) = has_entrypoints(ts, t, r, c, visited)?;
                if !has {
                    return Ok((false, l));
                }
            }
            Ok((true, false))
        }
        U::Difference(base, sub) => {
            let (has, l) = has_entrypoints(ts, t, r, base, visited)?;
            if !has {
                return Ok((false, l));
            }
            let (has, l) = has_entrypoints(ts, t, r, sub, visited)?;
            if !has {
                return Ok((false, l));
            }
            Ok((true, false))
        }
        U::Nil => invalid(format!("hasEntrypoints unknown rewrite for '{t}#{r}'")),
    }
}

/// `hasCycle` (through computed relations only). Go's `visited` map is
/// mutated in place and shared by sibling branches; so is this one.
fn has_cycle(
    ts: &Ts,
    t: &str,
    r: &str,
    u: &U,
    visited: &mut HashSet<String>,
) -> Result<bool, String> {
    visited.insert(format!("{t}#{r}"));
    let mut copy = visited.clone();
    let children: Vec<&U> = match u {
        U::This | U::Ttu { .. } | U::Nil => return Ok(false),
        U::Computed(c) => {
            if visited.contains(&format!("{t}#{c}")) {
                return Ok(true);
            }
            let Some(crel) = ts.rel(t, c) else {
                return invalid(format!("'{t}#{c}' undefined"));
            };
            return has_cycle(ts, t, c, &crel.rewrite, &mut copy);
        }
        U::Union(c) | U::Intersection(c) => c.iter().collect(),
        U::Difference(a, b) => vec![a, b],
    };
    for c in children {
        if has_cycle(ts, t, r, c, &mut copy)? {
            return Ok(true);
        }
    }
    Ok(false)
}

fn validate_relation(ts: &Ts, t: &str, r: &str) -> Result<(), String> {
    let rel = ts.rel(t, r).unwrap();
    rewrite_valid(ts, t, r, &rel.rewrite)?;
    type_restrictions_valid(ts, t, r)?;
    let (has, looped) = has_entrypoints(ts, t, r, &rel.rewrite, &Visited::new())?;
    if !has {
        let why = if looped {
            "potential loop"
        } else {
            "no entrypoints defined"
        };
        return invalid(format!(
            "the definition of relation '{r}' in object type '{t}' is invalid: {why}"
        ));
    }
    if has_cycle(ts, t, r, &rel.rewrite, &mut HashSet::new())? {
        return invalid(format!("the definition of relation '{r}' in object type '{t}' is invalid: an authorization model cannot contain a cycle"));
    }
    Ok(())
}

/// Everything `NewAndValidate` checks before conditions.
fn types_valid(req: &RequestJson) -> Result<(), String> {
    if !matches!(req.schema_version.as_str(), "1.1" | "1.2") {
        return invalid("invalid schema version");
    }
    let mut seen = HashSet::new();
    if !req
        .type_definitions
        .iter()
        .all(|td| seen.insert(td.ty.as_str()))
    {
        return invalid("an authorization model cannot contain duplicate types");
    }
    let mut ts = Ts {
        relations: HashMap::new(),
        tuplesets: HashMap::new(),
        conditions: &req.conditions,
    };
    for td in &req.type_definitions {
        if td.ty.is_empty() || td.ty == "self" || td.ty == "this" {
            return invalid(format!("the type name '{}' is reserved or empty", td.ty));
        }
        let mut rels = HashMap::new();
        let mut tuplesets = HashSet::new();
        for (name, u) in &td.relations {
            if name.is_empty() || name == "self" || name == "this" {
                return invalid(format!(
                    "the relation name '{name}' in type '{}' is reserved or empty",
                    td.ty
                ));
            }
            let rewrite = rewrite(u);
            collect_tuplesets(&rewrite, &mut tuplesets);
            let types = td
                .metadata
                .as_ref()
                .and_then(|m| m.relations.get(name))
                .map(|m| m.directly_related_user_types.clone())
                .unwrap_or_default();
            rels.insert(name.as_str(), Rel { rewrite, types });
        }
        ts.relations.insert(td.ty.as_str(), rels);
        ts.tuplesets.insert(td.ty.as_str(), tuplesets);
    }
    let mut types: Vec<&str> = ts.relations.keys().copied().collect();
    types.sort();
    for t in types {
        let mut rels: Vec<&str> = ts.relations[t].keys().copied().collect();
        rels.sort();
        for r in rels {
            validate_relation(&ts, t, r)?;
        }
    }
    Ok(())
}

/// Server settings that change which models are accepted.
#[derive(Clone, Copy, Debug, Default, serde::Deserialize)]
pub struct Options {
    /// OpenFGA's `inline_expressions` experimental flag: without it, a type
    /// restriction may not use the `$expression` condition.
    #[serde(default)]
    pub inline_expressions: bool,
}

/// Decides whether OpenFGA accepts a `WriteAuthorizationModelRequest` body
/// (or an `AuthorizationModel`) in protojson.
pub fn compile(json: &[u8], options: Options) -> Outcome {
    let req: RequestJson = match serde_json::from_slice(json) {
        Ok(r) => r,
        Err(e) => {
            return Outcome::Invalid {
                code: INVALID_ARGUMENT,
                message: format!("model JSON: {e}"),
            }
        }
    };
    if let Err(e) = proto_ok(&req) {
        return Outcome::Invalid {
            code: INVALID_ARGUMENT,
            message: e,
        };
    }
    let inline = req
        .type_definitions
        .iter()
        .filter_map(|td| td.metadata.as_ref())
        .any(|m| {
            m.relations.values().any(|rm| {
                rm.directly_related_user_types
                    .iter()
                    .any(|rr| rr.condition == "$expression")
            })
        });
    if inline && !options.inline_expressions {
        return Outcome::Invalid {
            code: "validation_error",
            message: "$expression requires the \"inline_expressions\" experimental feature flag"
                .into(),
        };
    }
    if req.type_definitions.len() > MAX_TYPES {
        return Outcome::Invalid {
            code: LIMIT,
            message: format!(
                "exceeded entity limit: type definitions in an authorization model: {MAX_TYPES}"
            ),
        };
    }
    // The protobuf encoding of a model is at most about twice its compact
    // JSON (tags and lengths against names and punctuation), plus its ID.
    let compact = serde_json::to_vec(&serde_json::from_slice::<Value>(json).unwrap_or_default())
        .map_or(usize::MAX, |v| v.len());
    let size_unknown = compact.saturating_mul(2).saturating_add(64) > MAX_SIZE;
    let semantic = types_valid(&req);
    let mut undecided = None;
    let semantic = semantic.and_then(|()| {
        for (key, c) in &req.conditions {
            if *key != c.name {
                return invalid(format!(
                    "condition key '{key}' does not match condition name '{}'",
                    c.name
                ));
            }
            let cj = ConditionJson {
                name: c.name.clone(),
                expression: c.expression.clone(),
                parameters: c
                    .parameters
                    .iter()
                    .map(|(k, v)| (k.clone(), v.to_model()))
                    .collect(),
            };
            match compile_verdict(&cj) {
                Verdict::Valid => {}
                Verdict::Invalid(e) => {
                    return invalid(format!("failed to compile condition '{key}': {e}"))
                }
                Verdict::Undecided(why) => undecided = Some(format!("condition '{key}': {why}")),
            }
        }
        Ok(())
    });
    if size_unknown {
        return Outcome::Undecided("model may exceed the size limit".into());
    }
    match (semantic, undecided) {
        (Err(e), _) => Outcome::Invalid {
            code: INVALID_MODEL,
            message: e,
        },
        (Ok(()), Some(why)) => Outcome::Undecided(why),
        (Ok(()), None) => Outcome::Valid,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn verdict(v: Value) -> Outcome {
        compile(&serde_json::to_vec(&v).unwrap(), Options::default())
    }

    fn code(o: &Outcome) -> &str {
        match o {
            Outcome::Valid => "valid",
            Outcome::Invalid { code, .. } => code,
            Outcome::Undecided(_) => "undecided",
        }
    }

    fn model(doc_relations: Value, doc_metadata: Value) -> Value {
        json!({
            "schema_version": "1.1",
            "type_definitions": [
                {"type": "user"},
                {"type": "doc", "relations": doc_relations, "metadata": {"relations": doc_metadata}},
            ],
        })
    }

    #[test]
    fn verdicts() {
        let ok = model(
            json!({"viewer": {"this": {}}}),
            json!({"viewer": {"directly_related_user_types": [{"type": "user"}]}}),
        );
        assert_eq!(verdict(ok), Outcome::Valid);
        // A direct relation without type restrictions.
        let no_types = model(json!({"viewer": {"this": {}}}), json!({}));
        assert_eq!(code(&verdict(no_types)), INVALID_MODEL);
        // A relation defined only through itself has no entrypoint.
        let self_only = model(
            json!({"a": {"computed_userset": {"relation": "b"}}, "b": {"computed_userset": {"relation": "a"}}}),
            json!({}),
        );
        assert_eq!(code(&verdict(self_only)), INVALID_MODEL);
        let bad_name = model(
            json!({"view er": {"this": {}}}),
            json!({"view er": {"directly_related_user_types": [{"type": "user"}]}}),
        );
        assert_eq!(code(&verdict(bad_name)), INVALID_ARGUMENT);
        let mut cond = model(
            json!({"viewer": {"this": {}}}),
            json!({"viewer": {"directly_related_user_types": [{"type": "user", "condition": "c"}]}}),
        );
        cond["conditions"] = json!({"c": {"name": "c", "expression": "x < 10", "parameters": {"x": {"type_name": "TYPE_NAME_INT"}}}});
        assert_eq!(verdict(cond.clone()), Outcome::Valid);
        cond["conditions"]["c"]["expression"] = json!("x + 1");
        assert_eq!(code(&verdict(cond.clone())), INVALID_MODEL, "not a bool");
        cond["conditions"]["c"]["expression"] = json!("x in [1, 2]");
        assert_eq!(code(&verdict(cond.clone())), "undecided");
        cond["conditions"]["c"]["parameters"]["x"] = json!({"type_name": "TYPE_NAME_LIST"});
        assert_eq!(
            code(&verdict(cond)),
            INVALID_MODEL,
            "list without its generic type"
        );
    }
}
