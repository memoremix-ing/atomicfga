// The atomicfga evaluator in JavaScript hosts: loads the storage-free
// evaluator module (rust/web/evaluator) and wires its store imports to a
// store adapter.
//
// A store adapter keeps bytes and knows nothing of authorization:
//
//   begin(write: boolean), commit(), abort()
//   tupleGet(store, object, relation, user)        -> Uint8Array | null
//   tupleScan(store, object, relation)             -> Uint8Array
//       (u32 LE user length, user, u32 LE value length, value)*
//   tuplePut(store, object, relation, user, value: Uint8Array)
//   tupleDelete(store, object, relation, user)
//   modelGet(store, id)                            -> Uint8Array | null
//   modelLatest(store)                             -> Uint8Array | null
//       (u32 LE ID length, ID, model)
//   modelPut(store, id, model: Uint8Array)
//   close()
//
// Adapters throw on failure. Everything is synchronous: in a browser, run
// in a worker, where OPFS offers synchronous file access.
//
// One transaction is open at a time. begin fails while one is open (which
// stays intact); commit and abort fail when none is; both end it, commit
// even when it fails (committing nothing then). Reads need a transaction,
// writes a write transaction. Values are copied in and out: an empty value
// is present, null is absent. close discards an open transaction, may be
// repeated, and everything else fails afterwards. web/test/lifecycle.js
// checks all of this; every adapter must pass it.
//
// The evaluator never sees an adapter directly: its calls take the
// transaction that `transaction(adapter, write, (tx) => ...)` opens. If a
// store operation fails during a call, the call throws and the transaction
// cannot commit, even if the callback catches the error: a write may have
// been applied in part.

const encoder = new TextEncoder();
const decoder = new TextDecoder();

/**
 * A store operation failed during an evaluator call (or the call failed
 * unexpectedly): the transaction's state is unknown, so it rolls back. The
 * store's error is the `cause`.
 */
export class TransactionAbortedError extends Error {
  constructor(cause) {
    super(`atomicfga: a store operation failed, so the transaction rolls back: ${cause?.message ?? cause}`, { cause });
    this.name = "TransactionAbortedError";
  }
}

const MUTATORS = new Set(["tuplePut", "tupleDelete", "modelPut"]);

// What a Transaction lets the evaluator do; module-private.
class Scope {
  open = true;
  failure = null; // the TransactionAbortedError, once one occurred
  constructor(adapter, write) {
    this.adapter = adapter;
    this.write = write;
  }
  fail(e) {
    this.failure ??= e instanceof TransactionAbortedError ? e : new TransactionAbortedError(e);
    return this.failure;
  }
}

let scopeOf;

/** An open transaction of `transaction()`, for evaluator calls. It cannot be made otherwise. */
class Transaction {
  #scope;
  constructor(scope) {
    this.#scope = scope;
  }
  static {
    scopeOf = (tx) => (typeof tx === "object" && tx !== null && #scope in tx ? tx.#scope : null);
  }
}

export class Evaluator {
  #exports;
  #scope = null; // the Scope of the call in progress
  #trapped = null; // the module's trap: its state is unknown afterwards
  #pending = new Uint8Array(0);

  static async load(source) {
    const evaluator = new Evaluator();
    const bytes = source instanceof Uint8Array || source instanceof ArrayBuffer ? source : await (await fetch(source)).arrayBuffer();
    const { instance } = await WebAssembly.instantiate(bytes, { store: evaluator.#imports() });
    evaluator.#exports = instance.exports;
    return evaluator;
  }

  #memory() {
    return new Uint8Array(this.#exports.memory.buffer);
  }

  #text(ptr, len) {
    return decoder.decode(this.#memory().subarray(ptr, ptr + len));
  }

  // Leaves `bytes` for the module's `fetch`; returns what the import returns.
  #result(bytes) {
    if (bytes === null || bytes === undefined) return -1;
    this.#pending = bytes;
    return bytes.length;
  }

  #call(f) {
    try {
      return f();
    } catch (e) {
      const message = encoder.encode(String(e?.message ?? e));
      this.#pending = message;
      return -2 - message.length;
    }
  }

  // Runs a store operation of the call in progress; a failure fails its
  // transaction.
  #store(method, ...args) {
    const scope = this.#scope;
    if (!scope) throw new Error("no transaction is bound");
    try {
      if (MUTATORS.has(method) && !scope.write) throw new Error("no write transaction is open");
      return scope.adapter[method](...args);
    } catch (e) {
      scope.fail(e);
      throw e;
    }
  }

  #imports() {
    const t = (p, l) => this.#text(p, l);
    const a = (method, ...args) => this.#store(method, ...args);
    const none = new Uint8Array(0);
    return {
      tuple_get: (s, sl, o, ol, r, rl, u, ul) => this.#call(() => this.#result(a("tupleGet", t(s, sl), t(o, ol), t(r, rl), t(u, ul)))),
      tuple_scan: (s, sl, o, ol, r, rl) => this.#call(() => this.#result(a("tupleScan", t(s, sl), t(o, ol), t(r, rl)))),
      tuple_put: (s, sl, o, ol, r, rl, u, ul, v, vl) =>
        this.#call(() => (a("tuplePut", t(s, sl), t(o, ol), t(r, rl), t(u, ul), this.#memory().slice(v, v + vl)), this.#result(none))),
      tuple_delete: (s, sl, o, ol, r, rl, u, ul) =>
        this.#call(() => (a("tupleDelete", t(s, sl), t(o, ol), t(r, rl), t(u, ul)), this.#result(none))),
      model_get: (s, sl, i, il) => this.#call(() => this.#result(a("modelGet", t(s, sl), t(i, il)))),
      model_latest: (s, sl) => this.#call(() => this.#result(a("modelLatest", t(s, sl)))),
      model_put: (s, sl, i, il, m, ml) =>
        this.#call(() => (a("modelPut", t(s, sl), t(i, il), this.#memory().slice(m, m + ml)), this.#result(none))),
      fetch: (dst) => {
        this.#memory().set(this.#pending, dst);
      },
    };
  }

  // Copies arguments into the module, calls `f` with (pointer, length)
  // pairs, and decodes the JSON response.
  #invoke(args, f) {
    if (this.#trapped) throw new Error("atomicfga: the evaluator module trapped; load a new one", { cause: this.#trapped });
    try {
      return this.#pass(args, f);
    } catch (e) {
      if (e instanceof WebAssembly.RuntimeError) this.#trapped = e;
      throw e;
    }
  }

  #pass(args, f) {
    const e = this.#exports;
    const encoded = args.map((x) => (typeof x === "string" ? encoder.encode(x) : x));
    const ptrs = encoded.map((b) => {
      const p = e.alloc(b.length);
      this.#memory().set(b, p);
      return p;
    });
    try {
      const n = f(...encoded.flatMap((b, i) => [ptrs[i], b.length]));
      const out = this.#memory().slice(e.result_ptr(), e.result_ptr() + n);
      return JSON.parse(decoder.decode(out));
    } finally {
      encoded.forEach((b, i) => e.dealloc(ptrs[i], b.length));
    }
  }

  // Runs an API call in `tx`. Anything that fails it (a store operation, a
  // trap) fails the transaction too.
  #with(tx, write, f) {
    const scope = scopeOf(tx);
    if (!scope) throw new TypeError("atomicfga: pass the transaction that transaction() gives its callback");
    if (!scope.open) throw new Error("atomicfga: the transaction has ended");
    if (scope.failure) throw scope.failure;
    if (write && !scope.write) throw new TypeError("atomicfga: writing needs a write transaction");
    if (this.#scope) throw new Error("atomicfga: the evaluator is already in a call");
    this.#scope = scope;
    try {
      const r = f();
      if (scope.failure) throw scope.failure;
      return r;
    } catch (e) {
      throw scope.fail(e);
    } finally {
      this.#scope = null;
    }
  }

  static #json(x) {
    return typeof x === "string" ? x : JSON.stringify(x);
  }

  /** Sets OpenFGA's `inline_expressions` flag. */
  setInlineExpressions(on) {
    this.#exports.set_inline_expressions(on ? 1 : 0);
  }

  /**
   * WriteAuthorizationModel under the given ID (a ULID), in write transaction
   * `tx`. A new model's ID must be greater than the store's latest.
   */
  writeModel(tx, store, id, request) {
    return this.#with(tx, true, () => this.#invoke([store, id, Evaluator.#json(request)], (...a) => this.#exports.write_model(...a)));
  }

  /** Write, in write transaction `tx`. The request names its model (`authorization_model_id`). */
  write(tx, store, request) {
    return this.#with(tx, true, () => this.#invoke([store, Evaluator.#json(request)], (...a) => this.#exports.write(...a)));
  }

  /**
   * Check, in transaction `tx`. The request names its model
   * (`authorization_model_id`). `canonical` selects the canonical semantics.
   */
  check(tx, store, request, { canonical = false } = {}) {
    return this.#with(tx, false, () => this.#invoke([store, Evaluator.#json(request)], (...a) => this.#exports.check(...a, canonical ? 1 : 0)));
  }

  /** The model compiler's verdict on a WriteAuthorizationModelRequest. */
  compile(request, { inlineExpressions = false } = {}) {
    return this.#invoke([Evaluator.#json(request)], (...a) => this.#exports.compile(...a, inlineExpressions ? 1 : 0));
  }
}

/**
 * Runs `f(tx)` in a transaction of `adapter`, `tx` being what evaluator calls
 * take: committed if `f` returns, aborted if it throws. A store failure during
 * an evaluator call aborts it too (throwing a TransactionAbortedError), even
 * if `f` caught the call's error. `f` must be synchronous: the transaction
 * would otherwise commit before the work it awaits.
 */
export function transaction(adapter, write, f) {
  if (f?.constructor?.name === "AsyncFunction") throw new TypeError("transaction: the callback must be synchronous, not async");
  const scope = new Scope(adapter, !!write);
  adapter.begin(!!write);
  let result;
  try {
    result = f(new Transaction(scope));
    if (typeof result?.then === "function") throw new TypeError("transaction: the callback returned a promise; it must be synchronous");
    if (scope.failure) throw scope.failure;
  } catch (e) {
    scope.open = false;
    try {
      adapter.abort();
    } catch (a) {
      throw new AggregateError([e, a], "atomicfga: the transaction failed, and aborting it failed too");
    }
    throw e;
  }
  scope.open = false;
  adapter.commit();
  return result;
}

/** Encodes scan results as adapters return them. */
export function encodeTuples(rows) {
  const parts = [];
  let size = 0;
  for (const [user, value] of rows) {
    const u = encoder.encode(user);
    parts.push(u, value);
    size += 8 + u.length + value.length;
  }
  const out = new Uint8Array(size);
  const view = new DataView(out.buffer);
  let at = 0;
  for (let i = 0; i < parts.length; i += 2) {
    for (const b of [parts[i], parts[i + 1]]) {
      view.setUint32(at, b.length, true);
      out.set(b, at + 4);
      at += 4 + b.length;
    }
  }
  return out;
}

/** Encodes a latest model as adapters return it. */
export function encodeModel(id, model) {
  const i = encoder.encode(id);
  const out = new Uint8Array(4 + i.length + model.length);
  new DataView(out.buffer).setUint32(0, i.length, true);
  out.set(i, 4);
  out.set(model, 4 + i.length);
  return out;
}
