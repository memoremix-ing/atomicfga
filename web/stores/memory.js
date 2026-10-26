// A store adapter in JavaScript memory: the reference the others are
// compared with. Aborting restores the state from the transaction's start.

import { encodeModel, encodeTuples } from "../atomicfga.js";

const key = (...parts) => parts.join("\u0000");

export class MemoryStore {
  #tuples = new Map();
  #models = new Map();
  #tx = null; // null, or { write, saved: [tuples, models] } for an abort
  #closed = false;

  #need(write) {
    if (this.#closed) throw new Error("the store is closed");
    if (!this.#tx) throw new Error("no transaction is open");
    if (write && !this.#tx.write) throw new Error("no write transaction is open");
  }

  begin(write) {
    if (this.#closed) throw new Error("the store is closed");
    if (this.#tx) throw new Error("a transaction is already open");
    // Values are never mutated in place, so copying the maps is a snapshot.
    this.#tx = { write: !!write, saved: write ? [new Map(this.#tuples), new Map(this.#models)] : null };
  }
  commit() {
    this.#need(false);
    this.#tx = null;
  }
  abort() {
    this.#need(false);
    if (this.#tx.saved) [this.#tuples, this.#models] = this.#tx.saved;
    this.#tx = null;
  }
  /** Discards an open transaction; everything fails afterwards. */
  close() {
    if (this.#tx) this.abort();
    this.#closed = true;
  }
  tupleGet(store, object, relation, user) {
    this.#need(false);
    return this.#tuples.get(key(store, object, relation, user))?.slice() ?? null;
  }
  tupleScan(store, object, relation) {
    this.#need(false);
    const prefix = key(store, object, relation, "");
    const rows = [];
    for (const [k, v] of this.#tuples) if (k.startsWith(prefix)) rows.push([k.slice(prefix.length), v]);
    rows.sort((a, b) => (a[0] < b[0] ? -1 : 1));
    return encodeTuples(rows);
  }
  tuplePut(store, object, relation, user, value) {
    this.#need(true);
    this.#tuples.set(key(store, object, relation, user), value.slice());
  }
  tupleDelete(store, object, relation, user) {
    this.#need(true);
    this.#tuples.delete(key(store, object, relation, user));
  }
  modelGet(store, id) {
    this.#need(false);
    return this.#models.get(key(store, id))?.slice() ?? null;
  }
  modelLatest(store) {
    this.#need(false);
    let latest = null;
    for (const [k, v] of this.#models) {
      const [s, id] = k.split("\u0000");
      if (s === store && (latest === null || id > latest[0])) latest = [id, v];
    }
    return latest && encodeModel(latest[0], latest[1]);
  }
  modelPut(store, id, model) {
    this.#need(true);
    this.#models.set(key(store, id), model.slice());
  }
}
