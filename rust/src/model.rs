//! Authorization models: protojson decoding (proto field names) and the
//! parts of OpenFGA's typesystem that Check consults.

use crate::cel::Condition;
use crate::strings::{split_object, split_object_relation};
use serde::Deserialize;
use serde_json::Value;
use std::collections::{BTreeMap, HashMap, HashSet, VecDeque};

#[derive(Deserialize)]
struct ModelJson {
    #[serde(default)]
    schema_version: String,
    #[serde(default)]
    type_definitions: Vec<TypeDefJson>,
    #[serde(default)]
    conditions: BTreeMap<String, ConditionJson>,
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

#[derive(Deserialize)]
struct MetadataJson {
    #[serde(default)]
    relations: BTreeMap<String, RelationMetadataJson>,
}

#[derive(Deserialize)]
struct RelationMetadataJson {
    #[serde(default)]
    directly_related_user_types: Vec<RelRefJson>,
}

#[derive(Deserialize)]
pub(crate) struct RelRefJson {
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
struct UsersetJson {
    #[serde(default)]
    this: Option<Value>,
    #[serde(default)]
    computed_userset: Option<ObjectRelationJson>,
    #[serde(default)]
    tuple_to_userset: Option<TupleToUsersetJson>,
    #[serde(default)]
    union: Option<UsersetsJson>,
    #[serde(default)]
    intersection: Option<UsersetsJson>,
    #[serde(default)]
    difference: Option<DifferenceJson>,
}

#[derive(Deserialize, Default)]
struct ObjectRelationJson {
    #[serde(default)]
    relation: String,
}

#[derive(Deserialize)]
struct TupleToUsersetJson {
    #[serde(default)]
    tupleset: ObjectRelationJson,
    #[serde(default)]
    computed_userset: ObjectRelationJson,
}

#[derive(Deserialize)]
struct UsersetsJson {
    #[serde(default)]
    child: Vec<UsersetJson>,
}

#[derive(Deserialize)]
struct DifferenceJson {
    base: Box<UsersetJson>,
    subtract: Box<UsersetJson>,
}

#[derive(Deserialize)]
pub(crate) struct ConditionJson {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub expression: String,
    #[serde(default)]
    pub parameters: BTreeMap<String, ParamTypeJson>,
}

#[derive(Deserialize, Clone)]
pub(crate) struct ParamTypeJson {
    #[serde(default)]
    pub type_name: Value,
    #[serde(default)]
    pub generic_types: Vec<ParamTypeJson>,
}

#[derive(Debug)]
pub enum Rewrite {
    This,
    Computed(String),
    Ttu { tupleset: String, computed: String },
    Union(Vec<Rewrite>),
    Intersection(Vec<Rewrite>),
    Difference(Box<Rewrite>, Box<Rewrite>),
}

/// The `relation_or_wildcard` oneof of a RelationReference.
#[derive(Clone, Debug, PartialEq)]
pub enum RefKind {
    Unset,
    /// Possibly empty: protojson keeps a set oneof member.
    Relation(String),
    Wildcard,
}

#[derive(Clone, Debug)]
pub struct RelRef {
    pub ty: String,
    pub kind: RefKind,
    pub condition: String,
}

impl RelRef {
    fn from_json(r: &RelRefJson) -> Result<Self, String> {
        let kind = match (&r.relation, &r.wildcard) {
            (Some(_), Some(_)) => {
                return Err("relation reference sets both relation and wildcard".into())
            }
            (Some(rel), None) => RefKind::Relation(rel.clone()),
            (None, Some(_)) => RefKind::Wildcard,
            (None, None) => RefKind::Unset,
        };
        Ok(RelRef {
            ty: r.ty.clone(),
            kind,
            condition: r.condition.clone(),
        })
    }

    /// `GetRelation()`.
    pub fn relation(&self) -> &str {
        match &self.kind {
            RefKind::Relation(r) => r,
            _ => "",
        }
    }

    pub fn is_wildcard(&self) -> bool {
        self.kind == RefKind::Wildcard
    }

    /// `typesystem.RelationEquals(self, other)`.
    pub fn relation_equals(&self, other: &RelRef) -> bool {
        if self.ty != other.ty {
            return false;
        }
        if self.kind == RefKind::Unset && other.kind == RefKind::Unset {
            return true;
        }
        if self.is_wildcard() && other.is_wildcard() {
            return true;
        }
        !self.relation().is_empty()
            && !other.relation().is_empty()
            && self.relation() == other.relation()
    }
}

pub struct Relation {
    pub rewrite: Rewrite,
    pub directly_related: Vec<RelRef>,
}

pub struct TypeDef {
    pub relations: HashMap<String, Relation>,
    /// Tupleset relations of every tuple-to-userset rewrite in the type.
    tuplesets: HashSet<String>,
}

pub struct Model {
    pub types: HashMap<String, TypeDef>,
    /// Nodes OpenFGA's planner may resolve with its recursive or weight-2
    /// strategies instead of the default one (see [`Model::alternatives`]).
    alternatives: HashSet<String>,
    pub conditions: HashMap<String, Condition>,
    graph: Graph,
}

/// `typesystem.GetRelation` failures.
#[derive(Debug, PartialEq)]
pub enum Undefined {
    Type,
    Relation,
}

fn rewrite_from_json(u: &UsersetJson) -> Result<Rewrite, String> {
    let set = [
        u.this.is_some(),
        u.computed_userset.is_some(),
        u.tuple_to_userset.is_some(),
        u.union.is_some(),
        u.intersection.is_some(),
        u.difference.is_some(),
    ];
    if set.iter().filter(|b| **b).count() != 1 {
        return Err("userset without exactly one rewrite".into());
    }
    Ok(if u.this.is_some() {
        Rewrite::This
    } else if let Some(c) = &u.computed_userset {
        Rewrite::Computed(c.relation.clone())
    } else if let Some(t) = &u.tuple_to_userset {
        Rewrite::Ttu {
            tupleset: t.tupleset.relation.clone(),
            computed: t.computed_userset.relation.clone(),
        }
    } else if let Some(c) = &u.union {
        Rewrite::Union(
            c.child
                .iter()
                .map(rewrite_from_json)
                .collect::<Result<_, _>>()?,
        )
    } else if let Some(c) = &u.intersection {
        Rewrite::Intersection(
            c.child
                .iter()
                .map(rewrite_from_json)
                .collect::<Result<_, _>>()?,
        )
    } else {
        let d = u.difference.as_ref().unwrap();
        Rewrite::Difference(
            Box::new(rewrite_from_json(&d.base)?),
            Box::new(rewrite_from_json(&d.subtract)?),
        )
    })
}

fn collect_tuplesets(r: &Rewrite, out: &mut HashSet<String>) {
    match r {
        Rewrite::Ttu { tupleset, .. } => {
            out.insert(tupleset.clone());
        }
        Rewrite::Union(c) | Rewrite::Intersection(c) => {
            c.iter().for_each(|c| collect_tuplesets(c, out))
        }
        Rewrite::Difference(a, b) => {
            collect_tuplesets(a, out);
            collect_tuplesets(b, out);
        }
        _ => {}
    }
}

impl Model {
    /// Decodes a model. An error is a refusal reason: the model uses
    /// something this port does not implement.
    pub fn from_json(json: &[u8], alternatives: HashSet<String>) -> Result<Model, String> {
        let m: ModelJson = serde_json::from_slice(json).map_err(|e| format!("model JSON: {e}"))?;
        // typesystem.IsSchemaVersionSupported
        if m.schema_version != "1.1" && m.schema_version != "1.2" {
            return Err(format!(
                "schema version {:?} is not supported",
                m.schema_version
            ));
        }
        let mut types = HashMap::new();
        for td in &m.type_definitions {
            if types.contains_key(&td.ty) {
                return Err(format!("duplicate type {:?}", td.ty));
            }
            let mut relations = HashMap::new();
            let mut tuplesets = HashSet::new();
            for (name, rw) in &td.relations {
                let rewrite = rewrite_from_json(rw)?;
                collect_tuplesets(&rewrite, &mut tuplesets);
                let directly_related =
                    match td.metadata.as_ref().and_then(|md| md.relations.get(name)) {
                        Some(md) => md
                            .directly_related_user_types
                            .iter()
                            .map(RelRef::from_json)
                            .collect::<Result<_, _>>()?,
                        None => vec![],
                    };
                relations.insert(
                    name.clone(),
                    Relation {
                        rewrite,
                        directly_related,
                    },
                );
            }
            types.insert(
                td.ty.clone(),
                TypeDef {
                    relations,
                    tuplesets,
                },
            );
        }
        let mut conditions = HashMap::new();
        for (key, c) in &m.conditions {
            conditions.insert(key.clone(), Condition::new(c));
        }
        let graph = Graph::build(&m.type_definitions, &types)?;
        Ok(Model {
            types,
            conditions,
            graph,
            alternatives,
        })
    }

    /// Whether OpenFGA may resolve a tuple-to-userset node with another
    /// strategy. Keys are computed by the host with OpenFGA's own
    /// `TTUUseWeight2Resolver`/`TTUUseRecursiveResolver`.
    pub fn ttu_alternative(
        &self,
        object_type: &str,
        relation: &str,
        tupleset: &str,
        computed: &str,
        user_type: &str,
    ) -> bool {
        self.alternatives.contains(&format!(
            "ttu:{object_type}#{relation}:{tupleset}:{computed}@{user_type}"
        ))
    }

    /// The same for directly assigned usersets (`UsersetUseRecursiveResolver`
    /// or `UsersetUseWeight2Resolver` for any of them).
    pub fn userset_alternative(&self, object_type: &str, relation: &str, user_type: &str) -> bool {
        self.alternatives
            .contains(&format!("userset:{object_type}#{relation}@{user_type}"))
    }

    /// Whether any relation the resolution of `object_type#relation` can
    /// reach admits conditional tuples.
    pub fn conditional_below(&self, object_type: &str, relation: &str) -> bool {
        let mut seen = HashSet::new();
        let mut stack = vec![(object_type.to_string(), relation.to_string())];
        while let Some((t, r)) = stack.pop() {
            if !seen.insert((t.clone(), r.clone())) {
                continue;
            }
            let Ok(rel) = self.get_relation(&t, &r) else {
                continue;
            };
            for rr in &rel.directly_related {
                if !rr.condition.is_empty() {
                    return true;
                }
                if !rr.relation().is_empty() {
                    stack.push((rr.ty.clone(), rr.relation().to_string()));
                }
            }
            let mut rewrites = vec![&rel.rewrite];
            while let Some(rw) = rewrites.pop() {
                match rw {
                    Rewrite::This => {}
                    Rewrite::Computed(c) => stack.push((t.clone(), c.clone())),
                    Rewrite::Ttu { tupleset, computed } => {
                        stack.push((t.clone(), tupleset.clone()));
                        if let Ok(ts) = self.get_relation(&t, tupleset) {
                            for target in &ts.directly_related {
                                stack.push((target.ty.clone(), computed.clone()));
                            }
                        }
                    }
                    Rewrite::Union(c) | Rewrite::Intersection(c) => rewrites.extend(c),
                    Rewrite::Difference(a, b) => rewrites.extend([&**a, &**b]),
                }
            }
        }
        false
    }

    pub fn get_relation(&self, object_type: &str, relation: &str) -> Result<&Relation, Undefined> {
        let td = self.types.get(object_type).ok_or(Undefined::Type)?;
        td.relations.get(relation).ok_or(Undefined::Relation)
    }

    pub fn has_type(&self, t: &str) -> bool {
        self.types.contains_key(t)
    }

    /// `IsDirectlyRelated(target, source)`.
    pub fn is_directly_related(
        &self,
        object_type: &str,
        relation: &str,
        source: &RelRef,
    ) -> Result<bool, Undefined> {
        let r = self.get_relation(object_type, relation)?;
        Ok(r.directly_related
            .iter()
            .any(|tr| source.relation_equals(tr)))
    }

    /// `IsPubliclyAssignable(target, userType)`.
    pub fn is_publicly_assignable(
        &self,
        object_type: &str,
        relation: &str,
        user_type: &str,
    ) -> Result<bool, Undefined> {
        let r = self.get_relation(object_type, relation)?;
        Ok(r.directly_related
            .iter()
            .any(|tr| tr.ty == user_type && tr.is_wildcard()))
    }

    /// `DirectlyRelatedUsersets`.
    pub fn directly_related_usersets(&self, object_type: &str, relation: &str) -> Vec<RelRef> {
        match self.get_relation(object_type, relation) {
            Ok(r) => r
                .directly_related
                .iter()
                .filter(|r| !r.relation().is_empty())
                .cloned()
                .collect(),
            Err(_) => vec![],
        }
    }

    pub fn is_tupleset_relation(
        &self,
        object_type: &str,
        relation: &str,
    ) -> Result<bool, Undefined> {
        self.get_relation(object_type, relation)?;
        Ok(self.types[object_type].tuplesets.contains(relation))
    }

    /// `typesystem.PathExists(user, relation, objectType)`. `Err` is the
    /// graph's "node not found".
    pub fn path_exists(
        &self,
        user: &str,
        relation: &str,
        object_type: &str,
    ) -> Result<bool, String> {
        let (user_object, user_relation) = split_object_relation(user);
        let user_type = split_object(user_object).0;
        let is_userset = !user_relation.is_empty();
        let from = if is_userset {
            format!("{user_type}#{user_relation}")
        } else {
            user_type.to_string()
        };
        let to = format!("{object_type}#{relation}");
        if self.graph.path(&from, &to)? {
            return Ok(true);
        }
        if is_userset {
            return Ok(false);
        }
        Ok(self
            .graph
            .path(&format!("{user_type}:*"), &to)
            .unwrap_or(false))
    }
}

/// The language package's AuthorizationModelGraph, reduced to reachability
/// (operator nodes are kept: they only relay edges).
struct Graph {
    ids: HashMap<String, usize>,
    edges: Vec<Vec<usize>>,
}

impl Graph {
    fn node(&mut self, label: &str) -> usize {
        if let Some(&id) = self.ids.get(label) {
            return id;
        }
        let id = self.edges.len();
        self.edges.push(vec![]);
        self.ids.insert(label.to_string(), id);
        id
    }

    fn anonymous(&mut self) -> usize {
        self.edges.push(vec![]);
        self.edges.len() - 1
    }

    fn edge(&mut self, from: usize, to: usize) {
        if !self.edges[from].contains(&to) {
            self.edges[from].push(to);
        }
    }

    fn build(tds: &[TypeDefJson], types: &HashMap<String, TypeDef>) -> Result<Graph, String> {
        let mut g = Graph {
            ids: HashMap::new(),
            edges: vec![],
        };
        let mut sorted: Vec<&TypeDefJson> = tds.iter().collect();
        sorted.sort_by(|a, b| a.ty.cmp(&b.ty));
        for td in sorted {
            g.node(&td.ty);
            let metadata = |rel: &str| -> Vec<RelRef> {
                td.metadata
                    .as_ref()
                    .and_then(|m| m.relations.get(rel))
                    .map(|m| {
                        m.directly_related_user_types
                            .iter()
                            .filter_map(|r| RelRef::from_json(r).ok())
                            .collect()
                    })
                    .unwrap_or_default()
            };
            for (name, rw) in &td.relations {
                let parent = g.node(&format!("{}#{}", td.ty, name));
                let rewrite = rewrite_from_json(rw)?;
                g.rewrite(parent, &rewrite, &td.ty, name, &metadata, types);
            }
        }
        Ok(g)
    }

    fn rewrite(
        &mut self,
        parent: usize,
        rw: &Rewrite,
        ty: &str,
        relation: &str,
        metadata: &dyn Fn(&str) -> Vec<RelRef>,
        types: &HashMap<String, TypeDef>,
    ) {
        match rw {
            Rewrite::This => {
                for r in metadata(relation) {
                    // classifyRelationReference skips malformed types.
                    if r.ty.is_empty() || r.ty.contains(':') || r.ty.contains('#') {
                        continue;
                    }
                    let label = match &r.kind {
                        RefKind::Wildcard => format!("{}:*", r.ty),
                        RefKind::Relation(rel) if !rel.is_empty() => format!("{}#{}", r.ty, rel),
                        _ => r.ty.clone(),
                    };
                    let n = self.node(&label);
                    self.edge(n, parent);
                }
            }
            Rewrite::Computed(rel) => {
                let n = self.node(&format!("{ty}#{rel}"));
                self.edge(n, parent);
            }
            Rewrite::Ttu { tupleset, computed } => {
                for r in metadata(tupleset) {
                    let exists = types
                        .get(&r.ty)
                        .is_some_and(|t| t.relations.contains_key(computed));
                    if !exists {
                        continue;
                    }
                    let n = self.node(&format!("{}#{}", r.ty, computed));
                    self.edge(n, parent);
                }
            }
            Rewrite::Union(c) | Rewrite::Intersection(c) => {
                let op = self.anonymous();
                self.edge(op, parent);
                for c in c {
                    self.rewrite(op, c, ty, relation, metadata, types);
                }
            }
            Rewrite::Difference(a, b) => {
                let op = self.anonymous();
                self.edge(op, parent);
                self.rewrite(op, a, ty, relation, metadata, types);
                self.rewrite(op, b, ty, relation, metadata, types);
            }
        }
    }

    fn path(&self, from: &str, to: &str) -> Result<bool, String> {
        let from = *self
            .ids
            .get(from)
            .ok_or_else(|| format!("node {from:?} not found"))?;
        let to = *self
            .ids
            .get(to)
            .ok_or_else(|| format!("node {to:?} not found"))?;
        let mut seen = vec![false; self.edges.len()];
        let mut queue = VecDeque::from([from]);
        seen[from] = true;
        while let Some(n) = queue.pop_front() {
            if n == to {
                return Ok(true);
            }
            for &m in &self.edges[n] {
                if !seen[m] {
                    seen[m] = true;
                    queue.push_back(m);
                }
            }
        }
        Ok(false)
    }
}
