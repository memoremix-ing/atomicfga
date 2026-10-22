// A store adapter in JavaScript memory: the reference the others are
// compared with. Aborting restores the state from the transaction's start.

import { encodeModel, encodeTuples } from "../atomicfga.js";

const key = (...parts) => parts.join("\u0000");

export class MemoryStore {
  #tuples = new Map();
  #models = new Map();
  #saved = null;

  begin(write) {
    if (this.#saved) throw new Error("a transaction is already open");
    this.#saved = write ? [new Map(this.#tuples), new Map(this.#models)] : [this.#tuples, this.#models];
  }
  commit() {
    this.#saved = null;
  }
  abort() {
    [this.#tuples, this.#models] = this.#saved;
    this.#saved = null;
  }
  tupleGet(store, object, relation, user) {
    return this.#tuples.get(key(store, object, relation, user)) ?? null;
  }
  tupleScan(store, object, relation) {
    const prefix = key(store, object, relation, "");
    const rows = [];
    for (const [k, v] of this.#tuples) if (k.startsWith(prefix)) rows.push([k.slice(prefix.length), v]);
    rows.sort((a, b) => (a[0] < b[0] ? -1 : 1));
    return encodeTuples(rows);
  }
  tuplePut(store, object, relation, user, value) {
    this.#tuples.set(key(store, object, relation, user), value);
  }
  tupleDelete(store, object, relation, user) {
    this.#tuples.delete(key(store, object, relation, user));
  }
  modelGet(store, id) {
    return this.#models.get(key(store, id)) ?? null;
  }
  modelLatest(store) {
    let latest = null;
    for (const [k, v] of this.#models) {
      const [s, id] = k.split("\u0000");
      if (s === store && (latest === null || id > latest[0])) latest = [id, v];
    }
    return latest && encodeModel(latest[0], latest[1]);
  }
  modelPut(store, id, model) {
    this.#models.set(key(store, id), model);
  }
}
