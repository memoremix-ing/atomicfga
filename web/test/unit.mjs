// The JS glue's own contracts, in Node: `node --test web/test/unit.mjs`.
import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { transaction } from "../atomicfga.js";
import { MemoryStore } from "../stores/memory.js";
import { RedbStore } from "../stores/redb.js";
import { lifecycle } from "./lifecycle.js";

/** An adapter that records its lifecycle calls. */
const recorder = () => {
  const calls = [];
  return { calls, begin: (w) => calls.push(w ? "begin write" : "begin read"), commit: () => calls.push("commit"), abort: () => calls.push("abort") };
};

test("transaction commits a callback that returns and aborts one that throws", () => {
  const a = recorder();
  assert.equal(transaction(a, true, () => 1), 1);
  assert.throws(() => transaction(a, false, () => { throw new Error("boom"); }), /boom/);
  assert.deepEqual(a.calls, ["begin write", "commit", "begin read", "abort"]);
});

test("transaction refuses async callbacks before beginning", () => {
  const a = recorder();
  let ran = false;
  assert.throws(() => transaction(a, true, async () => { ran = true; }), TypeError);
  assert.deepEqual(a.calls, []);
  assert.equal(ran, false);
});

test("transaction aborts a callback that returns a promise", async () => {
  const a = recorder();
  let later;
  assert.throws(() => transaction(a, true, () => (later = Promise.resolve().then(() => a.calls.push("write")))), TypeError);
  await later;
  assert.deepEqual(a.calls, ["begin write", "abort", "write"]);
});

// The adapter contract, on the adapters Node can run (the browser tests run
// it on all of them).
const wasm = (name) => readFile(new URL(`../../rust/target/wasm32-unknown-unknown/wasm/${name}.wasm`, import.meta.url));
const redb = await wasm("atomicfga_web_redb");
const adapters = {
  "JS memory": async () => new MemoryStore(),
  "redb (memory)": () => RedbStore.open(redb),
};
for (const [name, open] of Object.entries(adapters))
  for (const [c, error] of await lifecycle(open)) test(`${name}: ${c}`, () => assert.equal(error, null));
