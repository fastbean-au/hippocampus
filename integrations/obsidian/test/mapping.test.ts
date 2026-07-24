import assert from "node:assert/strict";
import { test } from "node:test";

import {
	isUnder,
	resolveGroup,
	resolveSignificance,
	stripFrontmatter,
	topFolder,
} from "../src/mapping";
import { DEFAULT_SETTINGS, HippocampusSettings } from "../src/settings";

function settings(overrides: Partial<HippocampusSettings>): HippocampusSettings {
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
	assert.equal(resolveSignificance({ significance: "7" }, "significance", 5), 7);
	assert.equal(resolveSignificance({ significance: "high" }, "significance", 5), 5);
	assert.equal(resolveSignificance(undefined, "significance", 5), 5);
});

test("resolveGroup honours each source", () => {
	assert.equal(resolveGroup(settings({ groupSource: "folder" }), "Daily/n.md", undefined), "Daily");
	assert.equal(
		resolveGroup(settings({ groupSource: "fixed", groupFixedValue: "vault" }), "Daily/n.md", undefined),
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
	assert.equal(stripFrontmatter("---\nsignificance: 7\n---\nbody text"), "body text");
	assert.equal(stripFrontmatter("no frontmatter here"), "no frontmatter here");
	assert.equal(stripFrontmatter("body with --- in middle"), "body with --- in middle");
});
