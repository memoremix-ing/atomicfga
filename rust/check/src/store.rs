//! The storage interface: everything [`crate::api::Fga`] needs from a store.
//!
//! A store keeps tuples, keyed `(store, object, relation, user)`, and
//! authorization models, keyed `(store, model ID)`. Values are opaque bytes:
//! a tuple's encoded condition (empty for none) and a model's protojson. The
//! store answers inside a transaction the caller owns, so implementations
//! wrap a transaction (redb's, a SQL connection's, a browser module's). A
//! store needs no knowledge of authorization: no validation, no resolution,
//! no query semantics beyond an exact lookup and a prefix scan.

/// Read access, within one transaction.
pub trait Store {
    /// The tuple `(store, object, relation, user)`'s value, if it exists.
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String>;

    /// Every tuple of `(store, object, relation)`: users and values, in any
    /// order.
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String>;

    /// The model `(store, id)`.
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String>;

    /// The store's model with the greatest ID (IDs are ULIDs, so the latest
    /// written), with its ID.
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String>;
}

/// Write access, within one transaction. Mutations become visible to later
/// reads of the same transaction.
pub trait StoreMut: Store {
    fn put_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
        value: &[u8],
    ) -> Result<(), String>;
    fn delete_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<(), String>;
    fn put_model(&self, store: &str, id: &str, model: &[u8]) -> Result<(), String>;
}

impl<S: Store + ?Sized> Store for &S {
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        (**self).tuple(store, object, relation, user)
    }
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        (**self).tuples(store, object, relation)
    }
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        (**self).model(store, id)
    }
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        (**self).latest_model(store)
    }
}

impl<S: StoreMut + ?Sized> StoreMut for &S {
    fn put_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
        value: &[u8],
    ) -> Result<(), String> {
        (**self).put_tuple(store, object, relation, user, value)
    }
    fn delete_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<(), String> {
        (**self).delete_tuple(store, object, relation, user)
    }
    fn put_model(&self, store: &str, id: &str, model: &[u8]) -> Result<(), String> {
        (**self).put_model(store, id, model)
    }
}

type TupleMap = std::collections::BTreeMap<(String, String, String, String), Vec<u8>>;
type ModelMap = std::collections::BTreeMap<(String, String), Vec<u8>>;

/// A store in memory: for tests, and as the reference for implementations.
#[derive(Default)]
pub struct MemoryStore {
    tuples: std::cell::RefCell<TupleMap>,
    models: std::cell::RefCell<ModelMap>,
}

impl Store for MemoryStore {
    fn tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<Option<Vec<u8>>, String> {
        Ok(self
            .tuples
            .borrow()
            .get(&(store.into(), object.into(), relation.into(), user.into()))
            .cloned())
    }
    fn tuples(
        &self,
        store: &str,
        object: &str,
        relation: &str,
    ) -> Result<Vec<(String, Vec<u8>)>, String> {
        let from = (
            store.to_string(),
            object.to_string(),
            relation.to_string(),
            String::new(),
        );
        Ok(self
            .tuples
            .borrow()
            .range(from..)
            .take_while(|((s, o, r, _), _)| s == store && o == object && r == relation)
            .map(|((_, _, _, u), v)| (u.clone(), v.clone()))
            .collect())
    }
    fn model(&self, store: &str, id: &str) -> Result<Option<Vec<u8>>, String> {
        Ok(self
            .models
            .borrow()
            .get(&(store.into(), id.into()))
            .cloned())
    }
    fn latest_model(&self, store: &str) -> Result<Option<(String, Vec<u8>)>, String> {
        Ok(self
            .models
            .borrow()
            .range((store.to_string(), String::new())..)
            .take_while(|((s, _), _)| s == store)
            .last()
            .map(|((_, id), m)| (id.clone(), m.clone())))
    }
}

impl StoreMut for MemoryStore {
    fn put_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
        value: &[u8],
    ) -> Result<(), String> {
        self.tuples.borrow_mut().insert(
            (store.into(), object.into(), relation.into(), user.into()),
            value.to_vec(),
        );
        Ok(())
    }
    fn delete_tuple(
        &self,
        store: &str,
        object: &str,
        relation: &str,
        user: &str,
    ) -> Result<(), String> {
        self.tuples.borrow_mut().remove(&(
            store.into(),
            object.into(),
            relation.into(),
            user.into(),
        ));
        Ok(())
    }
    fn put_model(&self, store: &str, id: &str, model: &[u8]) -> Result<(), String> {
        self.models
            .borrow_mut()
            .insert((store.into(), id.into()), model.to_vec());
        Ok(())
    }
}
