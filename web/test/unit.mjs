// The JS glue's own contracts, in Node: `node --test web/test/unit.mjs`.
import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { Evaluator, TransactionAbortedError, transaction } from "../atomicfga.js";
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

// Evaluator calls in transaction(), on those adapters, with store failures
// injected.
const evaluator = await Evaluator.load(await wasm("atomicfga_web"));
const STORE = "01HZZZSTORE000000000000000";
const MODEL_ID = "01HZZZ00000000000000000001";
const MODEL = {
  schema_version: "1.1",
  type_definitions: [
    { type: "user" },
    { type: "doc", relations: { viewer: { this: {} } }, metadata: { relations: { viewer: { directly_related_user_types: [{ type: "user" }] } } } },
  ],
};
const viewer = (object, user) => ({ object, relation: "viewer", user });
const SEEDED = [viewer("doc:1", "user:anne"), viewer("doc:1", "user:bob")];
// Deletes both seeded tuples and writes three: five mutations.
const REWRITE = { deletes: { tuple_keys: SEEDED }, writes: { tuple_keys: [viewer("doc:2", "user:anne"), viewer("doc:2", "user:bob"), viewer("doc:2", "user:carl")] } };
const UNIVERSE = ["doc:1", "doc:2"].flatMap((d) => ["user:anne", "user:bob", "user:carl"].map((u) => viewer(d, u)));

async function seeded(open) {
  const a = await open();
  transaction(a, true, (tx) => {
    assert.deepEqual(evaluator.writeModel(tx, STORE, MODEL_ID, MODEL), { authorization_model_id: MODEL_ID });
    assert.deepEqual(evaluator.write(tx, STORE, { writes: { tuple_keys: SEEDED } }), {});
  });
  return a;
}

// Every check of the universe, committed state.
const state = (a) =>
  transaction(a, false, (tx) => UNIVERSE.map((t) => `${t.object}@${t.user}: ${evaluator.check(tx, STORE, { tuple_key: t }).allowed}`));

/** `adapter`, except that its `nth` call of a method in `methods` throws. */
function failing(adapter, methods, nth) {
  let n = 0;
  return new Proxy(adapter, {
    get(target, p) {
      const v = target[p];
      if (typeof v !== "function") return v;
      return (...args) => {
        if (methods.includes(p) && ++n === nth) throw new Error("injected store failure");
        return v.apply(target, args);
      };
    },
  });
}

const MUTATIONS = ["tuplePut", "tupleDelete", "modelPut"];
const READS = ["tupleGet", "tupleScan", "modelGet", "modelLatest"];

for (const [name, open] of Object.entries(adapters)) {
  test(`${name}: a store failure at any point of a write rolls the whole transaction back`, async () => {
    for (const [methods, positions] of [[MUTATIONS, 5], [READS, 3]])
      for (let nth = 1; nth <= positions; nth++) {
        const a = await seeded(open);
        const before = state(a);
        let returned;
        assert.throws(
          () => transaction(failing(a, methods, nth), true, (tx) => (returned = evaluator.write(tx, STORE, REWRITE))),
          (e) => e instanceof TransactionAbortedError && /injected store failure/.test(e.cause?.message),
          `failure at ${methods[0]}… call ${nth}`,
        );
        assert.equal(returned, undefined, "the call threw rather than returning its internal_error");
        assert.deepEqual(state(a), before, `state after a failure at ${methods[0]}… call ${nth}`);
        a.close();
      }
  });

  test(`${name}: a caught store failure still rolls the transaction back`, async () => {
    const a = await seeded(open);
    const before = state(a);
    const f = failing(a, MUTATIONS, 3);
    assert.throws(
      () =>
        transaction(f, true, (tx) => {
          assert.throws(() => evaluator.write(tx, STORE, REWRITE), TransactionAbortedError);
          // Later calls fail too, even ones the store would answer.
          assert.throws(() => evaluator.check(tx, STORE, { tuple_key: SEEDED[0] }), TransactionAbortedError);
          return "the callback returned";
        }),
      TransactionAbortedError,
    );
    assert.deepEqual(state(a), before);
    a.close();
  });

  test(`${name}: a failed model write rolls back`, async () => {
    const a = await open();
    assert.throws(() => transaction(failing(a, ["modelPut"], 1), true, (tx) => evaluator.writeModel(tx, STORE, MODEL_ID, MODEL)), TransactionAbortedError);
    assert.deepEqual(
      transaction(a, false, (tx) => evaluator.check(tx, STORE, { tuple_key: SEEDED[0] })).error?.code,
      "latest_authorization_model_not_found",
    );
    a.close();
  });
}

test("evaluator calls take only a live transaction of transaction()", async () => {
  const a = await seeded(adapters["JS memory"]);
  const check = (tx) => evaluator.check(tx, STORE, { tuple_key: SEEDED[0] });
  a.begin(false);
  assert.throws(() => check(a), TypeError, "a raw adapter");
  assert.throws(() => check({}), TypeError, "a lookalike");
  a.abort();
  let kept;
  assert.equal(transaction(a, false, (tx) => ((kept = tx), check(tx).allowed)), true);
  assert.throws(() => check(kept), /ended/, "a transaction that ended");
  assert.throws(() => transaction(a, false, (tx) => evaluator.write(tx, STORE, REWRITE)), TypeError, "a write in a read transaction");
  assert.deepEqual(state(a).filter((s) => s.endsWith("true")).length, 2);
  a.close();
});
