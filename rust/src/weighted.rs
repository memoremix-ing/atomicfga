//! The weighted authorization model graph of OpenFGA's language package
//! (graph/weighted_graph*.go), as far as Check's strategy selection uses it:
//! the builder, weight assignment with tuple-cycle resolution, and the
//! typesystem predicates that decide when OpenFGA's planner may resolve a
//! node with its recursive or weight-2 strategy.
//!
//! Weights, recursive-relation and tuple-cycle marks are ported statement
//! by statement. The userset weights and wildcard sets, which only
//! ListObjects consults, are not.
//!
//! Go visits graph nodes in map order. OpenFGA's resulting tables were found
//! identical over repeated builds (`TestWeightedGraphMatchesOpenFGA` compares
//! them with this port), and [`tests::order_insensitive`] checks that this
//! port gives the same table for every node order it tries.

use crate::model::{Model, RefKind, Rewrite};
use std::collections::{HashMap, HashSet};

const INFINITE: i64 = i32::MAX as i64;

#[derive(Clone, Copy, PartialEq, Debug)]
enum NodeType {
    SpecificType,
    SpecificTypeAndRelation,
    Operator,
    SpecificTypeWildcard,
    LogicalDirectGrouping,
    LogicalTtuGrouping,
}

#[derive(Clone, Copy, PartialEq, Debug)]
enum EdgeType {
    Direct,
    Rewrite,
    Ttu,
    Computed,
    DirectLogical,
    TtuLogical,
}

type Weights = HashMap<String, i64>;

struct Node {
    label: String,
    unique: String,
    kind: NodeType,
    weights: Weights,
    recursive_relation: String,
    tuple_cycle: bool,
}

struct Edge {
    from: usize,
    to: usize,
    kind: EdgeType,
    tupleset_relation: String,
    weights: Weights,
    recursive_relation: String,
    tuple_cycle: bool,
}

#[derive(Debug)]
pub struct InvalidGraph(#[allow(dead_code)] String);

#[derive(Default)]
struct Graph {
    nodes: Vec<Node>,
    ids: HashMap<String, usize>,
    edges: Vec<Edge>,
    /// Outgoing edges by node, in insertion order.
    out: HashMap<usize, Vec<usize>>,
}

impl Graph {
    fn node(&mut self, unique: &str, label: &str, kind: NodeType) -> usize {
        if let Some(&id) = self.ids.get(unique) {
            return id;
        }
        self.nodes.push(Node {
            label: label.into(),
            unique: unique.into(),
            kind,
            weights: Weights::new(),
            recursive_relation: String::new(),
            tuple_cycle: false,
        });
        self.ids.insert(unique.into(), self.nodes.len() - 1);
        self.nodes.len() - 1
    }

    fn add_edge(&mut self, from: usize, to: usize, kind: EdgeType, tupleset_relation: &str) {
        self.edges.push(Edge {
            from,
            to,
            kind,
            tupleset_relation: tupleset_relation.into(),
            weights: Weights::new(),
            recursive_relation: String::new(),
            tuple_cycle: false,
        });
        let e = self.edges.len() - 1;
        self.out.entry(from).or_default().push(e);
    }

    fn has_edge(&self, from: usize, to: usize, kind: EdgeType, tupleset_relation: &str) -> bool {
        self.out.get(&from).is_some_and(|es| {
            es.iter().any(|&e| {
                self.edges[e].to == to
                    && self.edges[e].kind == kind
                    && self.edges[e].tupleset_relation == tupleset_relation
            })
        })
    }

    /// `UpsertEdge` (conditions are not tracked).
    fn upsert_edge(&mut self, from: usize, to: usize, kind: EdgeType, tupleset_relation: &str) {
        if !self.has_edge(from, to, kind, tupleset_relation) {
            self.add_edge(from, to, kind, tupleset_relation);
        }
    }

    fn edges_of(&self, n: usize) -> Vec<usize> {
        self.out.get(&n).cloned().unwrap_or_default()
    }

    fn is_logical_operator(&self, n: usize) -> bool {
        matches!(
            self.nodes[n].kind,
            NodeType::Operator | NodeType::LogicalTtuGrouping | NodeType::LogicalDirectGrouping
        )
    }

    fn is_logical_union_operator(&self, n: usize) -> bool {
        let node = &self.nodes[n];
        (node.kind == NodeType::Operator && node.label == "union")
            || matches!(
                node.kind,
                NodeType::LogicalTtuGrouping | NodeType::LogicalDirectGrouping
            )
    }
}

// --- builder (weighted_graph_builder.go) -------------------------------------

fn build(m: &Model) -> Result<Graph, InvalidGraph> {
    let mut g = Graph::default();
    let mut types: Vec<&String> = m.types.keys().collect();
    types.sort();
    for t in types {
        g.node(t, t, NodeType::SpecificType);
        let td = &m.types[t];
        let mut rels: Vec<&String> = td.relations.keys().collect();
        rels.sort();
        for r in rels {
            let unique = format!("{t}#{r}");
            let parent = g.node(&unique, &unique, NodeType::SpecificTypeAndRelation);
            let mut operator_index = 0;
            parse_rewrite(
                &mut g,
                parent,
                m,
                &td.relations[r].rewrite,
                t,
                r,
                &mut operator_index,
            )?;
        }
    }
    assign_weights(&mut g)?;
    Ok(g)
}

fn parse_rewrite(
    g: &mut Graph,
    parent: usize,
    m: &Model,
    rw: &Rewrite,
    t: &str,
    r: &str,
    index: &mut usize,
) -> Result<(), InvalidGraph> {
    let parent_name = format!("{t}#{r}");
    let (operator, children): (&str, Vec<&Rewrite>) = match rw {
        Rewrite::This => return parse_this(g, parent, m, t, r, &parent_name),
        Rewrite::Computed(c) => {
            let new = g.node(
                &format!("{t}#{c}"),
                &format!("{t}#{c}"),
                NodeType::SpecificTypeAndRelation,
            );
            let kind = if g.nodes[parent].kind == NodeType::SpecificTypeAndRelation
                && g.nodes[new].kind == NodeType::SpecificTypeAndRelation
            {
                EdgeType::Computed
            } else {
                EdgeType::Rewrite
            };
            g.add_edge(parent, new, kind, "");
            return Ok(());
        }
        Rewrite::Ttu { tupleset, computed } => {
            return parse_ttu(g, parent, m, t, tupleset, computed)
        }
        Rewrite::Union(c) => ("union", c.iter().collect()),
        Rewrite::Intersection(c) => ("intersection", c.iter().collect()),
        Rewrite::Difference(a, b) => ("exclusion", vec![&**a, &**b]),
    };
    let name = format!("{t}#{r}:{operator}:{index}");
    *index += 1;
    let op = g.node(&name, operator, NodeType::Operator);
    g.add_edge(parent, op, EdgeType::Rewrite, "");
    let _ = parent_name;
    for c in children {
        parse_rewrite(g, op, m, c, t, r, index)?;
    }
    Ok(())
}

fn parse_this(
    g: &mut Graph,
    parent: usize,
    m: &Model,
    t: &str,
    r: &str,
    _parent_name: &str,
) -> Result<(), InvalidGraph> {
    let refs = &m.types[t].relations[r].directly_related;
    let mut node = parent;
    if g.nodes[parent].kind != NodeType::SpecificTypeAndRelation && refs.len() > 1 {
        let unique = format!("{t}#direct:{r}");
        let logical = g.node(&unique, &unique, NodeType::LogicalDirectGrouping);
        g.add_edge(parent, logical, EdgeType::DirectLogical, "");
        node = logical;
    }
    for rr in refs {
        if rr.ty.is_empty() || rr.ty.contains(':') || rr.ty.contains('#') {
            continue;
        }
        let (label, kind) = match &rr.kind {
            RefKind::Wildcard => (format!("{}:*", rr.ty), NodeType::SpecificTypeWildcard),
            RefKind::Relation(rel) if !rel.is_empty() => (
                format!("{}#{rel}", rr.ty),
                NodeType::SpecificTypeAndRelation,
            ),
            _ => (rr.ty.clone(), NodeType::SpecificType),
        };
        let cur = g.node(&label, &label, kind);
        g.upsert_edge(node, cur, EdgeType::Direct, "");
    }
    Ok(())
}

fn parse_ttu(
    g: &mut Graph,
    parent: usize,
    m: &Model,
    t: &str,
    tupleset: &str,
    computed: &str,
) -> Result<(), InvalidGraph> {
    let Some(trel) = m.types[t].relations.get(tupleset) else {
        return Err(InvalidGraph(format!(
            "{tupleset} invalid tupleset relation"
        )));
    };
    let related = &trel.directly_related;
    if related.is_empty() {
        return Err(InvalidGraph(format!(
            "no type and relation link exists for tupleset relation {tupleset}"
        )));
    }
    let type_tupleset = format!("{t}#{tupleset}");
    let mut node = parent;
    if g.nodes[parent].kind != NodeType::SpecificTypeAndRelation && related.len() > 1 {
        let unique = format!("{t}#ttu:{tupleset}#{computed}");
        let logical = g.node(&unique, &unique, NodeType::LogicalTtuGrouping);
        g.add_edge(parent, logical, EdgeType::TtuLogical, &type_tupleset);
        node = logical;
    }
    for rr in related {
        if m.get_relation(&rr.ty, computed).is_err() {
            return Err(InvalidGraph(format!(
                "{} type does not have defined {computed} relation",
                rr.ty
            )));
        }
        let name = format!("{}#{computed}", rr.ty);
        let source = g.node(&name, &name, NodeType::SpecificTypeAndRelation);
        g.upsert_edge(node, source, EdgeType::Ttu, &type_tupleset);
    }
    Ok(())
}

// --- weights (weighted_graph.go) ---------------------------------------------

type Deps = HashMap<String, Vec<usize>>;

fn assign_weights(g: &mut Graph) -> Result<(), InvalidGraph> {
    assign_weights_in_order(g, &(0..g.nodes.len()).collect::<Vec<_>>())
}

fn assign_weights_in_order(g: &mut Graph, order: &[usize]) -> Result<(), InvalidGraph> {
    let mut visited = HashSet::new();
    let mut deps = Deps::new();
    for &n in order {
        if g.is_logical_operator(n) || visited.contains(&n) {
            continue;
        }
        let cycles = node_weight(g, n, &mut visited, &[], &mut deps)?;
        if !cycles.is_empty() {
            return Err(InvalidGraph(format!(
                "{} tuple cycles found without resolution",
                cycles.len()
            )));
        }
    }
    Ok(())
}

fn node_weight(
    g: &mut Graph,
    n: usize,
    visited: &mut HashSet<usize>,
    ancestors: &[usize],
    deps: &mut Deps,
) -> Result<Vec<String>, InvalidGraph> {
    let mut cycles = vec![];
    if visited.contains(&n)
        || matches!(
            g.nodes[n].kind,
            NodeType::SpecificType | NodeType::SpecificTypeWildcard
        )
    {
        return Ok(cycles);
    }
    visited.insert(n);
    for e in g.edges_of(n) {
        if !g.edges[e].weights.is_empty() {
            continue;
        }
        let to = g.edges[e].to;
        if matches!(
            g.nodes[to].kind,
            NodeType::SpecificType | NodeType::SpecificTypeWildcard
        ) {
            let mut label = g.nodes[to].unique.clone();
            if g.nodes[to].kind == NodeType::SpecificTypeWildcard {
                label.truncate(label.len() - 2);
            }
            g.edges[e].weights = Weights::from([(label, 1)]);
            continue;
        }
        let c = edge_weight(g, e, ancestors, visited, deps)?;
        cycles.extend(c);
    }
    weight_from_edges(g, n, deps, cycles)
}

fn edge_weight(
    g: &mut Graph,
    e: usize,
    ancestors: &[usize],
    visited: &mut HashSet<usize>,
    deps: &mut Deps,
) -> Result<Vec<String>, InvalidGraph> {
    let (from, to) = (g.edges[e].from, g.edges[e].to);
    let to_label = g.nodes[to].unique.clone();
    if from == to {
        g.edges[e].weights = Weights::from([(format!("R#{to_label}"), INFINITE)]);
        deps.entry(to_label.clone()).or_default().push(e);
        let from_label = g.nodes[from].unique.clone();
        recursive_metadata(g, &[e], &from_label);
        return Ok(vec![from_label]);
    }
    let mut path = ancestors.to_vec();
    path.push(e);
    let mut cycle = node_weight(g, to, visited, &path, deps)?;
    if g.nodes[to].weights.is_empty() {
        if is_tuple_cycle(g, to, &path) {
            g.edges[e].weights = Weights::from([(format!("R#{to_label}"), INFINITE)]);
            deps.entry(to_label.clone()).or_default().push(e);
            cycle.push(to_label);
            return Ok(cycle);
        }
        return Err(InvalidGraph("model cycle".into()));
    }
    let in_cycle = !cycle.is_empty();
    if in_cycle {
        for id in &cycle {
            deps.entry(id.clone()).or_default().push(e);
        }
    }
    let mut weights = Weights::new();
    for (k, v) in g.nodes[to].weights.clone() {
        if !in_cycle {
            if let Some(dep) = k.strip_prefix("R#") {
                deps.entry(dep.to_string()).or_default().push(e);
                cycle.push(dep.to_string());
            }
        }
        weights.insert(k, v);
    }
    if matches!(g.edges[e].kind, EdgeType::Ttu | EdgeType::Direct) {
        for v in weights.values_mut() {
            if *v != INFINITE {
                *v += 1;
            }
        }
    }
    g.edges[e].weights = weights;
    Ok(cycle)
}

fn is_tuple_cycle(g: &mut Graph, n: usize, path: &[usize]) -> bool {
    let id = g.nodes[n].unique.clone();
    let mut tracking = false;
    let mut tuple_cycle = false;
    let mut recursive_relation = String::new();
    let mut recursion = true;
    let mut cycle_path = vec![];
    for &e in path {
        let (from, to, kind) = (g.edges[e].from, g.edges[e].to, g.edges[e].kind);
        if !tracking && g.nodes[from].unique == id {
            tracking = true;
            if g.nodes[from].kind == NodeType::SpecificTypeAndRelation {
                recursive_relation = id.clone();
            }
        }
        if tracking {
            cycle_path.push(e);
            if kind == EdgeType::Ttu
                || (kind == EdgeType::Direct
                    && g.nodes[to].kind == NodeType::SpecificTypeAndRelation)
            {
                tuple_cycle = true;
            }
            if g.nodes[to].kind == NodeType::SpecificTypeAndRelation {
                if recursive_relation.is_empty() {
                    recursive_relation = g.nodes[to].unique.clone();
                } else if g.nodes[to].unique != recursive_relation {
                    recursion = false;
                }
            }
        }
    }
    if tuple_cycle {
        if recursion {
            recursive_metadata(g, &cycle_path, &recursive_relation);
        } else {
            for &e in &cycle_path {
                let (from, to) = (g.edges[e].from, g.edges[e].to);
                g.nodes[from].tuple_cycle = true;
                g.nodes[to].tuple_cycle = true;
                g.edges[e].tuple_cycle = true;
            }
        }
    }
    tuple_cycle
}

fn recursive_metadata(g: &mut Graph, edges: &[usize], relation: &str) {
    for &e in edges {
        let (from, to) = (g.edges[e].from, g.edges[e].to);
        g.nodes[from].recursive_relation = relation.into();
        g.nodes[to].recursive_relation = relation.into();
        g.edges[e].recursive_relation = relation.into();
    }
}

fn weight_from_edges(
    g: &mut Graph,
    n: usize,
    deps: &mut Deps,
    cycles: Vec<String>,
) -> Result<Vec<String>, InvalidGraph> {
    let id = g.nodes[n].unique.clone();
    if cycles.is_empty() {
        if g.nodes[n].kind != NodeType::Operator {
            max_strategy(g, n)?;
        } else {
            match g.nodes[n].label.as_str() {
                "union" => max_strategy(g, n)?,
                "intersection" => enforce_strategy(g, n)?,
                "exclusion" => mixed_strategy(g, n)?,
                _ => {}
            }
        }
        return Ok(cycles);
    }
    if g.nodes[n].kind == NodeType::SpecificTypeAndRelation && cycles.contains(&id) {
        fix_dependencies(g, n, deps, &cycles)?;
        return Ok(cycles.into_iter().filter(|c| *c != id).collect());
    }
    if !g.is_logical_operator(n) {
        max_strategy(g, n)?;
        return Ok(cycles);
    }
    if g.is_logical_union_operator(n) {
        if cycles.contains(&id) {
            fix_dependencies(g, n, deps, &cycles)?;
            return Ok(cycles.into_iter().filter(|c| *c != id).collect());
        }
        max_strategy(g, n)?;
        return Ok(cycles);
    }
    Err(InvalidGraph(
        "operands AND or BUT NOT cannot be involved in a cycle".into(),
    ))
}

fn no_terminal(g: &Graph, n: usize) -> Result<(), InvalidGraph> {
    let kind = g.nodes[n].kind;
    if g.edges_of(n).is_empty()
        && kind != NodeType::SpecificType
        && kind != NodeType::SpecificTypeWildcard
    {
        return Err(InvalidGraph(format!(
            "{} node does not have any terminal type to reach to",
            g.nodes[n].unique
        )));
    }
    Ok(())
}

fn max_strategy(g: &mut Graph, n: usize) -> Result<(), InvalidGraph> {
    no_terminal(g, n)?;
    let mut weights = Weights::new();
    for e in g.edges_of(n) {
        for (k, &v) in &g.edges[e].weights {
            let w = weights.entry(k.clone()).or_insert(v);
            *w = (*w).max(v);
        }
    }
    g.nodes[n].weights = weights;
    Ok(())
}

fn mixed_strategy(g: &mut Graph, n: usize) -> Result<(), InvalidGraph> {
    no_terminal(g, n)?;
    let edges = g.edges_of(n);
    if edges.len() != 2 {
        return Err(InvalidGraph(
            "invalid number of edges for exclusion node".into(),
        ));
    }
    let mut weights = g.edges[edges[0]].weights.clone();
    for (k, &v) in &g.edges[edges[1]].weights {
        if let Some(w) = weights.get_mut(k) {
            *w = (*w).max(v);
        }
    }
    g.nodes[n].weights = weights;
    Ok(())
}

fn enforce_strategy(g: &mut Graph, n: usize) -> Result<(), InvalidGraph> {
    no_terminal(g, n)?;
    let edges = g.edges_of(n);
    let mut weights = Weights::new();
    for (i, &e) in edges.iter().enumerate() {
        if i == 0 {
            weights = g.edges[e].weights.clone();
        } else {
            let ew = &g.edges[e].weights;
            weights = weights
                .into_iter()
                .filter_map(|(k, w)| ew.get(&k).map(|&v| (k, w.max(v))))
                .collect();
        }
    }
    let empty = weights.is_empty();
    g.nodes[n].weights = weights;
    if empty {
        return Err(InvalidGraph("not all paths return the same type".into()));
    }
    Ok(())
}

fn fix_dependencies(
    g: &mut Graph,
    n: usize,
    deps: &mut Deps,
    cycles: &[String],
) -> Result<(), InvalidGraph> {
    let id = g.nodes[n].unique.clone();
    let reference = format!("R#{id}");
    if g.nodes[n].kind != NodeType::SpecificTypeAndRelation && !g.is_logical_union_operator(n) {
        return Err(InvalidGraph(
            "reference node is not a union operator or a relation".into(),
        ));
    }
    no_terminal(g, n)?;
    let multi_branch = cycles.iter().filter(|c| **c == id).count();
    let mut references = vec![];
    let mut weights = Weights::new();
    for e in g.edges_of(n) {
        for k in g.edges[e].weights.keys() {
            if *k == reference {
                continue;
            }
            if k.starts_with("R#") {
                references.push(k.clone());
            }
            weights.insert(k.clone(), INFINITE);
        }
    }
    g.nodes[n].weights = weights;
    fix_dependent_edges(g, n, &reference, !references.is_empty(), deps);
    fix_dependent_nodes(g, n, &reference, deps);
    if multi_branch > 1 {
        g.nodes[n].tuple_cycle = true;
        for e in deps.get(&id).cloned().unwrap_or_default() {
            let (from, to) = (g.edges[e].from, g.edges[e].to);
            g.edges[e].tuple_cycle = true;
            g.nodes[from].tuple_cycle = true;
            g.nodes[to].tuple_cycle = true;
        }
    }
    deps.remove(&id);
    Ok(())
}

fn merge(into: &mut Weights, k: &str, v: i64) {
    let w = into.entry(k.to_string()).or_insert(v);
    *w = (*w).max(v);
}

fn fix_dependent_edges(
    g: &mut Graph,
    n: usize,
    reference: &str,
    has_references: bool,
    deps: &mut Deps,
) {
    let id = g.nodes[n].unique.clone();
    let node_weights = g.nodes[n].weights.clone();
    let tuple_cycle = g.nodes[n].tuple_cycle;
    // Go ranges over the dependency list as it was when the loop started.
    for e in deps.get(&id).cloned().unwrap_or_default() {
        let mut weights = Weights::new();
        for (k1, v1) in g.edges[e].weights.clone() {
            if k1 == reference {
                for (k2, &v2) in &node_weights {
                    if !weights.contains_key(k2) {
                        weights.insert(k2.clone(), v2);
                        if has_references {
                            if let Some(dep) = k2.strip_prefix("R#") {
                                deps.entry(dep.to_string()).or_default().push(e);
                            }
                        }
                    } else {
                        merge(&mut weights, k2, v2);
                    }
                }
            } else {
                merge(&mut weights, &k1, v1);
            }
        }
        g.edges[e].weights = weights;
        if tuple_cycle {
            g.edges[e].tuple_cycle = true;
        }
    }
}

fn fix_dependent_nodes(g: &mut Graph, n: usize, reference: &str, deps: &Deps) {
    let id = g.nodes[n].unique.clone();
    let node_weights = g.nodes[n].weights.clone();
    let tuple_cycle = g.nodes[n].tuple_cycle;
    for e in deps.get(&id).cloned().unwrap_or_default() {
        let from = g.edges[e].from;
        let mut weights = Weights::new();
        for (k1, v1) in g.nodes[from].weights.clone() {
            if k1 == reference {
                for (k2, &v2) in &node_weights {
                    merge(&mut weights, k2, v2);
                }
            } else {
                merge(&mut weights, &k1, v1);
            }
        }
        g.nodes[from].weights = weights;
        if tuple_cycle {
            g.nodes[from].tuple_cycle = true;
        }
    }
}

// --- strategy predicates (pkg/typesystem) ------------------------------------

struct Strategies<'a> {
    g: &'a Graph,
}

impl Strategies<'_> {
    fn node(&self, label: &str) -> Option<usize> {
        self.g.ids.get(label).copied()
    }

    fn weight(w: &Weights, user_type: &str) -> Option<i64> {
        w.get(user_type).copied()
    }

    /// `UsersetUseWeight2Resolver`.
    fn userset_weight2(
        &self,
        object_type: &str,
        relation: &str,
        user_type: &str,
        userset: (&str, &str),
    ) -> bool {
        let Some(n) = self.node(&format!("{object_type}#{relation}")) else {
            return false;
        };
        let node = &self.g.nodes[n];
        if node.tuple_cycle || !node.recursive_relation.is_empty() {
            return false;
        }
        let Some(u) = self.node(&format!("{}#{}", userset.0, userset.1)) else {
            return false;
        };
        Self::weight(&self.g.nodes[u].weights, user_type) == Some(1)
    }

    /// `TTUUseWeight2Resolver`.
    fn ttu_weight2(
        &self,
        object_type: &str,
        relation: &str,
        user_type: &str,
        tupleset: &str,
        computed: &str,
    ) -> bool {
        let Some(n) = self.node(&format!("{object_type}#{relation}")) else {
            return false;
        };
        let tupleset_key = format!("{object_type}#{tupleset}");
        if Self::weight(&self.g.nodes[n].weights, user_type).is_none() {
            return false;
        }
        let Some(mut edges) = self.g.out.get(&n).cloned() else {
            return false;
        };
        let suffix = format!("#{computed}");
        let mut found = 0;
        while found == 0 {
            let mut inner = vec![];
            for e in edges {
                let edge = &self.g.edges[e];
                if edge.kind == EdgeType::Rewrite {
                    let Some(op) = self.g.out.get(&edge.to) else {
                        return false;
                    };
                    inner.extend(op.iter().copied());
                }
                if edge.kind == EdgeType::Ttu
                    && edge.tupleset_relation == tupleset_key
                    && self.g.nodes[edge.to].unique.ends_with(&suffix)
                {
                    if let Some(w) = Self::weight(&edge.weights, user_type) {
                        if w > 2 {
                            return false;
                        }
                        found += 1;
                    }
                }
            }
            if inner.is_empty() {
                break;
            }
            edges = inner;
        }
        found != 0
    }

    /// `TTUUseRecursiveResolver` (`ttu`) and `UsersetUseRecursiveResolver`.
    fn recursive(&self, object_type: &str, relation: &str, user_type: &str, ttu: bool) -> bool {
        let obj_rel = format!("{object_type}#{relation}");
        let Some(n) = self.node(&obj_rel) else {
            return false;
        };
        let node = &self.g.nodes[n];
        if Self::weight(&node.weights, user_type) != Some(INFINITE) {
            return false;
        }
        if node.recursive_relation != node.unique || node.tuple_cycle {
            return false;
        }
        let Some(mut edges) = self.g.out.get(&n).cloned() else {
            return false;
        };
        let (recursive_kind, logical_kind) = if ttu {
            (EdgeType::Ttu, EdgeType::TtuLogical)
        } else {
            (EdgeType::Direct, EdgeType::DirectLogical)
        };
        let mut found = false;
        while !edges.is_empty() {
            let mut inner = vec![];
            for e in edges {
                let edge = &self.g.edges[e];
                let Some(w) = Self::weight(&edge.weights, user_type) else {
                    continue;
                };
                if edge.recursive_relation == obj_rel {
                    if edge.kind == recursive_kind && edge.to == n && !found {
                        found = true;
                        continue;
                    }
                    if edge.kind == EdgeType::Rewrite || edge.kind == logical_kind {
                        let Some(next) = self.g.out.get(&edge.to) else {
                            return false;
                        };
                        inner.extend(next.iter().copied());
                        continue;
                    }
                }
                if w > 1 {
                    return false;
                }
            }
            if inner.is_empty() {
                break;
            }
            edges = inner;
        }
        found
    }
}

fn ttus(rw: &Rewrite, out: &mut Vec<(String, String)>) {
    match rw {
        Rewrite::Ttu { tupleset, computed } => out.push((tupleset.clone(), computed.clone())),
        Rewrite::Union(c) | Rewrite::Intersection(c) => c.iter().for_each(|c| ttus(c, out)),
        Rewrite::Difference(a, b) => {
            ttus(a, out);
            ttus(b, out);
        }
        _ => {}
    }
}

fn table(m: &Model, g: Option<&Graph>) -> HashSet<String> {
    let mut keys = HashSet::new();
    let Some(g) = g else { return keys }; // OpenFGA discards a graph it cannot build
    let s = Strategies { g };
    for (t, td) in &m.types {
        for (r, rel) in &td.relations {
            let usersets = m.directly_related_usersets(t, r);
            let mut rel_ttus = vec![];
            ttus(&rel.rewrite, &mut rel_ttus);
            for user_type in m.types.keys() {
                let alt = s.recursive(t, r, user_type, false)
                    || usersets
                        .iter()
                        .any(|u| s.userset_weight2(t, r, user_type, (&u.ty, u.relation())));
                if alt {
                    keys.insert(format!("userset:{t}#{r}@{user_type}"));
                }
                for (ts, cu) in &rel_ttus {
                    if s.ttu_weight2(t, r, user_type, ts, cu) || s.recursive(t, r, user_type, true)
                    {
                        keys.insert(format!("ttu:{t}#{r}:{ts}:{cu}@{user_type}"));
                    }
                }
            }
        }
    }
    keys
}

/// The nodes OpenFGA's planner may resolve with its recursive or weight-2
/// strategies (keys as [`Model::ttu_alternative`] and
/// [`Model::userset_alternative`] look them up).
pub fn alternatives(m: &Model) -> HashSet<String> {
    table(m, build(m).ok().as_ref())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Builds with nodes visited in several orders and returns the distinct
    /// tables.
    pub fn tables_over_orders(m: &Model, seeds: u64) -> HashSet<Vec<String>> {
        let mut out = HashSet::new();
        for seed in 0..seeds {
            let mut g = Graph::default();
            // Same construction as `build`, without its weight assignment.
            let mut types: Vec<&String> = m.types.keys().collect();
            types.sort();
            let mut ok = true;
            for t in types {
                g.node(t, t, NodeType::SpecificType);
                let td = &m.types[t];
                let mut rels: Vec<&String> = td.relations.keys().collect();
                rels.sort();
                for r in rels {
                    let unique = format!("{t}#{r}");
                    let parent = g.node(&unique, &unique, NodeType::SpecificTypeAndRelation);
                    let mut i = 0;
                    ok &= parse_rewrite(&mut g, parent, m, &td.relations[r].rewrite, t, r, &mut i)
                        .is_ok();
                }
            }
            let mut order: Vec<usize> = (0..g.nodes.len()).collect();
            let mut x = seed.wrapping_mul(0x9E3779B97F4A7C15) | 1;
            for i in (1..order.len()).rev() {
                x ^= x << 13;
                x ^= x >> 7;
                x ^= x << 17;
                order.swap(i, (x % (i as u64 + 1)) as usize);
            }
            ok &= assign_weights_in_order(&mut g, &order).is_ok();
            let mut t: Vec<String> = table(m, ok.then_some(&g)).into_iter().collect();
            t.sort();
            out.insert(t);
        }
        out
    }

    #[test]
    fn recursive_parent() {
        let m = Model::from_json(
            br#"{"schema_version":"1.1","type_definitions":[{"type":"user"},{"type":"folder","relations":{
                "parent":{"this":{}},
                "viewer":{"union":{"child":[{"this":{}},{"tuple_to_userset":{"tupleset":{"relation":"parent"},"computed_userset":{"relation":"viewer"}}}]}}},
              "metadata":{"relations":{"parent":{"directly_related_user_types":[{"type":"folder"}]},"viewer":{"directly_related_user_types":[{"type":"user"}]}}}}]}"#,
        )
        .unwrap();
        let alts = alternatives(&m);
        assert!(
            alts.contains("ttu:folder#viewer:parent:viewer@user"),
            "{alts:?}"
        );
        assert_eq!(tables_over_orders(&m, 20).len(), 1);
    }
}
