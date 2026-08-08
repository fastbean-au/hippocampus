import assert from "node:assert/strict";
import { test } from "node:test";

import {
  isUnder,
  normaliseMetadataKey,
  resolveGroup,
  resolveMetadata,
  resolveSignificance,
  stripFrontmatter,
  topFolder,
} from "../src/mapping";
import { DEFAULT_SETTINGS, HippocampusSettings } from "../src/settings";

function settings(
  overrides: Partial<HippocampusSettings>,
): HippocampusSettings {
  return Object.assign({}, DEFAULT_SETTINGS, overrides);
}

test("topFolder returns the first path segment", () => {
  assert.equal(topFolder("Daily/2026/note.md"), "Daily");
  assert.equal(topFolder("note.md"), "");
});

test("isUnder scopes to a folder (empty = whole vault)", () => {
  assert.equal(isUnder("Daily/note.md", "Daily"), true);
  assert.equal(isUnder("Daily", "Daily"), true);
  assert.equal(isUnder("Projects/note.md", "Daily"), false);
  assert.equal(isUnder("Dailies/note.md", "Daily"), false); // prefix but not a folder boundary
  assert.equal(isUnder("anything.md", ""), true);
});

test("resolveSignificance prefers a numeric frontmatter override", () => {
  assert.equal(resolveSignificance({ significance: 9 }, "significance", 5), 9);
  assert.equal(
    resolveSignificance({ significance: "7" }, "significance", 5),
    7,
  );
  assert.equal(
    resolveSignificance({ significance: "high" }, "significance", 5),
    5,
  );
  assert.equal(resolveSignificance(undefined, "significance", 5), 5);
});

test("resolveGroup honours each source", () => {
  assert.equal(
    resolveGroup(settings({ groupSource: "folder" }), "Daily/n.md", undefined),
    "Daily",
  );
  assert.equal(
    resolveGroup(
      settings({ groupSource: "fixed", groupFixedValue: "vault" }),
      "Daily/n.md",
      undefined,
    ),
    "vault",
  );
  assert.equal(
    resolveGroup(
      settings({ groupSource: "frontmatter", groupFrontmatterKey: "area" }),
      "Daily/n.md",
      { area: "work" },
    ),
    "work",
  );
});

test("stripFrontmatter removes only a leading YAML block", () => {
  assert.equal(
    stripFrontmatter("---\nsignificance: 7\n---\nbody text"),
    "body text",
  );
  assert.equal(stripFrontmatter("no frontmatter here"), "no frontmatter here");
  assert.equal(
    stripFrontmatter("body with --- in middle"),
    "body with --- in middle",
  );
});

test("resolveMetadata returns undefined when nothing is configured", () => {
  assert.equal(resolveMetadata(settings({}), { project: "apollo" }), undefined);
});

test("resolveMetadata copies only the allowlisted frontmatter keys", () => {
  // "internal" is present in the frontmatter but not named, so it is not copied: a vault's
  // frontmatter carries plugin bookkeeping the note author never meant as classification.
  const got = resolveMetadata(
    settings({ metadataFrontmatterKeys: ["project", "author"] }),
    { project: "apollo", author: "sean", internal: "do not copy" },
  );

  assert.deepEqual(got, { project: "apollo", author: "sean" });
});

test("resolveMetadata stringifies values and joins arrays", () => {
  const got = resolveMetadata(
    settings({ metadataFrontmatterKeys: ["tags", "count", "done", "nested"] }),
    { tags: ["a", "b"], count: 7, done: true, nested: { no: "good" } },
  );

  // An array is a meaningful list of values; an object is not, and is skipped.
  assert.deepEqual(got, { tags: "a,b", count: "7", done: "true" });
});

test("resolveMetadata normalises awkward frontmatter keys", () => {
  const got = resolveMetadata(
    settings({ metadataFrontmatterKeys: ["Project Name"] }),
    {
      "Project Name": "apollo",
    },
  );

  assert.deepEqual(got, { project_name: "apollo" });
});

test("resolveMetadata applies fixed labels, which frontmatter overrides", () => {
  const got = resolveMetadata(
    settings({
      metadataFixed: "source=obsidian\nvault=work",
      metadataFrontmatterKeys: ["vault"],
    }),
    { vault: "personal" },
  );

  assert.deepEqual(got, { source: "obsidian", vault: "personal" });
});

test("resolveMetadata skips malformed fixed lines rather than failing", () => {
  const got = resolveMetadata(
    settings({ metadataFixed: "source=obsidian\nnovalue\n=novalue\n" }),
    undefined,
  );

  assert.deepEqual(got, { source: "obsidian" });
});

test("normaliseMetadataKey fits the service's key charset", () => {
  assert.equal(normaliseMetadataKey("Project Name"), "project_name");
  assert.equal(normaliseMetadataKey("content.type"), "content.type");
  assert.equal(normaliseMetadataKey("ns:key"), "ns:key");
  assert.equal(normaliseMetadataKey("path/to"), "path/to");
  assert.equal(normaliseMetadataKey("kebab-key"), "kebab-key");

  // A key must begin alphanumeric, so leading punctuation is dropped rather than substituted.
  assert.equal(normaliseMetadataKey("-leading"), "leading");
  assert.equal(normaliseMetadataKey("---"), "");
  assert.equal(normaliseMetadataKey(""), "");
});
