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

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export class Evaluator {
  #exports;
  #adapter = null;
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

  #imports() {
    const t = (p, l) => this.#text(p, l);
    const a = () => {
      if (!this.#adapter) throw new Error("no store adapter is bound");
      return this.#adapter;
    };
    return {
      tuple_get: (s, sl, o, ol, r, rl, u, ul) => this.#call(() => this.#result(a().tupleGet(t(s, sl), t(o, ol), t(r, rl), t(u, ul)))),
      tuple_scan: (s, sl, o, ol, r, rl) => this.#call(() => this.#result(a().tupleScan(t(s, sl), t(o, ol), t(r, rl)))),
      tuple_put: (s, sl, o, ol, r, rl, u, ul, v, vl) =>
        this.#call(() => {
          a().tuplePut(t(s, sl), t(o, ol), t(r, rl), t(u, ul), this.#memory().slice(v, v + vl));
          return this.#result(new Uint8Array(0));
        }),
      tuple_delete: (s, sl, o, ol, r, rl, u, ul) =>
        this.#call(() => {
          a().tupleDelete(t(s, sl), t(o, ol), t(r, rl), t(u, ul));
          return this.#result(new Uint8Array(0));
        }),
      model_get: (s, sl, i, il) => this.#call(() => this.#result(a().modelGet(t(s, sl), t(i, il)))),
      model_latest: (s, sl) => this.#call(() => this.#result(a().modelLatest(t(s, sl)))),
      model_put: (s, sl, i, il, m, ml) =>
        this.#call(() => {
          a().modelPut(t(s, sl), t(i, il), this.#memory().slice(m, m + ml));
          return this.#result(new Uint8Array(0));
        }),
      fetch: (dst) => {
        this.#memory().set(this.#pending, dst);
      },
    };
  }

  // Copies arguments into the module, calls `f` with (pointer, length)
  // pairs, and decodes the JSON response.
  #invoke(args, f) {
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

  #with(adapter, f) {
    this.#adapter = adapter;
    try {
      return f();
    } finally {
      this.#adapter = null;
    }
  }

  static #json(x) {
    return typeof x === "string" ? x : JSON.stringify(x);
  }

  /** Sets OpenFGA's `inline_expressions` flag. */
  setInlineExpressions(on) {
    this.#exports.set_inline_expressions(on ? 1 : 0);
  }

  /** WriteAuthorizationModel under the given ID (a ULID), in the adapter's open write transaction. */
  writeModel(adapter, store, id, request) {
    return this.#with(adapter, () => this.#invoke([store, id, Evaluator.#json(request)], (...a) => this.#exports.write_model(...a)));
  }

  /** Write, in the adapter's open write transaction. */
  write(adapter, store, request) {
    return this.#with(adapter, () => this.#invoke([store, Evaluator.#json(request)], (...a) => this.#exports.write(...a)));
  }

  /** Check, in the adapter's open transaction. `canonical` selects the canonical semantics. */
  check(adapter, store, request, { canonical = false } = {}) {
    return this.#with(adapter, () => this.#invoke([store, Evaluator.#json(request)], (...a) => this.#exports.check(...a, canonical ? 1 : 0)));
  }

  /** The model compiler's verdict on a WriteAuthorizationModelRequest. */
  compile(request, { inlineExpressions = false } = {}) {
    return this.#invoke([Evaluator.#json(request)], (...a) => this.#exports.compile(...a, inlineExpressions ? 1 : 0));
  }
}

/**
 * Runs `f` in a transaction of `adapter`: committed if `f` returns, aborted if it throws.
 * `f` must be synchronous: the transaction would otherwise commit before the work it awaits.
 */
export function transaction(adapter, write, f) {
  if (f?.constructor?.name === "AsyncFunction") throw new TypeError("transaction: the callback must be synchronous, not async");
  adapter.begin(write);
  let result;
  try {
    result = f();
    if (typeof result?.then === "function") throw new TypeError("transaction: the callback returned a promise; it must be synchronous");
  } catch (e) {
    adapter.abort();
    throw e;
  }
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
