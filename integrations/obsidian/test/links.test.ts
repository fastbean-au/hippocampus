import assert from "node:assert/strict";
import { test } from "node:test";

import { MAX_LINKS, linkDelta, linkPath, resolveLinkIds } from "../src/links";
import { linksFrom } from "../src/parse";

test("linkPath strips the alias and the subpath", () => {
  assert.equal(linkPath("Note"), "Note");
  assert.equal(linkPath("Note|shown as this"), "Note");
  assert.equal(linkPath("Note#Section"), "Note");
  assert.equal(linkPath("Note#^abc123"), "Note");
  assert.equal(linkPath("Folder/Note#Section|alias"), "Folder/Note");
  assert.equal(linkPath("  Note  "), "Note");
});

// A link within the same note carries no path at all, and must not become a self-link the service
// would refuse.
test("linkPath reduces a same-note link to nothing", () => {
  assert.equal(linkPath("#Section"), "");
  assert.equal(linkPath("#^abc123"), "");
});

test("resolveLinkIds maps note paths to memory ids", () => {
  const map = { "a.md": "m-a", "b.md": "m-b", "c.md": "m-c" };

  assert.deepEqual(resolveLinkIds(["a.md", "c.md"], map, "m-self"), [
    "m-a",
    "m-c",
  ]);
});

// The three silent drops, all of them ordinary in a vault rather than exceptional.
test("resolveLinkIds drops what cannot become an edge", () => {
  const map = { "a.md": "m-a", "b.md": "m-b", "self.md": "m-self" };

  assert.deepEqual(
    resolveLinkIds(
      [
        "missing.md", // a wikilink to a note that does not exist yet
        "a.md",
        "a.md", // the same note linked twice is one edge
        "self.md", // a note linking itself; the service refuses a self-link
        "b.md",
      ],
      map,
      "m-self",
    ),
    ["m-a", "m-b"],
  );
});

test("resolveLinkIds caps at the service's per-item bound, keeping document order", () => {
  const map: Record<string, string> = {};
  const paths: string[] = [];

  for (let i = 0; i < MAX_LINKS + 20; i++) {
    map[`n${i}.md`] = `m-${i}`;
    paths.push(`n${i}.md`);
  }

  const ids = resolveLinkIds(paths, map, "m-self");

  assert.equal(ids.length, MAX_LINKS);
  assert.equal(ids[0], "m-0");
  assert.equal(ids[MAX_LINKS - 1], `m-${MAX_LINKS - 1}`);
});

test("linkDelta adds what is new and removes what the note no longer links", () => {
  const { add, remove } = linkDelta(
    ["m-a", "m-b"],
    [
      { id: "m-b", outbound: true },
      { id: "m-c", outbound: true },
    ],
  );

  assert.deepEqual(add, ["m-a"]);
  assert.deepEqual(remove, ["m-c"]);
});

// The removal rule that matters: unlinking removes the pair in BOTH directions, so an edge the far
// end also declares is left alone rather than silently deleted on its behalf.
test("linkDelta leaves an edge the other note declares too", () => {
  const { add, remove } = linkDelta(
    [],
    [
      { id: "m-mutual", outbound: true },
      { id: "m-mutual", outbound: false },
      { id: "m-ours", outbound: true },
    ],
  );

  assert.deepEqual(add, []);
  assert.deepEqual(remove, ["m-ours"]);
});

// An inbound-only edge is somebody else's, and a note that has not changed must produce no writes at
// all - which is what makes re-syncing a whole vault cheap.
test("linkDelta ignores inbound edges and is a no-op when nothing moved", () => {
  const { add, remove } = linkDelta(
    ["m-a"],
    [
      { id: "m-a", outbound: true },
      { id: "m-inbound", outbound: false },
    ],
  );

  assert.deepEqual(add, []);
  assert.deepEqual(remove, []);
});

test("linksFrom normalises the gateway's direction enum", () => {
  const edges = linksFrom({
    links: [
      { id: "m-a", significance: "5", direction: "LINK_DIRECTION_OUTBOUND" },
      { id: "m-b", significance: 3, direction: "LINK_DIRECTION_INBOUND" },
      { id: "m-c", direction: 2 },
      { id: "m-d" },
    ],
  });

  assert.deepEqual(edges, [
    { id: "m-a", significance: 5, outbound: true },
    { id: "m-b", significance: 3, outbound: false },
    { id: "m-c", significance: 0, outbound: true },
    // An absent direction is read as not ours, which is the safe reading: the delta only ever
    // removes an edge it believes it declared.
    { id: "m-d", significance: 0, outbound: false },
  ]);
});

test("linksFrom tolerates a response with no links", () => {
  assert.deepEqual(linksFrom({}), []);
});
