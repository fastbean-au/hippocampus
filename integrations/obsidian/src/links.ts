// Pure helpers that turn a note's wikilinks into the link writes a memory needs. They take
// primitives rather than Obsidian objects so they are unit-testable without a running app; the
// resolution of a link's text to an actual note is the vault's job and stays in sync.ts.
//
// The premise this exists for: Obsidian's whole model is `[[wikilinks]]`, and Hippocampus raises the
// effective significance of BOTH ends of a link, damped by log1p. So a heavily-linked note is
// exactly the note the decay model should keep - and a synced vault that dropped its link graph
// produced a store of isolated memories, decaying as though the vault had no structure at all.

// MAX_LINKS mirrors the service's per-item bound (types.MaxLinks). It is applied here as well as
// there because the service counts links in BOTH directions against it, so a note whose outbound
// links alone exceed the cap would have every link write refused rather than merely trimmed.
export const MAX_LINKS = 128;

// linkPath reduces one wikilink's raw target to the note it names.
//
// A wikilink may carry a display alias (`[[Note|shown as this]]`) and a subpath - a heading
// (`[[Note#Section]]`) or a block reference (`[[Note#^abc123]]`). Neither changes which note is
// linked, and both must go before the text can be resolved. A link that is only a subpath
// (`[[#Section]]`, a link within the same note) reduces to "" and is dropped by the caller, since a
// memory may not link to itself.
export function linkPath(raw: string): string {
  let out = raw.trim();

  const alias = out.indexOf("|");

  if (alias >= 0) {
    out = out.slice(0, alias);
  }

  const subpath = out.indexOf("#");

  if (subpath >= 0) {
    out = out.slice(0, subpath);
  }

  return out.trim();
}

// resolveLinkIds turns the note paths a file links to into the memory ids to link, dropping
// everything that cannot become one.
//
// Three things are dropped silently, and all three are normal rather than exceptional. A path with
// no entry in the map is a note that has not been synced - or, most often in Obsidian, a wikilink to
// a note that does not exist yet, which is an ordinary way to write. A duplicate is a note linked
// twice, which is one edge. And the note's own id is dropped because the service refuses a
// self-link, which a `[[Note]]` inside Note itself would otherwise produce.
//
// The cap keeps document order, so what survives on a hub note is the links its author wrote first.
export function resolveLinkIds(
  paths: string[],
  pathToId: Record<string, string>,
  selfId: string,
): string[] {
  const out: string[] = [];
  const seen = new Set<string>();

  for (const path of paths) {
    const id = pathToId[path];

    if (id === undefined || id === "" || id === selfId || seen.has(id)) {
      continue;
    }

    seen.add(id);
    out.push(id);

    if (out.length === MAX_LINKS) {
      break;
    }
  }

  return out;
}

// LinkEdgeView is one edge as the service reports it, narrowed to what the delta needs.
export interface LinkEdgeView {
  id: string;
  outbound: boolean;
}

// linkDelta works out what to write so a memory's outbound links match the note's wikilinks.
//
// Removal is the half worth reading. A wikilink deleted from a note must take its edge with it, or
// the graph only ever grows and a note nothing links to any more stays propped up by edges that
// describe a vault that no longer exists - which is the opposite of what linking it was for.
//
// But the service's unlink removes the pair in BOTH directions, so a target that links back to us is
// left alone: the far end still declares that edge, the vault's link graph still contains it, and
// unlinking would silently delete somebody else's link until that note happened to be synced again.
export function linkDelta(
  desired: string[],
  existing: LinkEdgeView[],
): { add: string[]; remove: string[] } {
  const wanted = new Set(desired);
  const held = new Set<string>();
  const linksBack = new Set<string>();

  for (const edge of existing) {
    if (edge.outbound) {
      held.add(edge.id);
    } else {
      linksBack.add(edge.id);
    }
  }

  return {
    add: desired.filter((id) => !held.has(id)),
    remove: [...held].filter((id) => !wanted.has(id) && !linksBack.has(id)),
  };
}
