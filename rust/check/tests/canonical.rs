//! Canonical checks against an independent oracle: the least fixed point of
//! spec/check.qnt's rules, computed bottom-up as the spec computes it, over
//! universes far larger than the spec's (folder chains up to 70 long,
//! membership cycles). Every answer must equal the oracle's; none may be
//! refused.

use atomicfga_check::api::Fga;
use atomicfga_check::store::{MemoryStore, StoreMut};
use atomicfga_check::Semantics;
use serde_json::json;
use std::collections::HashSet;

mod common;

const STORE: &str = "s";
const MODEL_ID: &str = "01HZZZ00000000000000000001";
const USERS: [&str; 3] = ["user:a", "user:b", "user:c"];
const WILDCARD: &str = "user:*";

type Tuple = (String, String, String);

struct Universe {
    groups: Vec<String>,
    folders: Vec<String>,
    docs: Vec<String>,
    tuples: HashSet<Tuple>,
}

impl Universe {
    fn usersets(&self) -> impl Iterator<Item = String> + '_ {
        self.groups.iter().map(|g| format!("{g}#member"))
    }

    fn subjects(&self) -> Vec<String> {
        USERS
            .iter()
            .map(|u| u.to_string())
            .chain(self.usersets())
            .collect()
    }

    fn has(&self, o: &str, r: &str, u: &str) -> bool {
        self.tuples.contains(&(o.into(), r.into(), u.into()))
    }
}

/// The spec's `holds`: derived, or self-defining.
fn holds(h: &HashSet<Tuple>, o: &str, r: &str, u: &str) -> bool {
    h.contains(&(o.into(), r.into(), u.into())) || (r == "member" && u == format!("{o}#member"))
}

/// The spec's `direct`.
fn direct(w: &Universe, h: &HashSet<Tuple>, o: &str, r: &str, u: &str) -> bool {
    w.has(o, r, u)
        || (USERS.contains(&u) && w.has(o, r, WILDCARD))
        || w.groups
            .iter()
            .any(|g| w.has(o, r, &format!("{g}#member")) && holds(h, g, "member", u))
}

/// The spec's `fromParent`.
fn from_parent(w: &Universe, h: &HashSet<Tuple>, o: &str, u: &str) -> bool {
    w.folders
        .iter()
        .any(|f| w.has(o, "parent", f) && holds(h, f, "viewer", u))
}

/// The spec's `derive`.
fn derive(w: &Universe, h: &HashSet<Tuple>, o: &str, r: &str, u: &str) -> bool {
    match (o.split(':').next().unwrap(), r) {
        ("group", _) | ("folder", "owner") | ("doc", "blocked" | "editor" | "public") => {
            direct(w, h, o, r, u)
        }
        ("folder", _) => {
            direct(w, h, o, r, u) || holds(h, o, "owner", u) || from_parent(w, h, o, u)
        }
        ("doc", "viewer") => {
            (holds(h, o, "editor", u) || from_parent(w, h, o, u)) && !holds(h, o, "blocked", u)
        }
        _ => holds(h, o, "editor", u) && from_parent(w, h, o, u),
    }
}

fn queries(w: &Universe) -> Vec<Tuple> {
    let mut rels: Vec<(String, &str)> = vec![];
    rels.extend(w.groups.iter().map(|g| (g.clone(), "member")));
    for f in &w.folders {
        rels.extend([(f.clone(), "owner"), (f.clone(), "viewer")]);
    }
    for d in &w.docs {
        for r in ["blocked", "editor", "viewer", "restricted", "public"] {
            rels.push((d.clone(), r));
        }
    }
    let subjects = w.subjects();
    rels.into_iter()
        .flat_map(|(o, r)| {
            subjects
                .iter()
                .map(move |u| (o.clone(), r.to_string(), u.clone()))
        })
        .collect()
}

/// The least fixed point: the positive rules first (iterated from nothing
/// until nothing changes), then `doc#viewer`, whose subtrahend is then final.
fn closure(w: &Universe) -> HashSet<Tuple> {
    let all = queries(w);
    let mut h = HashSet::new();
    for stratum in [false, true] {
        loop {
            let next: HashSet<Tuple> = all
                .iter()
                .filter(|(o, r, _)| stratum || !(o.starts_with("doc:") && r == "viewer"))
                .filter(|(o, r, u)| {
                    h.contains(&(o.clone(), r.clone(), u.clone())) || derive(w, &h, o, r, u)
                })
                .cloned()
                .collect();
            if next.len() == h.len() {
                break;
            }
            h = next;
        }
    }
    h
}

// mulberry32
struct Rng(u32);

impl Rng {
    fn next(&mut self) -> u32 {
        self.0 = self.0.wrapping_add(0x6d2b79f5);
        let mut t = self.0;
        t = (t ^ (t >> 15)).wrapping_mul(t | 1);
        t ^= t.wrapping_add((t ^ (t >> 7)).wrapping_mul(t | 61));
        t ^ (t >> 14)
    }
    fn below(&mut self, n: usize) -> usize {
        (self.next() as usize) % n
    }
    fn chance(&mut self, percent: u32) -> bool {
        self.next() % 100 < percent
    }
}

/// A random universe: a long folder chain with some extra (possibly cyclic)
/// parents, groups nested in one another (with cycles), and tuples of every
/// relation. Implicit tuples are left out, as OpenFGA refuses them.
fn universe(seed: u32) -> Universe {
    let mut r = Rng(seed);
    let groups: Vec<String> = (0..2 + r.below(5)).map(|i| format!("group:g{i}")).collect();
    let folders: Vec<String> = (0..10 + r.below(61))
        .map(|i| format!("folder:f{i}"))
        .collect();
    let docs: Vec<String> = (0..1 + r.below(3)).map(|i| format!("doc:d{i}")).collect();
    let (g, f, d) = (groups.clone(), folders.clone(), docs.clone());
    let mut tuples = HashSet::new();
    let mut t = |o: &str, rel: &str, u: &str| {
        tuples.insert((o.to_string(), rel.to_string(), u.to_string()));
    };
    let pick = |r: &mut Rng, xs: &[String]| xs[r.below(xs.len())].clone();
    let subject = |r: &mut Rng| {
        if r.chance(60) {
            USERS[r.below(USERS.len())].to_string()
        } else {
            format!("{}#member", g[r.below(g.len())])
        }
    };
    for i in 1..f.len() {
        t(&f[i - 1], "parent", &f[i]);
    }
    for _ in 0..r.below(4) {
        let (a, b) = (pick(&mut r, &f), pick(&mut r, &f));
        t(&a, "parent", &b);
    }
    for a in &g {
        for b in &g {
            if a != b && r.chance(35) {
                t(a, "member", &format!("{b}#member"));
            }
        }
        if r.chance(50) {
            t(a, "member", USERS[r.below(USERS.len())]);
        }
        if r.chance(10) {
            t(a, "member", WILDCARD);
        }
    }
    for _ in 0..1 + r.below(3) {
        let (o, s) = (pick(&mut r, &f), subject(&mut r));
        t(&o, "viewer", &s);
        let (o, s) = (pick(&mut r, &f), subject(&mut r));
        t(&o, "owner", &s);
    }
    for doc in &d {
        for _ in 0..1 + r.below(2) {
            let p = pick(&mut r, &f);
            t(doc, "parent", &p);
        }
        for _ in 0..r.below(3) {
            let s = subject(&mut r);
            t(doc, "blocked", &s);
        }
        for _ in 0..r.below(3) {
            t(doc, "editor", USERS[r.below(USERS.len())]);
        }
        if r.chance(30) {
            t(doc, "public", WILDCARD);
        }
    }
    Universe {
        groups,
        folders,
        docs,
        tuples,
    }
}

#[test]
fn canonical_checks_equal_the_least_fixed_point() {
    let model = serde_json::to_vec(&common::spec_model()).unwrap();
    let (mut checks, mut allowed) = (0, 0);
    for seed in 1..=24 {
        let w = universe(seed);
        let fga = Fga::default();
        let s = MemoryStore::default();
        let r = fga.write_model(&s, STORE, MODEL_ID, &model);
        assert_eq!(r["authorization_model_id"], MODEL_ID, "{r}");
        for (o, rel, u) in &w.tuples {
            s.put_tuple(STORE, o, rel, u, b"").unwrap();
        }
        let want = closure(&w);
        // Every group and doc fact, and every fact of folders at the chain's
        // start, middle and end.
        let n = w.folders.len();
        let sampled: HashSet<&String> = [0, n / 2, n - 1].iter().map(|&i| &w.folders[i]).collect();
        for (o, rel, u) in queries(&w) {
            if o.starts_with("folder:") && !sampled.contains(&o) {
                continue;
            }
            let req = json!({"authorization_model_id": MODEL_ID, "tuple_key": {"object": o, "relation": rel, "user": u}});
            let got = fga.check(
                &s,
                STORE,
                &serde_json::to_vec(&req).unwrap(),
                Semantics::Canonical,
            );
            let expect = holds(&want, &o, &rel, &u);
            assert_eq!(
                got["allowed"].as_bool(),
                Some(expect),
                "seed {seed}: {o}#{rel}@{u}: got {got}, the least fixed point says {expect}"
            );
            checks += 1;
            allowed += expect as usize;
        }
    }
    eprintln!("{checks} canonical checks, {allowed} allowed, all equal to the least fixed point");
    assert!(
        allowed > checks / 20 && allowed < checks - checks / 20,
        "too one-sided: {allowed} of {checks}"
    );
}
