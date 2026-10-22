// The canonical semantics of Check, for one authorization model.
//
// A check holds iff its (object, relation, user) fact is derivable from the
// stored tuples: the least fixed point of the rules below, computed
// bottom-up over a finite universe. There is no evaluation order, no cycle
// flag and no depth limit. Exclusion is stratified: `blocked` does not
// depend on `viewer`, so the fixed point is well defined.
//
// The model (OpenFGA DSL, for reference):
//
//   type user
//   type group
//     relations
//       define member: [user, user:*, group#member]
//   type folder
//     relations
//       define parent: [folder]
//       define owner: [user, group#member]
//       define viewer: [user, group#member] or owner or viewer from parent
//   type doc
//     relations
//       define parent: [folder]
//       define blocked: [user, group#member]
//       define editor: [user]
//       define viewer: (editor or viewer from parent) but not blocked
//       define restricted: editor and viewer from parent
//       define public: [user:*]
//
// Actions write and delete valid tuples (`stored`), and check a query; `result` holds
// the last query and its canonical answer, which the Rust evaluator must
// reproduce (rust/tests/quint.rs).
module check {
  type Tuple = { object: str, relation: str, user: str }
  type Query = { object: str, relation: str, user: str }
  type Fact = (str, str, str)

  pure val GROUPS = Set("group:g1", "group:g2")
  pure val FOLDERS = Set("folder:f1", "folder:f2")
  pure val DOCS = Set("doc:d1")
  pure val USERS = Set("user:a", "user:b")
  pure val WILDCARD = "user:*"
  // Quint has no string operations, so the object of each userset is listed.
  pure val USERSETS: str -> str = Map("group:g1#member" -> "group:g1", "group:g2#member" -> "group:g2")
  pure val SUBJECTS = USERS.union(USERSETS.keys())

  pure def tuplesFor(objects: Set[str], relation: str, users: Set[str]): Set[Tuple] =
    tuples(objects, users).map(((o, u)) => { object: o, relation: relation, user: u })

  // An implicit tuple (`group:g1#member@group:g1#member`) restates what holds
  // anyway; OpenFGA refuses to write one.
  pure def implicit(t: Tuple): bool =
    USERSETS.keys().contains(t.user) and USERSETS.get(t.user) == t.object and t.relation == "member"

  // Every tuple the model's type restrictions allow, except implicit ones.
  pure val POOL: Set[Tuple] =
    tuplesFor(GROUPS, "member", USERS.union(Set(WILDCARD)).union(USERSETS.keys())).filter(t => not(implicit(t)))
      .union(tuplesFor(FOLDERS, "parent", FOLDERS))
      .union(tuplesFor(FOLDERS, "owner", SUBJECTS))
      .union(tuplesFor(FOLDERS, "viewer", SUBJECTS))
      .union(tuplesFor(DOCS, "parent", FOLDERS))
      .union(tuplesFor(DOCS, "blocked", SUBJECTS))
      .union(tuplesFor(DOCS, "editor", USERS))
      .union(tuplesFor(DOCS, "public", Set(WILDCARD)))

  pure val RELATIONS: Set[(str, str)] =
    tuples(GROUPS, Set("member"))
      .union(tuples(FOLDERS, Set("owner", "viewer")))
      .union(tuples(DOCS, Set("blocked", "editor", "viewer", "restricted", "public")))

  pure val QUERIES: Set[Query] =
    tuples(RELATIONS, SUBJECTS).map(((rel, u)) => { object: rel._1, relation: rel._2, user: u })

  pure val FACTS: Set[Fact] = QUERIES.map(q => (q.object, q.relation, q.user))

  // A fact holds if derived, or if it is self-defining (`group:g1#member`
  // is a member of `group:g1`).
  pure def holds(H: Set[Fact], o: str, r: str, u: str): bool =
    H.contains((o, r, u)) or USERSETS.keys().exists(s => s == u and USERSETS.get(s) == o and r == "member")

  // Direct assignment: the user itself, the typed wildcard, or a userset
  // whose members include the user.
  pure def direct(T: Set[Tuple], H: Set[Fact], o: str, r: str, u: str): bool = or {
    T.contains({ object: o, relation: r, user: u }),
    USERS.contains(u) and T.contains({ object: o, relation: r, user: WILDCARD }),
    USERSETS.keys().exists(s => T.contains({ object: o, relation: r, user: s }) and holds(H, USERSETS.get(s), "member", u)),
  }

  // Tuple to userset: `computed from tupleset`.
  pure def fromParent(T: Set[Tuple], H: Set[Fact], o: str, tupleset: str, computed: str, u: str): bool =
    T.exists(t => t.object == o and t.relation == tupleset and holds(H, t.user, computed, u))

  pure def derive(T: Set[Tuple], H: Set[Fact], o: str, r: str, u: str): bool =
    if (GROUPS.contains(o)) direct(T, H, o, r, u)
    else if (FOLDERS.contains(o) and r == "owner") direct(T, H, o, r, u)
    else if (FOLDERS.contains(o)) or {
      direct(T, H, o, r, u),
      holds(H, o, "owner", u),
      fromParent(T, H, o, "parent", "viewer", u),
    }
    else if (r == "blocked" or r == "editor" or r == "public") direct(T, H, o, r, u)
    else if (r == "viewer") and {
      holds(H, o, "editor", u) or fromParent(T, H, o, "parent", "viewer", u),
      not(holds(H, o, "blocked", u)),
    }
    else and {
      holds(H, o, "editor", u),
      fromParent(T, H, o, "parent", "viewer", u),
    }

  // Iterating all rules at once from the empty set: every positive fact is
  // derived by the longest chain in the universe, after which `blocked` is
  // final and `viewer` is computed from it.
  pure val ROUNDS = 16

  pure def closure(T: Set[Tuple]): Set[Fact] =
    1.to(ROUNDS).fold(Set(), (H, _) => FACTS.filter(((o, r, u)) => derive(T, H, o, r, u)))

  pure def check(T: Set[Tuple], q: Query): bool = holds(closure(T), q.object, q.relation, q.user)

  var stored: Set[Tuple]
  var result: { object: str, relation: str, user: str, allowed: bool }

  action init = all {
    stored' = Set(),
    result' = { object: "", relation: "", user: "", allowed: false },
  }

  action WriteTuple = {
    nondet t = POOL.oneOf()
    all {
      stored' = stored.union(Set(t)),
      result' = result,
    }
  }

  action DeleteTuple = {
    nondet t = POOL.oneOf()
    all {
      stored' = stored.exclude(Set(t)),
      result' = result,
    }
  }

  action Check = {
    nondet q = QUERIES.oneOf()
    all {
      stored' = stored,
      result' = { object: q.object, relation: q.relation, user: q.user, allowed: check(stored, q) },
    }
  }

  action step = any { WriteTuple, WriteTuple, DeleteTuple, Check }

  // The closure is a fixed point: one more round changes nothing.
  val ClosureIsFixed = {
    val H = closure(stored)
    FACTS.filter(((o, r, u)) => derive(stored, H, o, r, u)) == H
  }

  // A blocked user never views the doc.
  val BlockedNeverViews = {
    val H = closure(stored)
    SUBJECTS.forall(u => not(holds(H, "doc:d1", "viewer", u) and holds(H, "doc:d1", "blocked", u)))
  }
}
