//! [redb](https://www.redb.org) storage for `atomicfga-check`: tuples and
//! authorization models in the caller's redb transactions.
//!
//! Wrap a transaction and hand it to [`atomicfga_check::api::Fga`]:
//!
//! ```ignore
//! let tx = db.begin_write()?;
//! fga.write(&Write(&tx), "store", request);      // validated as OpenFGA does
//! app_table.insert(...)?;                        // the application's own data
//! let answer = fga.check(&Write(&tx), "store", check, Semantics::OpenFga);
//! tx.commit()?;                                  // or abort: both or neither
//! ```
//!
//! This crate only stores bytes: two tables keyed by tuple and by model, an
//! exact lookup and a prefix scan. Everything about authorization lives in
//! `atomicfga-check`.

use atomicfga_check::store::{Store, StoreMut};
use redb::{ReadTransaction, ReadableTable, TableDefinition, TableError, WriteTransaction};

/// (store, object, relation, user) → the tuple's encoded condition.
pub const TUPLES: TableDefinition<(&str, &str, &str, &str), &[u8]> =
    TableDefinition::new("atomicfga_tuples");
/// (store, model ID) → the model in protojson.
pub const MODELS: TableDefinition<(&str, &str), &[u8]> = TableDefinition::new("atomicfga_models");

/// A read transaction as a [`Store`].
pub struct Read<'a>(pub &'a ReadTransaction);

/// A write transaction as a [`StoreMut`]; reads see its uncommitted writes.
pub struct Write<'a>(pub &'a WriteTransaction);

fn err(e: impl std::fmt::Display) -> String {
    e.to_string()
}

fn get(
    t: &impl ReadableTable<(&'static str, &'static str, &'static str, &'static str), &'static [u8]>,
    k: (&str, &str, &str, &str),
) -> Result<Option<Vec<u8>>, String> {
    Ok(t.get(k).map_err(err)?.map(|v| v.value().to_vec()))
}

fn scan(
    t: &impl ReadableTable<(&'static str, &'static str, &'static str, &'static str), &'static [u8]>,
    store: &str,
    object: &str,
    relation: &str,
) -> Result<Vec<(String, Vec<u8>)>, String> {
    let mut out = vec![];
    for row in t.range((store, object, relation, "")..).map_err(err)? {
        let (k, v) = row.map_err(err)?;
        let (s, o, r, u) = k.value();
        if s != store || o != object || r != relation {
            break;
        }
        out.push((u.to_string(), v.value().to_vec()));
    }
    Ok(out)
}

fn model(
    t: &impl ReadableTable<(&'static str, &'static str), &'static [u8]>,
    store: &str,
    id: &str,
) -> Result<Option<Vec<u8>>, String> {
    Ok(t.get((store, id)).map_err(err)?.map(|v| v.value().to_vec()))
}

fn latest(
    t: &impl ReadableTable<(&'static str, &'static str), &'static [u8]>,
    store: &str,
) -> Result<Option<(String, Vec<u8>)>, String> {
    // The greatest key with this store: the end of the store's range.
    let mut range = t.range((store, "")..).map_err(err)?;
    let mut last = None;
    for row in range.by_ref() {
        let (k, v) = row.map_err(err)?;
        let (s, id) = k.value();
        if s != store {
            break;
        }
        last = Some((id.to_string(), v.value().to_vec()));
    }
    Ok(last)
}

impl Store for Read<'_> {
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        match self.0.open_table(TUPLES) {
            Ok(t) => get(&t, (store, object, relation, user)),
            Err(TableError::TableDoesNotExist(_)) => Ok(None),
            Err(e) => Err(err(e)),
        }
    }
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        match self.0.open_table(TUPLES) {
            Ok(t) => scan(&t, store, object, relation),
            Err(TableError::TableDoesNotExist(_)) => Ok(vec![]),
            Err(e) => Err(err(e)),
        }
    }
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        match self.0.open_table(MODELS) {
            Ok(t) => model(&t, store, id),
            Err(TableError::TableDoesNotExist(_)) => Ok(None),
            Err(e) => Err(err(e)),
        }
    }
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        match self.0.open_table(MODELS) {
            Ok(t) => latest(&t, store),
            Err(TableError::TableDoesNotExist(_)) => Ok(None),
            Err(e) => Err(err(e)),
        }
    }
}

impl Store for Write<'_> {
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        get(
            &self.0.open_table(TUPLES).map_err(err)?,
            (store, object, relation, user),
        )
    }
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        scan(
            &self.0.open_table(TUPLES).map_err(err)?,
            store,
            object,
            relation,
        )
    }
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        model(&self.0.open_table(MODELS).map_err(err)?, store, id)
    }
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        latest(&self.0.open_table(MODELS).map_err(err)?, store)
    }
}

impl StoreMut for Write<'_> {
    fn put_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
        value: &[u8],
    ) -> Result<(), String> {
        self.0
            .open_table(TUPLES)
            .map_err(err)?
            .insert((store, object, relation, user), value)
            .map_err(err)?;
        Ok(())
    }
    fn delete_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<(), String> {
        self.0
            .open_table(TUPLES)
            .map_err(err)?
            .remove((store, object, relation, user))
            .map_err(err)?;
        Ok(())
    }
    fn put_model(&self, store: &str, id: &str, model: &[u8]) -> Result<(), String> {
        self.0
            .open_table(MODELS)
            .map_err(err)?
            .insert((store, id), model)
            .map_err(err)?;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use atomicfga_check::api::Fga;
    use atomicfga_check::Semantics;
    use redb::{backends::InMemoryBackend, Database, ReadableDatabase};
    use serde_json::{json, Value};

    const MODEL: &str = r#"{"schema_version":"1.1","type_definitions":[{"type":"user"},
        {"type":"doc","relations":{"viewer":{"this":{}}},
         "metadata":{"relations":{"viewer":{"directly_related_user_types":[{"type":"user"}]}}}}]}"#;

    fn check(fga: &Fga, s: &impl Store, user: &str) -> Value {
        let req = json!({"tuple_key": {"object": "doc:1", "relation": "viewer", "user": user}});
        fga.check(
            s,
            "s",
            &serde_json::to_vec(&req).unwrap(),
            Semantics::OpenFga,
        )
    }

    #[test]
    fn caller_owned_transactions() {
        let db = Database::builder()
            .create_with_backend(InMemoryBackend::new())
            .unwrap();
        let fga = Fga::default();
        let w = json!({"writes": {"tuple_keys": [{"object": "doc:1", "relation": "viewer", "user": "user:anne"}]}});
        let tx = db.begin_write().unwrap();
        fga.write_model(
            &Write(&tx),
            "s",
            "01ARZ3NDEKTSV4RRFFQ69G5FAV",
            MODEL.as_bytes(),
        );
        assert_eq!(
            fga.write(&Write(&tx), "s", &serde_json::to_vec(&w).unwrap()),
            json!({})
        );
        // Read-your-writes inside the uncommitted transaction.
        assert_eq!(check(&fga, &Write(&tx), "user:anne")["allowed"], true);
        assert_eq!(check(&fga, &Write(&tx), "user:bob")["allowed"], false);
        tx.abort().unwrap();
        // Rolled back: no model, no tuple.
        let r = db.begin_read().unwrap();
        assert_eq!(
            check(&fga, &Read(&r), "user:anne")["error"]["code"],
            "latest_authorization_model_not_found"
        );
        drop(r);

        let tx = db.begin_write().unwrap();
        fga.write_model(
            &Write(&tx),
            "s",
            "01ARZ3NDEKTSV4RRFFQ69G5FAV",
            MODEL.as_bytes(),
        );
        fga.write(&Write(&tx), "s", &serde_json::to_vec(&w).unwrap());
        tx.commit().unwrap();
        let r = db.begin_read().unwrap();
        assert_eq!(check(&fga, &Read(&r), "user:anne")["allowed"], true);
    }
}
