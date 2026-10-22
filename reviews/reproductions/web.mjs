// Run with: node --test reviews/reproductions/web.mjs
// These probes intentionally fail while the reported defects remain.
import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { Evaluator, transaction } from "../../web/atomicfga.js";
import { MemoryStore } from "../../web/stores/memory.js";

const wasm = await readFile(new URL("../../rust/target/wasm32-unknown-unknown/wasm/atomicfga_web.wasm", import.meta.url));
const id = "01ARZ3NDEKTSV4RRFFQ69G5FAV";
const model = {
  schema_version: "1.1", type_definitions: [{ type: "user" }, {
    type: "doc", relations: { viewer: { this: {} } },
    metadata: { relations: { viewer: { directly_related_user_types: [{ type: "user" }] } } },
  }],
};
const tuple = (user) => ({ object: "doc:1", relation: "viewer", user });

test("async transaction callback must be rejected before committing", async () => {
  const calls = [];
  const adapter = { begin() { calls.push("begin"); }, commit() { calls.push("commit"); }, abort() { calls.push("abort"); } };
  await transaction(adapter, true, async () => { await Promise.resolve(); calls.push("write"); });
  assert.notDeepEqual(calls, ["begin", "commit", "write"], "commit ran before callback's write");
});

test("storage failure must not commit a partial API write through transaction()", async () => {
  class FailingStore extends MemoryStore {
    writes = 0;
    tuplePut(...args) {
      if (++this.writes === 2) throw new Error("injected disk failure");
      super.tuplePut(...args);
    }
  }
  const e = await Evaluator.load(wasm); const s = new FailingStore();
  transaction(s, true, () => e.writeModel(s, "s", id, model));
  const result = transaction(s, true, () => e.write(s, "s", { writes: { tuple_keys: [tuple("user:anne"), tuple("user:bob")] } }));
  assert.equal(result.error?.code, "internal_error");
  assert.equal(s.tupleGet("s", "doc:1", "viewer", "user:anne"), null, "first tuple committed despite second tuple's storage error");
});

test("reference adapter must reject writes in a read transaction", () => {
  const s = new MemoryStore(); s.begin(false);
  assert.throws(() => s.tuplePut("s", "doc:1", "viewer", "user:anne", new Uint8Array()), /write/i);
});

test("nested redb write transaction must return a controlled error", () => {
  const source = `
    import { readFile } from 'node:fs/promises';
    import { RedbStore } from ${JSON.stringify(new URL("../../web/stores/redb.js", import.meta.url).href)};
    const bytes = await readFile(${JSON.stringify(new URL("../../rust/target/wasm32-unknown-unknown/wasm/atomicfga_web_redb.wasm", import.meta.url).pathname)});
    const store = await RedbStore.open(bytes);
    store.begin(true);
    try { store.begin(true); console.log('accepted nested transaction'); }
    catch (e) { console.log(e.message); }
  `;
  const r = spawnSync(process.execPath, ["--input-type=module", "-e", source], { timeout: 2000, encoding: "utf8" });
  assert.match(r.stdout ?? "", /a transaction is already open/, `nested begin trapped or hung: error=${r.error}; stdout=${r.stdout}; stderr=${r.stderr}`);
});
