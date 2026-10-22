// Intentionally failing regression probes, isolated from the project's test suite.
use atomicfga_check::{
    api::Fga,
    store::{MemoryStore, StoreMut},
    Semantics,
};
use serde_json::{json, Value};

const ID: &str = "01ARZ3NDEKTSV4RRFFQ69G5FAV";

fn model(restrictive: bool) -> Value {
    let viewer = if restrictive {
        json!({"difference":{"base":{"computed_userset":{"relation":"editor"}},"subtract":{"computed_userset":{"relation":"blocked"}}}})
    } else {
        json!({"computed_userset":{"relation":"editor"}})
    };
    json!({"schema_version":"1.1","type_definitions":[{"type":"user"},{"type":"doc",
        "relations":{"editor":{"this":{}},"blocked":{"this":{}},"viewer":viewer},
        "metadata":{"relations":{
            "editor":{"directly_related_user_types":[{"type":"user"}]},
            "blocked":{"directly_related_user_types":[{"type":"user"}]}
        }}
    }]})
}

fn install(f: &Fga, s: &MemoryStore, store: &str, restrictive: bool) {
    let r = f.write_model(
        s,
        store,
        ID,
        &serde_json::to_vec(&model(restrictive)).unwrap(),
    );
    assert_eq!(r["authorization_model_id"], ID, "{r}");
    s.put_tuple(store, "doc:1", "editor", "user:anne", b"")
        .unwrap();
    s.put_tuple(store, "doc:1", "blocked", "user:anne", b"")
        .unwrap();
}

fn check(f: &Fga, s: &MemoryStore, store: &str, relation: &str) -> Value {
    f.check(
        s,
        store,
        &serde_json::to_vec(&json!({"tuple_key":{
            "object":"doc:1","relation":relation,"user":"user:anne"
        }}))
        .unwrap(),
        Semantics::OpenFga,
    )
}

#[test]
fn same_id_in_two_stores_must_not_share_compiled_models() {
    let f = Fga::default();
    let s = MemoryStore::default();
    install(&f, &s, "a", false);
    assert_eq!(check(&f, &s, "a", "viewer")["allowed"], true);
    install(&f, &s, "b", true);
    assert_eq!(check(&Fga::default(), &s, "b", "viewer")["allowed"], false);
    let got = check(&f, &s, "b", "viewer");
    assert_eq!(
        got["allowed"], false,
        "warm evaluator used store a's permissive model: {got}"
    );
}

#[test]
fn model_overwrite_must_not_leave_a_stale_grant() {
    let f = Fga::default();
    let s = MemoryStore::default();
    install(&f, &s, "a", false);
    assert_eq!(check(&f, &s, "a", "viewer")["allowed"], true);
    install(&f, &s, "a", true); // API currently accepts replacement
    assert_eq!(check(&Fga::default(), &s, "a", "viewer")["allowed"], false);
    let got = check(&f, &s, "a", "viewer");
    assert_eq!(
        got["allowed"], false,
        "accepted replacement did not affect warm evaluator: {got}"
    );
}

#[test]
fn corrupt_condition_must_not_become_unconditional() {
    let f = Fga::default();
    let s = MemoryStore::default();
    let mut m = model(false);
    m["type_definitions"][1]["metadata"]["relations"]["editor"]["directly_related_user_types"] =
        json!([{"type":"user"},{"type":"user","condition":"guard"}]);
    m["conditions"] = json!({"guard":{"name":"guard","expression":"ok","parameters":{"ok":{"type_name":"TYPE_NAME_BOOL"}}}});
    let r = f.write_model(&s, "a", ID, &serde_json::to_vec(&m).unwrap());
    assert_eq!(r["authorization_model_id"], ID, "{r}");
    s.put_tuple(
        "a",
        "doc:1",
        "editor",
        "user:anne",
        br#"{"name":"guard","context":{"ok":false}}"#,
    )
    .unwrap();
    assert_eq!(check(&f, &s, "a", "editor")["allowed"], false);
    s.put_tuple("a", "doc:1", "editor", "user:anne", b"{")
        .unwrap();
    let got = check(&f, &s, "a", "editor");
    assert_ne!(
        got["allowed"], true,
        "malformed nonempty condition bytes granted access: {got}"
    );
}

#[test]
fn canonical_semantics_must_cover_a_derivable_long_chain() {
    let f = Fga::default();
    let s = MemoryStore::default();
    let m = json!({"schema_version":"1.1","type_definitions":[{"type":"user"},{"type":"folder",
        "relations":{"parent":{"this":{}},"viewer":{"union":{"child":[{"this":{}},{"tuple_to_userset":{"tupleset":{"relation":"parent"},"computed_userset":{"relation":"viewer"}}}]}}},
        "metadata":{"relations":{"parent":{"directly_related_user_types":[{"type":"folder"}]},"viewer":{"directly_related_user_types":[{"type":"user"}]}}}
    }]});
    let r = f.write_model(&s, "s", ID, &serde_json::to_vec(&m).unwrap());
    assert_eq!(r["authorization_model_id"], ID, "{r}");
    for i in 0..26 {
        s.put_tuple(
            "s",
            &format!("folder:{i}"),
            "parent",
            &format!("folder:{}", i + 1),
            b"",
        )
        .unwrap();
    }
    s.put_tuple("s", "folder:26", "viewer", "user:anne", b"")
        .unwrap();
    let req = serde_json::to_vec(
        &json!({"tuple_key":{"object":"folder:0","relation":"viewer","user":"user:anne"}}),
    )
    .unwrap();
    let got = f.check(&s, "s", &req, Semantics::Canonical);
    assert_eq!(
        got["allowed"], true,
        "finite derivation exists but canonical check returned {got}"
    );
}
