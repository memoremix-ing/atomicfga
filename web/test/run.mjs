// Runs the store adapter tests in Chromium, Firefox and WebKit (Playwright's
// cached browsers), each on a persistent profile: a fresh run; then, after
// quitting and relaunching the browser on the same profile, a check that
// the OPFS stores kept what was committed; then timings. Exits non-zero on
// any mismatch.
//
//   node web/test/run.mjs [chromium firefox webkit]
import { chromium, firefox, webkit } from "playwright-core";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { serve } from "./server.mjs";

const engines = { chromium, firefox, webkit };
const names = process.argv.slice(2).length ? process.argv.slice(2) : Object.keys(engines);
const server = await serve();
const base = `http://127.0.0.1:${server.address().port}/web/test/index.html`;
let failed = false;

async function phase(page, name) {
  await page.goto(`${base}?phase=${name}`);
  await page.waitForFunction(() => window.result !== undefined, null, { timeout: 600_000 });
  const r = await page.evaluate(() => window.result);
  if (!r.ok) throw new Error(r.error);
  return r.result;
}

async function session(name, profile, f) {
  const context = await engines[name].launchPersistentContext(profile);
  try {
    const page = await context.newPage();
    page.on("console", (m) => m.type() === "error" && console.error(`[${name}] ${m.text()}`));
    return await f(page);
  } finally {
    await context.close();
  }
}

for (const name of names) {
  const profile = fs.mkdtempSync(path.join(os.tmpdir(), `atomicfga-${name}-`));
  try {
    const fresh = await session(name, profile, (page) => phase(page, "fresh"));
    // A new browser process on the same profile.
    const reopen = await session(name, profile, (page) => phase(page, "reopen"));
    const perf = await session(name, profile, (page) => phase(page, "perf"));
    console.log(`\n== ${name}: ${fresh.userAgent}`);
    console.log(`workload: ${JSON.stringify(fresh.workload)}; outcomes ${JSON.stringify(fresh.outcomeKinds)}`);
    for (const r of [...fresh.results, ...reopen.results.map((r) => ({ ...r, name: r.name + " (browser restarted)" }))]) {
      console.log(`${r.mismatches ? "FAIL" : "ok  "} ${r.name}: ${r.outcomes} outcomes, ${r.mismatches} mismatches${r.first ? " first " + JSON.stringify(r.first) : ""}`);
      failed ||= r.mismatches > 0;
    }
    for (const r of perf.results) console.log(`perf ${r.name}: load ${r.tuples} tuples ${r.loadMs} ms; checks ${JSON.stringify(r.checks)}${r.fileBytes ? `; file ${r.fileBytes} bytes` : ""}`);
  } catch (e) {
    failed = true;
    console.error(`[${name}] ${e.stack ?? e}`);
  } finally {
    fs.rmSync(profile, { recursive: true, force: true });
  }
}
server.close();
process.exit(failed ? 1 : 0);
