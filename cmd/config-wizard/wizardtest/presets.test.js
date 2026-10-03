// The wizard's starting points, held to the wizard's own checks and to docs/use-cases.md.
//
// A preset is the configuration most first-time operators will start from and many will not
// change, so a preset that raises a warning is the wizard advising against its own suggestion - and
// before this test, three of the five did: one ran an OpenSearch cluster to answer searches the
// store already answered, one configured a store with no capacity bound at all, and one set a
// minimum age its own retention floor made meaningless. Nothing caught it, because nothing ran
// validate() over them.

import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { loadWizard, repoRoot } from "./harness.js";

const presets = loadWizard().PRESETS;

// names reports whether an issue's text mentions a configuration key. Keys appear in the messages
// verbatim, which is what lets an issue be traced back to the field that would fix it.
const names = (issue, key) => issue.text.includes(key);

for (const preset of presets) {
  test(`preset "${preset.id}" raises nothing it has not declared`, () => {
    const wizard = loadWizard();
    const chosen = wizard.PRESETS.find(
      (candidate) => candidate.id === preset.id,
    );

    wizard.applyPreset(chosen);

    const issues = wizard.validate();
    const used = new Set();

    for (const issue of issues) {
      if (issue.level === "info") {
        continue;
      }

      if (chosen.supply.some((key) => names(issue, key))) {
        continue;
      }

      if (issue.level === "warn") {
        const acknowledged = chosen.accepts.find((entry) =>
          issue.text.includes(entry.match),
        );

        if (acknowledged) {
          used.add(acknowledged.match);

          continue;
        }
      }

      assert.fail(
        `preset "${chosen.id}" raises an ${issue.level} it neither leaves to the operator (supply) nor acknowledges (accepts): ${issue.text}`,
      );
    }

    for (const entry of chosen.accepts) {
      assert.ok(
        used.has(entry.match),
        `preset "${chosen.id}" acknowledges a warning matching "${entry.match}" that it no longer raises — remove the stale entry`,
      );
    }
  });
}

test("every preset sets only declared fields, to values they accept", () => {
  const wizard = loadWizard();

  for (const preset of wizard.PRESETS) {
    for (const [key, entry] of Object.entries(preset.values)) {
      const field = wizard.FIELDS.get(key);

      assert.ok(
        field,
        `preset "${preset.id}" sets ${key}, which the wizard does not declare — the value would be carried silently and never shown`,
      );

      switch (field.type) {
        case "bool":
          assert.equal(
            typeof entry,
            "boolean",
            `preset "${preset.id}": ${key} is a bool`,
          );

          break;

        case "int":
          assert.ok(
            Number.isInteger(entry),
            `preset "${preset.id}": ${key} is an int`,
          );

          break;

        case "float":
          assert.equal(
            typeof entry,
            "number",
            `preset "${preset.id}": ${key} is a number`,
          );

          break;

        case "select":
          assert.ok(
            field.options.some(([option]) => String(option) === String(entry)),
            `preset "${preset.id}": ${key} = ${entry} is not one of its options`,
          );

          break;

        default:
          assert.equal(
            typeof entry,
            "string",
            `preset "${preset.id}": ${key} is text`,
          );
      }
    }

    for (const key of preset.supply) {
      assert.ok(
        wizard.FIELDS.has(key),
        `preset "${preset.id}" asks the operator to supply ${key}, which the wizard does not declare`,
      );
    }

    assert.ok(
      wizard.TARGETS.some((target) => target.id === preset.target),
      `preset "${preset.id}" names target "${preset.target}", which does not exist`,
    );

    for (const property of ["label", "blurb", "suits", "useCase"]) {
      assert.ok(preset[property], `preset "${preset.id}" has no ${property}`);
    }
  }
});

// GitHub's heading anchor: lower-cased, punctuation other than hyphens dropped, each space a hyphen
// (so "a / b" becomes "a--b").
const slug = (heading) =>
  heading
    .trim()
    .toLowerCase()
    .replace(/[^\p{L}\p{N}\s-]/gu, "")
    .replace(/\s/g, "-");

test("every preset names a section of docs/use-cases.md, and that section names it back", () => {
  const doc = readFileSync(join(repoRoot, "docs", "use-cases.md"), "utf8");
  const sections = new Map();
  let current = null;

  for (const line of doc.split("\n")) {
    const heading = /^#{2,4} (.+)$/.exec(line);

    if (heading) {
      current = slug(heading[1]);
      sections.set(current, "");

      continue;
    }

    if (current) {
      sections.set(current, sections.get(current) + line + "\n");
    }
  }

  for (const preset of presets) {
    assert.ok(
      sections.has(preset.useCase),
      `preset "${preset.id}" links to #${preset.useCase}, which is no heading in docs/use-cases.md`,
    );

    assert.ok(
      sections
        .get(preset.useCase)
        .includes(`Wizard starting point: ${preset.label}.`),
      `the use-cases section #${preset.useCase} does not name its wizard starting point ("Wizard starting point: ${preset.label}.")`,
    );
  }
});
