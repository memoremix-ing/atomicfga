// The JS glue's own contracts, in Node: `node --test web/test/unit.mjs`.
import test from "node:test";
import assert from "node:assert/strict";
import { transaction } from "../atomicfga.js";

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
