// Loads the configuration wizard's script into a fresh vm context and hands back its internals.
//
// The Go tests beside this directory scan app.js as text, which holds the KEY space - every key a
// check reads is a declared field - but cannot say what a check concludes. That needs the script
// run, and it can be: everything at the top level is a declaration, and the one statement that
// touches the DOM is the trailing init() call, which is cut off here. What remains needs no
// document at all until something renders, and nothing here renders.
//
// localStorage is deliberately absent. persist() and restore() wrap every access in try/catch for
// a browser with storage disabled, and the ReferenceError lands there, so the wizard behaves exactly
// as it does in a private window.

import vm from "node:vm";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));

export const repoRoot = join(here, "..", "..", "..");

const source = readFileSync(join(here, "..", "wizard", "app.js"), "utf8");
const boot = /\ninit\(\);\s*$/;

// Each call returns a new context, because the wizard's state is a module-level object and a test
// that applied one preset must not leave its answers behind for the next.
export function loadWizard() {
  if (!boot.test(source)) {
    throw new Error(
      "app.js no longer ends with init(); - the harness cannot load it without running the DOM boot",
    );
  }

  const script =
    source.replace(boot, "\n") +
    "\nglobalThis.__wizard = { PRESETS, TARGETS, FIELDS, state, value, setValue, applyPreset, validate, buildConfig };\n";
  const context = vm.createContext({});

  vm.runInContext(script, context, { filename: "app.js" });

  return context.__wizard;
}
