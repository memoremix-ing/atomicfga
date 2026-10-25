// Browser tests of the store adapters, run in a dedicated worker (where OPFS
// is synchronous). Driven by web/test/run.mjs through index.html.
//
// Every backend replays one seeded workload (transactions of model writes,
// tuple writes and deletes, and checks; some transactions aborted), and its
// outcomes must equal those of the in-memory reference adapter: the same
// evaluator, so any difference is the store's. Persistence is checked
// across a page reload; timings are measured on a bulk load and on checks.

import { Evaluator, transaction } from "../atomicfga.js";
import { MemoryStore } from "../stores/memory.js";
import { RedbStore } from "../stores/redb.js";
import { SqliteStore } from "../stores/sqlite.js";
import sqlite3InitModule from "../node_modules/@sqlite.org/sqlite-wasm/dist/index.mjs";

const WASM = "/rust/target/wasm32-unknown-unknown/wasm/";
const STORE = "01HZZZSTORE000000000000000";
const REDB_FILE = "atomicfga-test.redb";

const direct = (types) => ({ directly_related_user_types: types });
const user = { type: "user" };
const member = { type: "group", relation: "member" };
const computed = (relation) => ({ computed_userset: { relation } });
const ttu = (tupleset, relation) => ({ tuple_to_userset: { tupleset: { relation: tupleset }, computed_userset: { relation } } });
const MODEL = {
  schema_version: "1.1",
  type_definitions: [
    { type: "user" },
    { type: "group", relations: { member: { this: {} } }, metadata: { relations: { member: direct([user, { type: "user", wildcard: {} }, member]) } } },
    {
      type: "folder",
      relations: { parent: { this: {} }, viewer: { union: { child: [{ this: {} }, ttu("parent", "viewer")] } } },
      metadata: { relations: { parent: direct([{ type: "folder" }]), viewer: direct([user, member]) } },
    },
    {
      type: "doc",
      relations: {
        parent: { this: {} },
        editor: { this: {} },
        blocked: { this: {} },
        viewer: { union: { child: [computed("editor"), ttu("parent", "viewer")] } },
        can_read: { difference: { base: computed("viewer"), subtract: computed("blocked") } },
      },
      metadata: {
        relations: {
          parent: direct([{ type: "folder" }]),
          editor: direct([user, { type: "user", condition: "level_ok" }]),
          blocked: direct([user, member]),
        },
      },
    },
  ],
  conditions: {
    level_ok: {
      name: "level_ok",
      expression: "level >= required",
      parameters: { level: { type_name: "TYPE_NAME_INT" }, required: { type_name: "TYPE_NAME_INT" } },
    },
  },
};

// mulberry32
function rng(seed) {
  return () => {
    seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const USERS = ["user:anne", "user:bob", "user:carl", "user:*", "group:eng#member", "group:ops#member"];
const RELATIONS = { group: ["member"], folder: ["parent", "viewer"], doc: ["parent", "editor", "blocked", "viewer", "can_read"] };
const OBJECTS = ["group:eng", "group:ops", "folder:a", "folder:b", "doc:1", "doc:2"];

function workload(seed, n) {
  const r = rng(seed);
  const pick = (xs) => xs[Math.floor(r() * xs.length)];
  const tuple = () => {
    const object = pick(OBJECTS);
    const t = { object, relation: pick(RELATIONS[object.split(":")[0]]), user: pick(object.startsWith("folder") || object.startsWith("doc") ? [...USERS, "folder:a", "folder:b"] : USERS) };
    if (r() < 0.15) t.condition = { name: "level_ok", context: r() < 0.7 ? { required: Math.floor(r() * 3) } : {} };
    return t;
  };
  const txs = [];
  const models = ["01HZZZ00000000000000000001", "01HZZZ00000000000000000002"];
  txs.push({ abort: false, ops: [{ kind: "model", id: models[0] }] });
  for (let i = 0; i < n; i++) {
    const ops = [];
    for (let j = 0; j < 1 + Math.floor(r() * 4); j++) {
      const x = r();
      if (x < 0.4) {
        const writes = Array.from({ length: 1 + Math.floor(r() * 3) }, tuple);
        const req = { writes: { tuple_keys: writes } };
        if (r() < 0.15) req.writes.on_duplicate = "ignore";
        if (r() < 0.2) req.deletes = { tuple_keys: [tuple()].map(({ object, relation, user }) => ({ object, relation, user })) };
        ops.push({ kind: "write", req });
      } else if (x < 0.5) {
        ops.push({ kind: "write", req: { deletes: { tuple_keys: [tuple()].map(({ object, relation, user }) => ({ object, relation, user })) } } });
      } else if (x < 0.52 && i > n / 2) {
        ops.push({ kind: "model", id: models[1] });
      } else {
        const object = pick(OBJECTS);
        const req = { tuple_key: { object, relation: pick(RELATIONS[object.split(":")[0]]), user: pick(USERS.filter((u) => u !== "user:*")) } };
        if (r() < 0.3) req.context = { level: Math.floor(r() * 3) };
        if (r() < 0.1) req.contextual_tuples = { tuple_keys: [tuple()] };
        ops.push({ kind: "check", req, canonical: r() < 0.3 });
      }
    }
    txs.push({ abort: r() < 0.2, ops });
  }
  return txs;
}

// Runs the workload; returns every outcome, as JSON text.
function replay(evaluator, adapter, txs) {
  const out = [];
  for (const tx of txs) {
    const write = tx.ops.some((o) => o.kind !== "check");
    adapter.begin(write);
    try {
      for (const op of tx.ops) {
        let r;
        if (op.kind === "model") r = evaluator.writeModel(adapter, STORE, op.id, MODEL);
        else if (op.kind === "write") r = evaluator.write(adapter, STORE, op.req);
        else r = evaluator.check(adapter, STORE, op.req, { canonical: op.canonical });
        delete r.reads_total; // how many reads, not what they answered
        out.push(JSON.stringify(r));
      }
    } catch (e) {
      adapter.abort();
      throw e;
    }
    if (tx.abort) adapter.abort();
    else adapter.commit();
  }
  return out;
}

// Every check of the workload's universe, in one read transaction.
function probe(evaluator, adapter) {
  return transaction(adapter, false, () => {
    const out = [];
    for (const object of OBJECTS)
      for (const relation of RELATIONS[object.split(":")[0]])
        for (const user of USERS.filter((u) => u !== "user:*")) {
          const r = evaluator.check(adapter, STORE, { tuple_key: { object, relation, user }, context: { level: 2 } });
          delete r.reads_total;
          out.push(`${object}#${relation}@${user}: ${JSON.stringify(r)}`);
        }
    return out;
  });
}

async function wipe() {
  const root = await navigator.storage.getDirectory();
  try {
    await root.removeEntry(REDB_FILE);
  } catch {}
  await SqliteStore.wipe(sqlite3InitModule);
}

async function backends(evaluatorBytes) {
  return {
    "redb (memory)": () => RedbStore.open(WASM + "atomicfga_web_redb.wasm"),
    "redb (OPFS)": () => RedbStore.open(WASM + "atomicfga_web_redb.wasm", { opfsFile: REDB_FILE }),
    "SQLite (OPFS)": () => SqliteStore.open(sqlite3InitModule),
  };
}

function compare(name, got, want) {
  const diffs = [];
  for (let i = 0; i < Math.max(got.length, want.length); i++) if (got[i] !== want[i]) diffs.push({ i, got: got[i], want: want[i] });
  return { name, outcomes: want.length, mismatches: diffs.length, first: diffs[0] ?? null };
}

function ms(f) {
  const t0 = performance.now();
  const r = f();
  return [performance.now() - t0, r];
}

// A tree of folders (100 under each of 10), each with viewers, and docs
// under them: `tuples` tuples in batches of 100.
function bulk(evaluator, adapter, tuples) {
  const keys = [];
  for (let i = 0; keys.length < tuples; i++) {
    const folder = `folder:f${i}`;
    if (i > 0) keys.push({ object: folder, relation: "parent", user: `folder:f${Math.floor((i - 1) / 10)}` });
    keys.push({ object: folder, relation: "viewer", user: `user:u${i % 500}` });
    keys.push({ object: `doc:d${i}`, relation: "parent", user: folder });
    keys.push({ object: `doc:d${i}`, relation: "editor", user: `user:u${(i * 7) % 500}` });
  }
  keys.length = tuples;
  return ms(() => {
    for (let i = 0; i < keys.length; i += 100)
      transaction(adapter, true, () => {
        const r = evaluator.write(adapter, STORE, { writes: { tuple_keys: keys.slice(i, i + 100) } });
        if (r.error) throw new Error(JSON.stringify(r.error));
      });
  })[0];
}

async function run(phase) {
  const evaluator = await Evaluator.load(WASM + "atomicfga_web.wasm");
  const result = { phase, userAgent: navigator.userAgent, results: [] };
  const txs = workload(1, 120);
  const reference = new MemoryStore();
  const want = replay(evaluator, reference, txs);
  const wantProbe = probe(evaluator, reference);
  result.workload = { transactions: txs.length, outcomes: want.length, aborted: txs.filter((t) => t.abort).length };
  result.outcomeKinds = want.reduce((m, o) => ((m[o.slice(0, 18)] = (m[o.slice(0, 18)] ?? 0) + 1), m), {});

  if (phase === "fresh") {
    await wipe();
    for (const [name, open] of Object.entries(await backends())) {
      const adapter = await open();
      try {
        result.results.push(compare(name + ": workload", replay(evaluator, adapter, txs), want));
        result.results.push(compare(name + ": state after", probe(evaluator, adapter), wantProbe));
      } finally {
        adapter.close();
      }
    }
  } else if (phase === "reopen") {
    // A new browser process: the OPFS stores must hold what the "fresh"
    // phase committed.
    for (const [name, open] of Object.entries(await backends())) {
      if (name.includes("memory")) continue;
      const adapter = await open();
      try {
        result.results.push(compare(name + ": state", probe(evaluator, adapter), wantProbe));
      } finally {
        adapter.close();
      }
    }
  } else if (phase === "perf") {
    await wipe();
    const opens = { "JS memory (reference)": async () => new MemoryStore(), ...(await backends()) };
    for (const [name, open] of Object.entries(opens)) {
      const adapter = await open();
      try {
        transaction(adapter, true, () => evaluator.writeModel(adapter, STORE, "01HZZZ00000000000000000001", MODEL));
        const loadMs = bulk(evaluator, adapter, 10000);
        const checks = [
          ["direct", { tuple_key: { object: "doc:d5", relation: "editor", user: "user:u35" } }],
          ["denied", { tuple_key: { object: "doc:d5", relation: "editor", user: "user:nobody" } }],
          ["parent chain (depth 3)", { tuple_key: { object: "doc:d555", relation: "viewer", user: "user:u5" } }],
          ["exclusion", { tuple_key: { object: "doc:d77", relation: "can_read", user: "user:u77" } }],
        ];
        const timings = {};
        transaction(adapter, false, () => {
          for (const [label, req] of checks) {
            for (let i = 0; i < 20; i++) evaluator.check(adapter, STORE, req);
            const n = 300;
            const [t, r] = ms(() => {
              let last;
              for (let i = 0; i < n; i++) last = evaluator.check(adapter, STORE, req);
              return last;
            });
            timings[label] = { us: Math.round((1000 * t) / n), allowed: r.allowed ?? r.error ?? r.refused, reads: r.reads_total };
          }
        });
        let bytes = null;
        if (name === "redb (OPFS)") bytes = (await (await (await navigator.storage.getDirectory()).getFileHandle(REDB_FILE)).getFile()).size;
        result.results.push({ name, loadMs: Math.round(loadMs), tuples: 10000, checks: timings, fileBytes: bytes });
      } finally {
        adapter.close?.();
      }
    }
  }
  return result;
}

self.onmessage = async (e) => {
  try {
    self.postMessage({ ok: true, result: await run(e.data.phase) });
  } catch (err) {
    self.postMessage({ ok: false, error: String(err?.stack ?? err) });
  }
};
