// UI smoke test: runs ui.html's script against status/config JSON produced by
// the Go code and fails on anything the page cannot render.
//
//   node ui_smoke.mjs <ui.html> <fixture-dir>
//
// Each <name>.json in fixture-dir is {"status": <GET /fefo/status>, "config":
// <GET /fefo/config>}. Every fixture is rendered once per display unit in a
// fresh node:vm context with a minimal DOM stub. A fixture fails when the
// page throws, rejects a promise, shows "Refresh failed" (or any error
// toast), asks for a key, or renders "undefined", "NaN" or "[object Object]".
// No dependencies beyond Node itself.

import fs from "node:fs";
import path from "node:path";
import vm from "node:vm";

const [htmlPath, fixtureDir] = process.argv.slice(2);
if (!htmlPath || !fixtureDir) {
  console.error("usage: node ui_smoke.mjs <ui.html> <fixture-dir>");
  process.exit(2);
}
const html = fs.readFileSync(htmlPath, "utf8");
const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
if (scripts.length !== 1) {
  console.error(`expected one inline <script> in ${htmlPath}, found ${scripts.length}`);
  process.exit(2);
}
// lineOffset keeps stack traces on ui.html's own line numbers.
const scriptStart = scripts[0].index + "<script>".length;
const source = new vm.Script(scripts[0][1], {
  filename: path.basename(htmlPath),
  lineOffset: html.slice(0, scriptStart).split("\n").length - 1,
});
const markup = html.slice(html.indexOf("<body>"), html.indexOf("<script>"));

// --- DOM stub ----------------------------------------------------------------
// Just enough of the DOM for el(), $() and the few document/window calls.

class Text {
  constructor(s) { this.nodeType = 3; this.textContent = String(s); }
}
class Element {
  constructor(tag) {
    this.nodeType = 1;
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.attributes = {};
    this.listeners = {};
    this.style = {};
    this.dataset = {};
    this.className = "";
    this.id = "";
    this.value = "";
    this.checked = this.disabled = this.hidden = this.open = false;
  }
  get classList() {
    const get = () => new Set(this.className.split(/\s+/).filter(Boolean));
    const put = (s) => { this.className = [...s].join(" "); };
    return {
      contains: (c) => get().has(c),
      add: (...cs) => { const s = get(); cs.forEach((c) => s.add(c)); put(s); },
      remove: (...cs) => { const s = get(); cs.forEach((c) => s.delete(c)); put(s); },
      toggle: (c, force) => {
        const s = get(), on = force === undefined ? !s.has(c) : !!force;
        on ? s.add(c) : s.delete(c); put(s); return on;
      },
    };
  }
  append(...kids) {
    for (const k of kids) this.children.push(k && k.nodeType ? k : new Text(k));
  }
  replaceChildren(...kids) { this.children = []; this.append(...kids); }
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this.children = v == null || v === "" ? [] : [new Text(v)]; }
  set innerHTML(v) { this.children = [new Text(String(v).replace(/<[^>]*>/g, ""))]; }
  setAttribute(k, v) { this.attributes[k] = String(v); }
  getAttribute(k) { return k in this.attributes ? this.attributes[k] : null; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  click() {
    const ev = { target: this, preventDefault() {} };
    this.onclick?.(ev);
    for (const fn of this.listeners.click || []) fn(ev);
  }
}

// makeDocument builds the elements the page looks up by id, the tab buttons
// and the sections from the page's own markup.
function makeDocument() {
  const byId = new Map(), tabs = [], sections = [];
  for (const m of markup.matchAll(/<(\w+)([^>]*)>/g)) {
    const [, tag, attrs] = m;
    const id = /\bid="([^"]+)"/.exec(attrs)?.[1];
    const tab = /\bdata-tab="([^"]+)"/.exec(attrs)?.[1];
    if (!id && !tab) continue;
    const n = new Element(tag);
    if (id) { n.id = id; byId.set(id, n); }
    if (/\bchecked\b/.test(attrs)) n.checked = true;
    if (/\bhidden\b/.test(attrs)) n.hidden = true;
    if (tag === "section") sections.push(n);
    if (tab) { n.dataset.tab = tab; tabs.push(n); }
  }
  return {
    byId,
    getElementById: (id) => byId.get(id) ?? null,
    createElement: (tag) => new Element(tag),
    querySelectorAll(sel) {
      if (sel === "#tabs button") return tabs;
      if (sel === "main section") return sections;
      throw new Error("ui_smoke: unsupported selector " + sel);
    },
    querySelector: () => null,
    documentElement: { dataset: {} },
  };
}

// Text that only appears when a field the page formats is missing.
const BAD_TEXT = /\bundefined\b|\bNaN\b|\[object Object\]/;
function scanText(node, where, out) {
  const label = node.id ? `#${node.id}` : where;
  if (node.nodeType === 3) {
    if (BAD_TEXT.test(node.textContent)) out.push(`${label}: text ${JSON.stringify(node.textContent)}`);
    return;
  }
  for (const [k, v] of Object.entries(node.attributes)) {
    if (BAD_TEXT.test(v)) out.push(`${label}: <${node.tagName.toLowerCase()} ${k}=${JSON.stringify(v)}>`);
  }
  if (typeof node.value === "string" && BAD_TEXT.test(node.value)) out.push(`${label}: value ${JSON.stringify(node.value)}`);
  for (const c of node.children) scanText(c, label, out);
}

// --- runner ------------------------------------------------------------------

const tick = () => new Promise((r) => setImmediate(r));
let current = "";
const rejections = [];
process.on("unhandledRejection", (e) => rejections.push({ where: current, e }));

const fmt = (e) => (e && e.stack) || String(e);

async function run(name, fixture, unit) {
  current = `${name} [${unit}]`;
  const problems = [];
  const document = makeDocument();
  const API = "/v0/management/fefo";
  const respond = (status, data) => ({
    ok: status >= 200 && status < 300, status, statusText: String(status),
    json: async () => structuredClone(data), text: async () => JSON.stringify(data),
  });
  const ctx = {
    document,
    console,
    location: { host: "fefo.test" },
    localStorage: { getItem: (k) => (k === "fefo-router:units" ? unit : null), setItem() {}, removeItem() {} },
    sessionStorage: { getItem: (k) => (k === "fefo-router:management-key" ? "smoke-key" : null), setItem() {}, removeItem() {} },
    matchMedia: () => ({ matches: false }),
    setInterval: () => 0, clearInterval() {}, setTimeout: () => 0, clearTimeout() {},
    prompt: () => null, confirm: () => false,
    addEventListener() {}, removeEventListener() {},
    atob, btoa, TextEncoder, TextDecoder, structuredClone, URL,
    fetch: async (url, opts = {}) => {
      if ((opts.method || "GET") !== "GET") return respond(405, { error: "ui_smoke: unexpected " + opts.method + " " + url });
      const p = String(url).split("?")[0];
      if (p === API + "/status") return respond(200, fixture.status);
      if (p === API + "/config") return respond(200, fixture.config);
      return respond(404, { error: "ui_smoke: no fixture for " + url });
    },
  };
  ctx.window = ctx;
  vm.createContext(ctx);
  const call = (code) => vm.runInContext(code, ctx, { filename: `ui_smoke:${current}` });

  try {
    source.runInContext(ctx);
    await tick(); await tick(); // let the page's initial refresh(true) settle
  } catch (e) {
    problems.push("loading the page threw:\n" + fmt(e));
  }
  const toast = document.getElementById("toast");
  if (toast.textContent) problems.push(`page reported: ${toast.textContent}`);
  if (!document.getElementById("login").hidden) problems.push(`page asked for a key: ${document.getElementById("loginErr").textContent}`);

  // Render every view directly, outside refresh's catch, so a failure keeps
  // its stack. status and cfgView were set by the page's own refresh.
  const steps = [
    "renderOverview()",
    "renderTraffic()",
    "renderOverrides()",
    "renderConfig(true)",
    "renderUnknown()",
    "renderBackups()",
    // Filters on the decisions table.
    `for (const [id, v] of [["fClient", "p"], ["fAccount", "@"], ["fOutcome", "selected"], ["fRole", "own"]]) $(id).value = v; renderDecisions();`,
    `for (const id of ["fClient", "fAccount", "fOutcome", "fRole"]) $(id).value = ""; renderDecisions();`,
    // Tab switching.
    `for (const b of document.querySelectorAll("#tabs button")) b.click();`,
  ];
  let loaded = false;
  try { loaded = call("status !== null && cfgView !== null"); } catch {} // script failed before declaring them
  if (!loaded) problems.push("refresh did not load status and config");
  else {
    for (const s of steps) {
      try { call(s); } catch (e) { problems.push(`${s} threw:\n${fmt(e)}`); }
    }
    try { await call("refresh(true)"); } catch (e) { problems.push("refresh(true) threw:\n" + fmt(e)); }
    await tick();
    if (toast.textContent && !problems.includes(`page reported: ${toast.textContent}`)) problems.push(`page reported: ${toast.textContent}`);
  }
  const bad = [];
  for (const n of document.byId.values()) if (n !== toast) scanText(n, "", bad);
  if (bad.length) problems.push("rendered missing values:\n  " + [...new Set(bad)].slice(0, 20).join("\n  "));
  for (const r of rejections.splice(0)) problems.push(`unhandled rejection (${r.where}):\n${fmt(r.e)}`);
  return problems;
}

const files = fs.readdirSync(fixtureDir).filter((f) => f.endsWith(".json")).sort();
if (!files.length) {
  console.error(`no fixtures in ${fixtureDir}`);
  process.exit(2);
}
let failed = 0;
for (const f of files) {
  const name = path.basename(f, ".json");
  const fixture = JSON.parse(fs.readFileSync(path.join(fixtureDir, f), "utf8"));
  for (const unit of ["usd", "tokens"]) {
    const problems = await run(name, fixture, unit);
    if (problems.length) {
      failed++;
      console.log(`FAIL ${name} [${unit}]\n  ${problems.join("\n").replaceAll("\n", "\n  ")}`);
    } else {
      console.log(`ok   ${name} [${unit}]`);
    }
  }
}
if (failed) {
  console.log(`${failed} render(s) failed`);
  process.exit(1);
}
