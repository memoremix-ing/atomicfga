//! What several test targets share.

use serde_json::json;

/// The model of spec/check.qnt, in protojson.
pub fn spec_model() -> serde_json::Value {
    let direct = |types: &[serde_json::Value]| json!({"directly_related_user_types": types});
    let user = json!({"type": "user"});
    let wildcard = json!({"type": "user", "wildcard": {}});
    let member = json!({"type": "group", "relation": "member"});
    let folder = json!({"type": "folder"});
    let this = json!({"this": {}});
    let computed = |r: &str| json!({"computed_userset": {"relation": r}});
    let ttu = |ts: &str, cu: &str| json!({"tuple_to_userset": {"tupleset": {"relation": ts}, "computed_userset": {"relation": cu}}});
    json!({
        "id": "01M5QUNTSPEC000000000000000",
        "schema_version": "1.1",
        "type_definitions": [
            {"type": "user"},
            {"type": "group", "relations": {"member": this},
             "metadata": {"relations": {"member": direct(&[user.clone(), wildcard, member.clone()])}}},
            {"type": "folder", "relations": {
                "parent": this,
                "owner": this,
                "viewer": {"union": {"child": [this, computed("owner"), ttu("parent", "viewer")]}}},
             "metadata": {"relations": {
                "parent": direct(std::slice::from_ref(&folder)),
                "owner": direct(&[user.clone(), member.clone()]),
                "viewer": direct(&[user.clone(), member.clone()])}}},
            {"type": "doc", "relations": {
                "parent": this,
                "blocked": this,
                "editor": this,
                "public": this,
                "viewer": {"difference": {
                    "base": {"union": {"child": [computed("editor"), ttu("parent", "viewer")]}},
                    "subtract": computed("blocked")}},
                "restricted": {"intersection": {"child": [computed("editor"), ttu("parent", "viewer")]}}},
             "metadata": {"relations": {
                "parent": direct(&[folder]),
                "blocked": direct(&[user.clone(), member]),
                "editor": direct(&[user]),
                "public": direct(&[json!({"type": "user", "wildcard": {}})])}}},
        ],
    })
}
