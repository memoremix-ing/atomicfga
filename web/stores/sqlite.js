// A store adapter over SQLite compiled to WebAssembly (@sqlite.org/sqlite-wasm),
// persisted to OPFS through its "SAH pool" VFS (in a worker; no
// cross-origin isolation needed). Pass SQLite's initializer (the default
// export of @sqlite.org/sqlite-wasm), so this module imports nothing a
// worker could not resolve.

import { encodeModel, encodeTuples } from "../atomicfga.js";

const SCHEMA = `
CREATE TABLE IF NOT EXISTS tuples (
  store TEXT NOT NULL, object TEXT NOT NULL, relation TEXT NOT NULL, user TEXT NOT NULL,
  value BLOB NOT NULL,
  PRIMARY KEY (store, object, relation, user)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS models (
  store TEXT NOT NULL, id TEXT NOT NULL, model BLOB NOT NULL,
  PRIMARY KEY (store, id)
) WITHOUT ROWID;`;

const empty = new Uint8Array(0);

// The SAH pool holds its files' access handles for the worker's lifetime:
// install each pool once.
const pools = new Map();

function pool(sqlite3InitModule, name) {
  if (!pools.has(name))
    pools.set(
      name,
      sqlite3InitModule().then(async (sqlite3) => ({ sqlite3, util: await sqlite3.installOpfsSAHPoolVfs({ name }) })),
    );
  return pools.get(name);
}

export class SqliteStore {
  #db;
  #capi;
  #stmts = {};
  // null, "read", "write", or "ended": SQLite rolled the transaction back
  // itself (after some errors it does), so statements would autocommit.
  #tx = null;
  #closed = false;

  /** Opens (creating) `file` in the SAH pool `poolName`. */
  static async open(sqlite3InitModule, { file = "/atomicfga.sqlite3", poolName = "atomicfga" } = {}) {
    const store = new SqliteStore();
    const { sqlite3, util } = await pool(sqlite3InitModule, poolName);
    store.#capi = sqlite3.capi;
    store.#db = new util.OpfsSAHPoolDb(file);
    store.#db.exec(SCHEMA);
    const prepare = (name, sql) => (store.#stmts[name] = store.#db.prepare(sql));
    prepare("get", "SELECT value FROM tuples WHERE store = ? AND object = ? AND relation = ? AND user = ?");
    prepare("scan", "SELECT user, value FROM tuples WHERE store = ? AND object = ? AND relation = ?");
    prepare("put", "INSERT OR REPLACE INTO tuples (store, object, relation, user, value) VALUES (?, ?, ?, ?, coalesce(?, x''))");
    prepare("delete", "DELETE FROM tuples WHERE store = ? AND object = ? AND relation = ? AND user = ?");
    prepare("model", "SELECT model FROM models WHERE store = ? AND id = ?");
    prepare("latest", "SELECT id, model FROM models WHERE store = ? ORDER BY id DESC LIMIT 1");
    prepare("putModel", "INSERT OR REPLACE INTO models (store, id, model) VALUES (?, ?, ?)");
    return store;
  }

  /** Removes every database file of the pool. */
  static async wipe(sqlite3InitModule, { poolName = "atomicfga" } = {}) {
    await (await pool(sqlite3InitModule, poolName)).util.wipeFiles();
  }

  /** Closes the database, discarding an open transaction. */
  close() {
    if (this.#closed) return;
    this.#closed = true;
    this.#tx = null;
    Object.values(this.#stmts).forEach((s) => s.finalize());
    this.#db.close(); // SQLite rolls back an open transaction
  }

  #inTransaction() {
    return this.#capi.sqlite3_get_autocommit(this.#db.pointer) === 0;
  }

  #need(write) {
    if (this.#closed) throw new Error("the store is closed");
    if (this.#tx === "ended") throw new Error("SQLite ended the transaction after an error; abort it");
    if (!this.#tx) throw new Error("no transaction is open");
    if (write && this.#tx !== "write") throw new Error("no write transaction is open");
  }

  #run(name, params, each, write = false) {
    this.#need(write);
    const s = this.#stmts[name];
    try {
      s.bind(params);
      while (s.step()) each?.(s);
    } catch (e) {
      if (!this.#inTransaction()) this.#tx = "ended";
      throw e;
    } finally {
      s.reset(true);
    }
  }

  #blob(s, i) {
    return s.get(i) ?? empty;
  }

  begin(write) {
    if (this.#closed) throw new Error("the store is closed");
    if (this.#tx) throw new Error("a transaction is already open");
    this.#db.exec(write ? "BEGIN IMMEDIATE" : "BEGIN");
    this.#tx = write ? "write" : "read";
  }
  /** Commits. The transaction ends even if committing fails (nothing is committed then). */
  commit() {
    if (this.#end() === "ended") throw new Error("SQLite ended the transaction after an error; nothing was committed");
    try {
      this.#db.exec("COMMIT");
    } catch (e) {
      if (this.#inTransaction()) this.#db.exec("ROLLBACK");
      throw e;
    }
  }
  abort() {
    this.#end();
    if (this.#inTransaction()) this.#db.exec("ROLLBACK");
  }
  #end() {
    if (this.#closed) throw new Error("the store is closed");
    if (!this.#tx) throw new Error("no transaction is open");
    const tx = this.#tx;
    this.#tx = null;
    return tx;
  }
  tupleGet(store, object, relation, user) {
    let v = null;
    this.#run("get", [store, object, relation, user], (s) => (v = this.#blob(s, 0)));
    return v;
  }
  tupleScan(store, object, relation) {
    const rows = [];
    this.#run("scan", [store, object, relation], (s) => rows.push([s.get(0), this.#blob(s, 1)]));
    return encodeTuples(rows);
  }
  tuplePut(store, object, relation, user, value) {
    this.#run("put", [store, object, relation, user, value], null, true);
  }
  tupleDelete(store, object, relation, user) {
    this.#run("delete", [store, object, relation, user], null, true);
  }
  modelGet(store, id) {
    let v = null;
    this.#run("model", [store, id], (s) => (v = this.#blob(s, 0)));
    return v;
  }
  modelLatest(store) {
    let v = null;
    this.#run("latest", [store], (s) => (v = encodeModel(s.get(0), this.#blob(s, 1))));
    return v;
  }
  modelPut(store, id, model) {
    this.#run("putModel", [store, id, model], null, true);
  }
}
