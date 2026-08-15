// The drift guard for the console's click/change surface.
//
// Every control in the console names an action with data-act (click) or data-change (change), and
// app.js's ACTIONS table says what each one does. Nothing is bound inline, so a renamed control or
// a removed table entry produces a button that looks completely normal and does nothing at all —
// silently, with no console error. That is the exact failure mode TODO items 72 and 73 are lists
// of, each one found by clicking it.
//
// This reads the two files as text rather than importing them: app.js is the DOM half of the
// console and cannot be loaded under node. Comparing the sets is enough, and it is what makes the
// invariant cheap enough to hold.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const webui = join(dirname(fileURLToPath(import.meta.url)), "..", "webui");
const html = readFileSync(join(webui, "index.html"), "utf8");
const app = readFileSync(join(webui, "app.js"), "utf8");

// Comments are stripped before the inline-attribute checks below: this repo documents its decisions
// in prose beside the code, and a comment explaining WHY there are no inline styles would otherwise
// fail the test asserting there are none.
function stripComments(text, kind) {
  const withoutBlocks = text
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/<!--[\s\S]*?-->/g, "");

  return kind === "js"
    ? withoutBlocks.replace(/^\s*\/\/.*$/gm, "")
    : withoutBlocks;
}

const matchAll = (text, re) => [...text.matchAll(re)].map((m) => m[1]);

// Actions referenced by a control: the static markup, plus the templates app.js renders into
// innerHTML (table rows, link lists, the forgotten log).
const referenced = new Set([
  ...matchAll(html, /data-act="([a-z-]+)"/g),
  ...matchAll(html, /data-change="([a-z-]+)"/g),
  ...matchAll(app, /data-act="([a-z-]+)"/g),
  ...matchAll(app, /data-change="([a-z-]+)"/g),
]);

// Keys of the ACTIONS object literal in app.js.
function declaredActions() {
  const start = app.indexOf("const ACTIONS = {");

  assert.notEqual(start, -1, "ACTIONS table not found in app.js");

  const end = app.indexOf("\n};", start);

  assert.notEqual(end, -1, "ACTIONS table is not closed");

  const body = app.slice(start, end);

  // Keys are at one level of indentation: `  "name": ...` or `  name: ...`.
  return new Set(matchAll(body, /^ {2}"?([a-z][a-z-]*)"?:/gm));
}

test("every control's action has a handler", () => {
  const declared = declaredActions();
  const missing = [...referenced].filter((a) => !declared.has(a)).sort();

  assert.deepEqual(
    missing,
    [],
    `these controls would be inert: ${missing.join(", ")}`,
  );
});

test("every handler is reachable from a control", () => {
  const declared = declaredActions();
  const orphaned = [...declared].filter((a) => !referenced.has(a)).sort();

  assert.deepEqual(
    orphaned,
    [],
    `these handlers are unreachable, so a control was renamed or removed: ${orphaned.join(", ")}`,
  );
});

test("the surface is not empty, so a broken parse cannot pass vacuously", () => {
  assert.ok(referenced.size > 25, `only found ${referenced.size} actions`);
  assert.ok(declaredActions().size > 25);
});

// A <select> emits a click when its dropdown is merely opened. If one carried data-act, the click
// listener would re-run its query every time the user looked at the control — a wasted round trip
// per glance, and a visibly flickering table.
test("no select carries data-act; the change controls use data-change", () => {
  const selects = [...html.matchAll(/<select\b[^>]*>/g)].map((m) => m[0]);
  const offenders = selects.filter((s) => s.includes("data-act="));

  assert.deepEqual(
    offenders,
    [],
    "a select must use data-change, not data-act",
  );

  assert.ok(
    selects.some((s) => s.includes("data-change=")),
    "expected at least one select to re-query on change",
  );
});

// The split exists so the page can be served under a CSP without unsafe-inline. An inline handler
// or style would be blocked by that policy — silently, since CSP violations are not exceptions.
test("no inline handlers or styles remain anywhere in the console", () => {
  for (const [name, text] of [
    ["index.html", stripComments(html, "html")],
    ["app.js", stripComments(app, "js")],
  ]) {
    const handlers = text.match(/\son(?:click|change|submit|input|load)="/g);

    assert.equal(handlers, null, `${name} still has inline handlers`);

    const styles = text.match(/\sstyle="/g);

    assert.equal(styles, null, `${name} still has inline style attributes`);
  }
});

// app.js is loaded as a module, so its top-level declarations are not global. Anything relying on
// them being global — an inline handler, a stray window.foo() — would break, and the module scope
// is also what lets lib.js be imported at all.
test("the console loads app.js as a module and references its assets absolutely", () => {
  assert.match(html, /<script type="module" src="\/ui\/app\.js"><\/script>/);
  assert.match(html, /<link rel="stylesheet" href="\/ui\/styles\.css" \/>/);
  assert.match(app, /from "\.\/lib\.js"/);
});

// lib.js must stay loadable without a DOM, or the suite that exercises it cannot run. Importing it
// here would only prove it parses; this checks it never reaches for the page.
test("lib.js touches neither the DOM nor the network", () => {
  const lib = readFileSync(join(webui, "lib.js"), "utf8");
  const code = lib
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");

  for (const forbidden of [
    /\bdocument\./,
    /\bwindow\./,
    /\blocalStorage\b/,
    /\bsessionStorage\b/,
    /\bfetch\s*\(/,
    /\$\(/,
  ]) {
    assert.ok(
      !forbidden.test(code),
      `lib.js must stay pure but matches ${forbidden}`,
    );
  }
});

// app.js is a module, so a lib.js function it uses without importing is a ReferenceError at the
// moment that code path runs — not at load, and not anywhere a test that only imports lib.js would
// see it. That is a genuinely silent break: the page loads, the tab renders, and one panel is empty
// with an unhandled rejection nobody is watching for. It is how the Now tab's first version shipped
// broken, so it is worth a check of its own.
//
// A linter would catch this, at the cost of a dependency tree in a Go source directory. Comparing
// two lists does not.
test("app.js imports every lib.js function it uses", () => {
  const lib = readFileSync(join(webui, "lib.js"), "utf8");
  const exported = [
    ...lib.matchAll(/^export (?:function|const) ([A-Za-z_$][\w$]*)/gm),
  ].map((m) => m[1]);

  assert.ok(exported.length > 15, "failed to parse lib.js's exports");

  const importBlock = app.match(/import \{([^}]*)\} from "\.\/lib\.js";/s);

  assert.ok(importBlock, "app.js does not import from lib.js");

  const imported = new Set(
    [...importBlock[1].matchAll(/([A-Za-z_$][\w$]*)/g)].map((m) => m[1]),
  );

  // Only the body after the import block counts, or every name would look "used" by the import.
  const body = app.slice(app.indexOf('} from "./lib.js";'));
  const missing = exported
    .filter((name) => new RegExp(`\\b${name}\\b`).test(body))
    .filter((name) => !imported.has(name))
    .sort();

  assert.deepEqual(
    missing,
    [],
    `app.js uses these without importing them, so they are ReferenceErrors at runtime: ${missing.join(", ")}`,
  );
});
