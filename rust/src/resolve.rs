//! OpenFGA's LocalChecker (internal/graph/check.go and default_resolver.go),
//! resolving into sets of possible outcomes (see [`crate::outcome`]).

use crate::cel::Bad;
use crate::exec::{join_all, join_union, read, BoxFut, IoRef, ReadReq};
use crate::model::{Model, RefKind, RelRef, Rewrite, Undefined};
use crate::outcome::{exclusion, intersection, Out};
use crate::strings::*;
use crate::validate::{validate_tuple_for_read, VErr};
use crate::{ApiError, Tuple};
use serde_json::{Map, Value};
use std::rc::Rc;
use std::sync::Arc;

/// serverconfig.DefaultResolveNodeLimit
const MAX_DEPTH: u32 = 25;

pub struct Ctx {
    pub model: Arc<Model>,
    pub io: IoRef,
    /// Ordered by object, as CombinedTupleReader keeps them.
    pub contextual: Vec<Tuple>,
    pub request_context: Option<Map<String, Value>>,
}

type Cx = Rc<Ctx>;

/// The path of tuple keys from the root request (VisitedPaths).
pub enum Visited {
    Nil,
    Cons(String, Rc<Visited>),
}

impl Visited {
    fn contains(&self, key: &str) -> bool {
        let mut v = self;
        while let Visited::Cons(k, next) = v {
            if k == key {
                return true;
            }
            v = next;
        }
        false
    }
}

#[derive(Clone)]
pub struct Req {
    pub object: String,
    pub relation: String,
    pub user: String,
    pub depth: u32,
    pub visited: Rc<Visited>,
}

fn ready<'a>(o: Out) -> BoxFut<'a, Out> {
    Box::pin(std::future::ready(o))
}

/// `LocalChecker.ResolveCheck`.
pub fn resolve_check(cx: Cx, req: Req) -> BoxFut<'static, Out> {
    Box::pin(async move {
        if req.depth == MAX_DEPTH {
            return Out::e(ApiError::TooComplex);
        }
        let key = tuple_key_string(&req.object, &req.relation, &req.user);
        if req.visited.contains(&key) {
            return Out::c();
        }
        let req = Req {
            visited: Rc::new(Visited::Cons(key, req.visited.clone())),
            ..req
        };
        if is_self_defining(&req.object, &req.relation, &req.user) {
            return Out::t();
        }
        let depth = req.depth;
        let model = cx.model.clone();
        let object_type = get_type(&req.object);
        let Ok(rel) = model.get_relation(object_type, &req.relation) else {
            return Out::e(ApiError::Internal(format!(
                "relation '{}' undefined for object type '{object_type}'",
                req.relation
            )));
        };
        match model.path_exists(&req.user, &req.relation, object_type) {
            Err(e) => Out::e(ApiError::Internal(e)),
            Ok(false) => Out::f(),
            Ok(true) => {
                let mut out = check_rewrite(cx, req.clone(), &rel.rewrite).await;
                out.depth = out.depth.max(depth);
                out
            }
        }
    })
}

/// OpenFGA's planner may resolve a node with its recursive or weight-2
/// strategy instead of the default one this port implements. Those agree
/// with the default on reachability, but differ at the edges: they never
/// report a cycle (plain false instead), they search one level deeper, and
/// they read tuples from the user's side, failing on a condition error there
/// that the default never encounters. So the default's outcome stands only
/// where none of that can matter.
fn widen(m: &Model, object_type: &str, relation: &str, mut out: Out) -> Out {
    if m.conditional_below(object_type, relation) {
        return Out::refused("OpenFGA may resolve this with another strategy, which reads further conditional tuples");
    }
    if out.has(crate::outcome::E) || out.depth + 3 >= MAX_DEPTH {
        return Out::refused(
            "OpenFGA may resolve this with another strategy, with other depth limits",
        );
    }
    if out.has(crate::outcome::C) {
        out.bits |= crate::outcome::F;
    }
    out
}

fn dispatch(cx: &Cx, parent: &Req, object: &str, relation: &str) -> BoxFut<'static, Out> {
    resolve_check(
        cx.clone(),
        Req {
            object: object.into(),
            relation: relation.into(),
            user: parent.user.clone(),
            depth: parent.depth + 1,
            visited: parent.visited.clone(),
        },
    )
}

fn check_rewrite<'a>(cx: Cx, req: Req, rw: &'a Rewrite) -> BoxFut<'a, Out> {
    match rw {
        Rewrite::This => check_direct(cx, req),
        Rewrite::Computed(r) => resolve_check(
            cx,
            Req {
                relation: r.clone(),
                ..req
            },
        ),
        Rewrite::Ttu { tupleset, computed } => check_ttu(cx, req, tupleset, computed),
        Rewrite::Union(children) => Box::pin(join_union(
            children
                .iter()
                .map(|c| check_rewrite(cx.clone(), req.clone(), c))
                .collect(),
        )),
        Rewrite::Intersection(children) if children.len() < 2 => {
            ready(Out::e(ApiError::Internal(format!(
                "expected at least two rewrite operands for intersection operator, but got '{}'",
                children.len()
            ))))
        }
        Rewrite::Intersection(children) => {
            let fs = children
                .iter()
                .map(|c| check_rewrite(cx.clone(), req.clone(), c))
                .collect();
            Box::pin(async move { intersection(&join_all(fs).await) })
        }
        Rewrite::Difference(base, sub) => {
            let fs = vec![
                check_rewrite(cx.clone(), req.clone(), base),
                check_rewrite(cx, req, sub),
            ];
            Box::pin(async move {
                let v = join_all(fs).await;
                exclusion(&v[0], &v[1])
            })
        }
    }
}

/// `checkDirect`.
fn check_direct(cx: Cx, req: Req) -> BoxFut<'static, Out> {
    let m = &cx.model;
    let object_type = get_type(&req.object);
    let user_type = get_type(&req.user).to_string();
    let user_relation = get_relation(&req.user);
    let source = RelRef {
        ty: user_type.clone(),
        kind: if user_relation.is_empty() {
            RefKind::Unset
        } else {
            RefKind::Relation(user_relation.into())
        },
        condition: String::new(),
    };
    let mut handlers = vec![];
    if m.is_directly_related(object_type, &req.relation, &source)
        .unwrap_or(false)
    {
        handlers.push(check_direct_user_tuple(cx.clone(), req.clone()));
    }
    if !is_object_relation(&req.user)
        && m.is_publicly_assignable(object_type, &req.relation, &user_type)
            .unwrap_or(false)
    {
        handlers.push(check_public_assignable(cx.clone(), req.clone(), user_type));
    }
    let usersets = m.directly_related_usersets(object_type, &req.relation);
    if !usersets.is_empty() {
        handlers.push(check_direct_usersets(cx.clone(), req, usersets));
    }
    Box::pin(join_union(handlers))
}

/// `BuildTupleKeyConditionFilter`.
fn condition_met(cx: &Ctx, t: &Tuple) -> Result<bool, Bad> {
    let Some(c) = &t.condition else {
        return Ok(true);
    };
    if c.name == "$expression" {
        return Err(Bad::Refuse("inline expression conditions".into()));
    }
    if c.name.is_empty() {
        return Ok(true);
    }
    match cx.model.conditions.get(&c.name) {
        Some(cond) if cond.name == c.name => {
            cond.evaluate(cx.request_context.as_ref(), c.context.as_ref())
        }
        _ => Err(Bad::Error(format!(
            "failed to evaluate relationship condition '{}': condition was not found",
            c.name
        ))),
    }
}

fn condition_error(e: String) -> ApiError {
    ApiError::Validation(e)
}

fn read_error(e: String) -> Out {
    Out::e(ApiError::Internal(e))
}

/// `checkDirectUserTuple`.
fn check_direct_user_tuple(cx: Cx, req: Req) -> BoxFut<'static, Out> {
    Box::pin(async move {
        // CombinedTupleReader: the first matching contextual tuple, else the
        // datastore's.
        let found = cx
            .contextual
            .iter()
            .find(|t| t.object == req.object && t.relation == req.relation && t.user == req.user)
            .cloned();
        let t = match found {
            Some(t) => t,
            None => {
                let rr = ReadReq::User {
                    object: req.object.clone(),
                    relation: req.relation.clone(),
                    user: req.user.clone(),
                };
                match read(&cx.io, rr).await {
                    Err(e) => return read_error(e),
                    Ok(ts) => match ts.into_iter().next() {
                        None => return Out::f(),
                        Some(t) => t,
                    },
                }
            }
        };
        match validate_tuple_for_read(&cx.model, &t) {
            Err(VErr::Invalid(_)) => return Out::f(),
            Err(VErr::Refuse(r)) => return Out::refused(r),
            Ok(()) => {}
        }
        match condition_met(&cx, &t) {
            Ok(b) => Out::bool(b),
            Err(Bad::Error(e)) => Out::e(condition_error(e)),
            Err(Bad::Refuse(r)) => Out::refused(r),
        }
    })
}

/// What a FilteredTupleKeyIterator wrapped in a
/// ConditionsFilteredTupleKeyIterator yields.
struct Filtered {
    valid: Vec<Tuple>,
    /// Tuples this port cannot classify.
    unknown: Vec<String>,
    last_error: Option<String>,
}

impl Filtered {
    /// Outcomes that are not dispatches: unknown tuples, and the iterator's
    /// error at the end (reported only if no tuple passed the filter).
    fn residue(&self) -> Vec<Out> {
        let mut out: Vec<Out> = self
            .unknown
            .iter()
            .map(|r| Out::refused(r.clone()))
            .collect();
        if self.valid.is_empty() && self.unknown.is_empty() {
            if let Some(e) = &self.last_error {
                out.push(Out::e(condition_error(e.clone())));
            }
        }
        out
    }
}

fn filter(cx: &Ctx, tuples: Vec<Tuple>) -> Filtered {
    let mut f = Filtered {
        valid: vec![],
        unknown: vec![],
        last_error: None,
    };
    for t in tuples {
        match validate_tuple_for_read(&cx.model, &t) {
            Err(VErr::Invalid(_)) => continue,
            Err(VErr::Refuse(r)) => {
                f.unknown.push(r);
                continue;
            }
            Ok(()) => {}
        }
        match condition_met(cx, &t) {
            Ok(true) => f.valid.push(t),
            Ok(false) => {}
            Err(Bad::Error(e)) => f.last_error = Some(e),
            Err(Bad::Refuse(r)) => f.unknown.push(r),
        }
    }
    f
}

fn contextual_read(cx: &Ctx, object: &str, relation: &str) -> Vec<Tuple> {
    cx.contextual
        .iter()
        .filter(|t| t.object == object && t.relation == relation)
        .cloned()
        .collect()
}

/// `tupleMatchesAllowedUserTypeRestrictions`.
fn matches_restrictions(user: &str, allowed: &[RelRef]) -> bool {
    if !is_userset_user_type(user) {
        return false;
    }
    allowed.iter().any(|a| match &a.kind {
        RefKind::Wildcard => is_typed_wildcard(user) && get_type(user) == a.ty,
        RefKind::Relation(r) => {
            is_object_relation(user) && get_type(user) == a.ty && get_relation(user) == r
        }
        RefKind::Unset => false,
    })
}

async fn read_usersets(cx: &Ctx, req: &Req, allowed: Vec<RelRef>) -> Result<Vec<Tuple>, String> {
    let mut tuples: Vec<Tuple> = contextual_read(cx, &req.object, &req.relation)
        .into_iter()
        .filter(|t| matches_restrictions(&t.user, &allowed))
        .collect();
    let rr = ReadReq::Usersets {
        object: req.object.clone(),
        relation: req.relation.clone(),
        allowed,
    };
    tuples.extend(read(&cx.io, rr).await?);
    Ok(tuples)
}

/// `checkPublicAssignable`.
fn check_public_assignable(cx: Cx, req: Req, user_type: String) -> BoxFut<'static, Out> {
    Box::pin(async move {
        let allowed = vec![RelRef {
            ty: user_type,
            kind: RefKind::Wildcard,
            condition: String::new(),
        }];
        let tuples = match read_usersets(&cx, &req, allowed).await {
            Ok(t) => t,
            Err(e) => return read_error(e),
        };
        let f = filter(&cx, tuples);
        if !f.valid.is_empty() {
            return Out::t();
        }
        crate::outcome::union(&f.residue())
    })
}

/// `checkDirectUsersetTuples` with the default resolver (`defaultUserset`).
fn check_direct_usersets(cx: Cx, req: Req, usersets: Vec<RelRef>) -> BoxFut<'static, Out> {
    Box::pin(async move {
        let tuples = match read_usersets(&cx, &req, usersets).await {
            Ok(t) => t,
            Err(e) => return read_error(e),
        };
        let f = filter(&cx, tuples);
        let mut children = vec![];
        for t in &f.valid {
            let (object, relation) = split_object_relation(&t.user);
            if is_typed_wildcard(object) && get_type(&req.user) == get_type(object) {
                children.push(ready(Out::t()));
                break;
            }
            if !relation.is_empty() {
                children.push(dispatch(&cx, &req, object, relation));
            }
        }
        children.extend(f.residue().into_iter().map(ready));
        let out = join_union(children).await;
        let object_type = get_type(&req.object);
        if !is_object_relation(&req.user)
            && cx
                .model
                .userset_alternative(object_type, &req.relation, get_type(&req.user))
        {
            return widen(&cx.model, object_type, &req.relation, out);
        }
        out
    })
}

/// `checkTTU` with the default resolver (`defaultTTU`).
fn check_ttu<'a>(cx: Cx, req: Req, tupleset: &'a str, computed: &'a str) -> BoxFut<'a, Out> {
    Box::pin(async move {
        let mut tuples = contextual_read(&cx, &req.object, tupleset);
        let rr = ReadReq::Read {
            object: req.object.clone(),
            relation: tupleset.into(),
        };
        match read(&cx.io, rr).await {
            Ok(ts) => tuples.extend(ts),
            Err(e) => return read_error(e),
        }
        let f = filter(&cx, tuples);
        let mut children = vec![];
        for t in &f.valid {
            let object = split_object_relation(&t.user).0;
            if let Err(Undefined::Relation) = cx.model.get_relation(get_type(object), computed) {
                continue;
            }
            children.push(dispatch(&cx, &req, object, computed));
        }
        children.extend(f.residue().into_iter().map(ready));
        let out = join_union(children).await;
        let object_type = get_type(&req.object);
        if !is_object_relation(&req.user)
            && cx.model.ttu_alternative(
                object_type,
                &req.relation,
                tupleset,
                computed,
                get_type(&req.user),
            )
        {
            return widen(&cx.model, object_type, &req.relation, out);
        }
        out
    })
}
