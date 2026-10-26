// The store adapter contract (see web/atomicfga.js), as one suite every
// adapter must pass: in Node (web/test/unit.mjs) and in each browser's
// worker (web/test/worker.js).
//
// `open()` returns a fresh, empty adapter. With `reopen()` (persistent
// adapters), which opens the same database again after `close()`, the suite
// also checks what survives closing.
//
// Each case returns nothing or throws; `lifecycle` returns
// [[name, error or null]].

const S = "s";
const bytes = (...xs) => new Uint8Array(xs);
const text = (b) => (b === null ? null : Array.from(b).join(","));

function check(ok, message) {
  if (!ok) throw new Error(message);
}

function same(got, want, what) {
  check(JSON.stringify(got) === JSON.stringify(want), `${what}: got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
}

function throws(f, pattern, what) {
  try {
    f();
  } catch (e) {
    check(pattern.test(String(e?.message ?? e)), `${what}: threw ${JSON.stringify(String(e?.message ?? e))}, want ${pattern}`);
    return;
  }
  throw new Error(`${what}: did not throw (want ${pattern})`);
}

const decoder = new TextDecoder();

// tupleScan's encoding, decoded and sorted: [[user, value]].
function scan(adapter, object, relation) {
  const b = adapter.tupleScan(S, object, relation);
  const view = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const rows = [];
  for (let at = 0; at < b.length; ) {
    const n = view.getUint32(at, true);
    const user = decoder.decode(b.subarray(at + 4, at + 4 + n));
    at += 4 + n;
    const m = view.getUint32(at, true);
    rows.push([user, text(b.subarray(at + 4, at + 4 + m))]);
    at += 4 + m;
  }
  return rows.sort();
}

function latest(adapter, store = S) {
  const b = adapter.modelLatest(store);
  if (b === null) return null;
  const n = new DataView(b.buffer, b.byteOffset).getUint32(0, true);
  return [decoder.decode(b.subarray(4, 4 + n)), text(b.subarray(4 + n))];
}

const get = (a, user = "user:anne") => text(a.tupleGet(S, "doc:1", "viewer", user));

// Runs `f` in a transaction, committed if it returns.
function tx(adapter, write, f) {
  adapter.begin(write);
  let r;
  try {
    r = f();
  } catch (e) {
    adapter.abort();
    throw e;
  }
  adapter.commit();
  return r;
}

const reads = (a) => ({
  tupleGet: () => a.tupleGet(S, "doc:1", "viewer", "user:anne"),
  tupleScan: () => a.tupleScan(S, "doc:1", "viewer"),
  modelGet: () => a.modelGet(S, "01HZZZ00000000000000000001"),
  modelLatest: () => a.modelLatest(S),
});
const writes = (a) => ({
  tuplePut: () => a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1)),
  tupleDelete: () => a.tupleDelete(S, "doc:1", "viewer", "user:anne"),
  modelPut: () => a.modelPut(S, "01HZZZ00000000000000000001", bytes(1)),
});

const cases = {
  "a write transaction reads its own writes, and its commit persists them"(a) {
    tx(a, true, () => {
      a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1, 2));
      a.modelPut(S, "01HZZZ00000000000000000001", bytes(7));
      same(get(a), "1,2", "own tuple");
      same(text(a.modelGet(S, "01HZZZ00000000000000000001")), "7", "own model");
    });
    tx(a, false, () => {
      same(get(a), "1,2", "committed tuple");
      same(latest(a), ["01HZZZ00000000000000000001", "7"], "committed model");
    });
  },

  "abort discards the transaction's writes and deletes"(a) {
    tx(a, true, () => a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1)));
    a.begin(true);
    a.tupleDelete(S, "doc:1", "viewer", "user:anne");
    a.tuplePut(S, "doc:1", "viewer", "user:bob", bytes(2));
    a.modelPut(S, "01HZZZ00000000000000000001", bytes(7));
    same(get(a), null, "deleted inside");
    a.abort();
    tx(a, false, () => {
      same(get(a), "1", "deleted tuple after abort");
      same(get(a, "user:bob"), null, "written tuple after abort");
      same(latest(a), null, "written model after abort");
    });
  },

  "an empty value is present, not absent"(a) {
    tx(a, true, () => a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes()));
    tx(a, false, () => {
      same(get(a), "", "empty value");
      same(scan(a, "doc:1", "viewer"), [["user:anne", ""]], "scanned empty value");
      same(get(a, "user:bob"), null, "absent tuple");
    });
  },

  "a scan returns exactly its (store, object, relation)"(a) {
    tx(a, true, () => {
      for (const [store, object, relation, user] of [
        [S, "doc:1", "viewer", "user:anne"],
        [S, "doc:1", "viewer", "user:bob"],
        [S, "doc:1", "view", "user:carl"],
        [S, "doc:1", "viewers", "user:dan"],
        [S, "doc:10", "viewer", "user:erin"],
        [S, "doc:", "1", "user:fay"],
        ["s2", "doc:1", "viewer", "user:gus"],
      ])
        a.tuplePut(store, object, relation, user, bytes(user.length));
    });
    tx(a, false, () => {
      same(scan(a, "doc:1", "viewer"), [["user:anne", "9"], ["user:bob", "8"]], "scan");
      same(scan(a, "doc:2", "viewer"), [], "empty scan");
    });
  },

  "the latest model is the greatest ID of its store"(a) {
    tx(a, true, () => {
      a.modelPut(S, "01HZZZ00000000000000000002", bytes(2));
      a.modelPut(S, "01HZZZ00000000000000000001", bytes(1));
      a.modelPut("s2", "01HZZZ00000000000000000009", bytes(9));
    });
    tx(a, false, () => {
      same(latest(a), ["01HZZZ00000000000000000002", "2"], "latest");
      same(latest(a, "s3"), null, "latest of an empty store");
    });
  },

  "stored bytes belong to the store"(a) {
    const v = bytes(1, 2);
    const m = bytes(3);
    tx(a, true, () => {
      a.tuplePut(S, "doc:1", "viewer", "user:anne", v);
      a.modelPut(S, "01HZZZ00000000000000000001", m);
      v[0] = 9;
      m[0] = 9;
    });
    tx(a, false, () => {
      a.tupleGet(S, "doc:1", "viewer", "user:anne")[0] = 8;
      a.modelGet(S, "01HZZZ00000000000000000001")[0] = 8;
      same(get(a), "1,2", "tuple after mutating the caller's arrays");
      same(text(a.modelGet(S, "01HZZZ00000000000000000001")), "3", "model after mutating the caller's arrays");
    });
  },

  "beginning while a transaction is open fails and leaves it intact"(a) {
    for (const [first, second] of [[true, true], [true, false], [false, true], [false, false]]) {
      a.begin(first);
      throws(() => a.begin(second), /already open/, `begin(${second}) inside begin(${first})`);
      if (first) a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1));
      a.commit();
    }
    tx(a, false, () => same(get(a), "1", "the first transaction's write"));
  },

  "ending without a transaction fails"(a) {
    throws(() => a.commit(), /no transaction is open/, "commit");
    throws(() => a.abort(), /no transaction is open/, "abort");
    tx(a, true, () => {});
    throws(() => a.commit(), /no transaction is open/, "a second commit");
    a.begin(false);
    a.abort();
    throws(() => a.abort(), /no transaction is open/, "a second abort");
  },

  "reads and writes need a transaction"(a) {
    for (const [name, f] of Object.entries({ ...reads(a), ...writes(a) })) throws(f, /no transaction is open/, name);
    tx(a, false, () => same(get(a), null, "after refused writes"));
  },

  "a read transaction refuses writes and stays usable"(a) {
    tx(a, true, () => a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1)));
    a.begin(false);
    for (const [name, f] of Object.entries(writes(a))) throws(f, /no write transaction is open/, name);
    same(get(a), "1", "read after refused writes");
    a.commit();
    tx(a, false, () => {
      same(get(a), "1", "tuple after refused writes");
      same(latest(a), null, "models after refused writes");
    });
  },

  "a closed adapter refuses everything"(a) {
    a.begin(true);
    a.close();
    throws(() => a.begin(false), /closed/, "begin");
    throws(() => a.commit(), /closed/, "commit");
    throws(() => a.abort(), /closed/, "abort");
    for (const [name, f] of Object.entries({ ...reads(a), ...writes(a) })) throws(f, /closed/, name);
  },
};

const persistent = {
  async "closing discards an open transaction and keeps committed ones"(a, reopen) {
    tx(a, true, () => a.tuplePut(S, "doc:1", "viewer", "user:anne", bytes(1)));
    a.begin(true);
    a.tuplePut(S, "doc:1", "viewer", "user:bob", bytes(2));
    a.close();
    const b = await reopen();
    try {
      tx(b, false, () => {
        same(get(b), "1", "committed tuple");
        same(get(b, "user:bob"), null, "uncommitted tuple");
      });
    } finally {
      b.close();
    }
  },
};

/** Runs every case on fresh adapters from `open`. */
export async function lifecycle(open, reopen) {
  const out = [];
  const all = { ...cases, ...(reopen ? persistent : {}) };
  for (const [name, f] of Object.entries(all)) {
    const a = await open();
    let error = null;
    try {
      await f(a, reopen);
    } catch (e) {
      error = String(e?.stack ?? e);
    }
    try {
      a.close(); // after the case closed it too: closing again is harmless
    } catch (e) {
      error ??= `close: ${e?.stack ?? e}`;
    }
    out.push([name, error]);
  }
  return out;
}
