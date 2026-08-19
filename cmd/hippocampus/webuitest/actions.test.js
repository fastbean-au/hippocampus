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

// lib.js renders markup too - the deployment diagram's boxes carry data-act - so it is scanned for
// controls alongside app.js. It was not, when app.js was the only file that produced any: an action
// emitted from here would then have read as an unreachable handler, which is the guard failing in
// the one direction that teaches you to delete the handler rather than fix the scan.
const lib = readFileSync(join(webui, "lib.js"), "utf8");

// Comments are stripped before the scans below: this repo documents its decisions in prose beside
// the code, and a comment explaining WHY there are no inline styles would otherwise fail the test
// asserting there are none. Two of them do exactly that today.
//
// It is deliberately LINE-BASED, and that is the whole point of it. The obvious spelling — one
// /\/\*[\s\S]*?\*\//g over the file, which is what this was — is UNBOUNDED: a `/*` inside a string, a
// regex literal or a URL eats every line up to the next `*/`, and what it eats is code these tests
// then report as clean. A guard that can silently stop looking is the precise failure this file
// exists to catch, so the stripper must never be able to remove more than the line it matched. JS
// block comments are therefore refused outright rather than half-handled — see the test below.
//
// The HTML side has to span lines (index.html's comments do), so it is a state machine over lines
// rather than a regex over the file. Same property: it consumes what lies between the delimiters it
// actually sees, and a stray `<!--` costs the rest of that comment, not the rest of the file.
function stripComments(text, kind) {
  const lines = text.split("\n");

  if (kind === "js") {
    return lines.filter((line) => !/^\s*\/\//.test(line)).join("\n");
  }

  let inComment = false;

  return lines
    .map((line) => {
      let kept = "";
      let rest = line;

      while (rest !== "") {
        if (inComment) {
          const end = rest.indexOf("-->");

          if (end === -1) {
            break;
          }

          inComment = false;
          rest = rest.slice(end + 3);

          continue;
        }

        const start = rest.indexOf("<!--");

        if (start === -1) {
          kept += rest;

          break;
        }

        kept += rest.slice(0, start);
        inComment = true;
        rest = rest.slice(start + 4);
      }

      return kept;
    })
    .join("\n");
}

// The stripped sources. Every scan below reads these rather than the raw text, including the action
// sets: a data-act quoted in a comment would otherwise keep a handler looking reachable after its
// control was deleted, which is the guard passing in the direction that matters.
const htmlCode = stripComments(html, "html");
const appCode = stripComments(app, "js");
const libCode = stripComments(lib, "js");

const matchAll = (text, re) => [...text.matchAll(re)].map((m) => m[1]);

// Actions referenced by a control: the static markup, plus the templates app.js renders into
// innerHTML (table rows, link lists, the forgotten log).
const referenced = new Set([
  ...matchAll(htmlCode, /data-act="([a-z-]+)"/g),
  ...matchAll(htmlCode, /data-change="([a-z-]+)"/g),
  ...matchAll(appCode, /data-act="([a-z-]+)"/g),
  ...matchAll(appCode, /data-change="([a-z-]+)"/g),
  ...matchAll(libCode, /data-act="([a-z-]+)"/g),
  ...matchAll(libCode, /data-change="([a-z-]+)"/g),
]);

// Keys of the ACTIONS object literal in app.js.
function declaredActions() {
  const start = appCode.indexOf("const ACTIONS = {");

  assert.notEqual(start, -1, "ACTIONS table not found in app.js");

  const end = appCode.indexOf("\n};", start);

  assert.notEqual(end, -1, "ACTIONS table is not closed");

  const body = appCode.slice(start, end);

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
  const selects = [...htmlCode.matchAll(/<select\b[^>]*>/g)].map((m) => m[0]);
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

// The Events tab's List/Summarise sub-tabs are wired by id in three places: the button's data-mode,
// the EVENT_MODES table's key, and the pane and card ids that table names. Nothing throws if one of
// them is wrong - eventMode returns early on an unknown mode, and $() on a missing id would throw
// only once that branch ran - so the symptom is a tab that does nothing when clicked. The same
// silent shape as a missing ACTIONS entry, and worth the same check.
test("every event sub-tab is wired to a pane and a card that exist", () => {
  const table = appCode.match(/const EVENT_MODES = \{(.*?)\n\};/s);

  assert.ok(table, "EVENT_MODES table not found in app.js");

  const modes = new Map(
    [
      ...table[1].matchAll(/(\w+): \{ pane: "([\w-]+)", card: "([\w-]+)" \}/g),
    ].map((m) => [m[1], { pane: m[2], card: m[3] }]),
  );

  assert.ok(modes.size >= 2, `only parsed ${modes.size} modes`);

  const buttons = matchAll(
    htmlCode,
    /class="subtab[^"]*"[^>]*?data-mode="(\w+)"/gs,
  );

  assert.deepEqual(
    buttons.slice().sort(),
    [...modes.keys()].sort(),
    "a sub-tab button and its EVENT_MODES entry disagree",
  );

  for (const [mode, ids] of modes) {
    for (const id of [ids.pane, ids.card, "ev-mode-" + mode]) {
      assert.ok(
        htmlCode.includes(`id="${id}"`),
        `mode ${mode} names #${id}, which is not in the markup`,
      );
    }
  }
});

// The line-based stripper above cannot see a /* ... */ comment, so the console's JavaScript must not
// contain one. Refusing them is the honest half of that decision: the alternative, a regex spanning
// the file, silently deletes from any `/*` in a string or a URL to the next `*/` — and every scan
// here would then report the deleted code as clean.
//
// It also holds the convention the console is already written in: 3,800 lines of app.js and 850 of
// lib.js, and not one block comment between them.
test("the console's JavaScript carries no block comments, which the stripper cannot see", () => {
  for (const [name, text] of [
    ["app.js", app],
    ["lib.js", lib],
  ]) {
    assert.equal(
      text.includes("/*"),
      false,
      `${name} contains "/*". Use // comments: a line-based stripper is the only kind that cannot ` +
        `delete more than it matched, so a block comment here would leave these tests scanning ` +
        `less than they report on.`,
    );
  }
});

// The stripper is load-bearing enough to test directly: everything above trusts it to remove prose
// and nothing else, and the property that matters is not "it removes comments" but "it can never
// remove more than the line it matched". A regression here would not fail any test above — it would
// quietly shrink what they look at.
test("stripComments removes prose without reaching past it", () => {
  const js = [
    'const a = "keep me";',
    "// a comment",
    "  // indented too",
    "b(); // trailing",
  ].join("\n");

  assert.equal(
    stripComments(js, "js"),
    ['const a = "keep me";', "b(); // trailing"].join("\n"),
  );

  // The case the old spelling got wrong: a block-comment opener inside a string ran to the next
  // closer, taking every line between them with it.
  const trap = [
    'const glob = "/*";',
    'el.style = "x";',
    'const end = "*/";',
  ].join("\n");

  assert.equal(stripComments(trap, "js"), trap);

  const markup = [
    "<p>keep</p>",
    "<!-- one line -->",
    "<!-- opens here",
    '   style="not real"',
    "-->after<b>kept</b>",
  ].join("\n");

  assert.equal(
    stripComments(markup, "html"),
    ["<p>keep</p>", "", "", "", "after<b>kept</b>"].join("\n"),
  );
});

// The split exists so the page can be served under a CSP without unsafe-inline. An inline handler
// or style would be blocked by that policy — silently, since CSP violations are not exceptions.
test("no inline handlers or styles remain anywhere in the console", () => {
  for (const [name, text] of [
    ["index.html", htmlCode],
    ["app.js", appCode],
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
  assert.match(
    htmlCode,
    /<script type="module" src="\/ui\/app\.js"><\/script>/,
  );
  assert.match(htmlCode, /<link rel="stylesheet" href="\/ui\/styles\.css" \/>/);
  assert.match(appCode, /from "\.\/lib\.js"/);
});

// lib.js must stay loadable without a DOM, or the suite that exercises it cannot run. Importing it
// here would only prove it parses; this checks it never reaches for the page.
test("lib.js touches neither the DOM nor the network", () => {
  const code = libCode;

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
  const exported = [
    ...libCode.matchAll(/^export (?:function|const) ([A-Za-z_$][\w$]*)/gm),
  ].map((m) => m[1]);

  assert.ok(exported.length > 15, "failed to parse lib.js's exports");

  const importBlock = appCode.match(/import \{([^}]*)\} from "\.\/lib\.js";/s);

  assert.ok(importBlock, "app.js does not import from lib.js");

  const imported = new Set(
    [...importBlock[1].matchAll(/([A-Za-z_$][\w$]*)/g)].map((m) => m[1]),
  );

  // Only the body after the import block counts, or every name would look "used" by the import.
  const body = appCode.slice(appCode.indexOf('} from "./lib.js";'));
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

// Opening an event replaces the Events tab's list with the event and its memories, which eventDetail
// does by id: it hides the create form, the filter and both results cards, and the back button's
// label is written into one more. A renamed or removed card leaves every one of those a null, and
// $(null).classList throws — but only at the moment somebody opens an event, which no other test
// here reaches. Same silent shape as the sub-tab wiring above, and the same cheap answer.
test("the event drill-down names cards and controls that exist", () => {
  const fn = appCode.match(/function eventDetail\(on\) \{(.*?)\n\}/s);

  assert.ok(fn, "eventDetail not found in app.js");

  // Once the class names are taken out, every quoted string left in the body is an element id: the
  // ones passed straight to $() and the ones in the list it hides by iteration. Plus the back
  // button, whose label openEvent writes.
  const body = fn[1].replace(/classList\.\w+\(\s*"[\w-]+"/g, "classList");
  const named = [...body.matchAll(/"([\w-]+)"/g)].map((m) => m[1]);
  const ids = [...new Set([...named, "event-back"])];

  assert.ok(ids.length >= 6, `only parsed ${ids.length} ids from eventDetail`);

  for (const id of ids) {
    assert.ok(
      htmlCode.includes(`id="${id}"`),
      `the drill-down names #${id}, which is not in the markup`,
    );
  }
});

// The back button is the only way out of the drill-down, so it must be the control that leaves it
// and it must not be the only thing that does: a hand-driven tab click has to leave it too, or the
// Events tab keeps showing one event where its list belongs.
test("leaving the drill-down is wired to the back button and to the nav", () => {
  assert.match(
    htmlCode,
    /data-act="close-event"\s+id="event-back"/,
    "the back button must carry close-event",
  );

  const nav = appCode.match(
    /document\.querySelectorAll\("nav button"\)\.forEach\(\(b\) => \{(.*?)\n\}\);/s,
  );

  assert.ok(nav, "the nav listener was not found in app.js");
  assert.match(
    nav[1],
    /closeEvent\(\)/,
    "a nav click must leave the drill-down before switching tabs",
  );
});
