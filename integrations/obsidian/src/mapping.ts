// Pure helpers that turn a note (its path, frontmatter, and content) into the significance, group,
// and body a memory is stored with. They take primitives rather than Obsidian objects so they are
// straightforward to unit-test without a running app.

import type { HippocampusSettings } from "./settings";

export type Frontmatter = Record<string, unknown> | undefined;

// topFolder returns the first path segment of a vault-relative path ("" for a note in the root).
export function topFolder(path: string): string {
  const idx = path.indexOf("/");

  return idx < 0 ? "" : path.slice(0, idx);
}

// isUnder returns true when path is inside folder (or folder is empty, meaning the whole vault).
export function isUnder(path: string, folder: string): boolean {
  if (folder === "") {
    return true;
  }

  return path === folder || path.startsWith(folder + "/");
}

// resolveSignificance prefers a numeric frontmatter override, falling back to the configured
// default. A non-numeric or absent frontmatter value yields the default.
export function resolveSignificance(
  frontmatter: Frontmatter,
  key: string,
  fallback: number,
): number {
  const raw = frontmatter ? frontmatter[key] : undefined;

  if (typeof raw === "number" && Number.isFinite(raw)) {
    return raw;
  }

  if (typeof raw === "string" && raw.trim() !== "") {
    const n = Number(raw);

    if (Number.isFinite(n)) {
      return n;
    }
  }

  return fallback;
}

// resolveGroup derives the memory's group label from the configured source.
export function resolveGroup(
  settings: HippocampusSettings,
  path: string,
  frontmatter: Frontmatter,
): string {
  switch (settings.groupSource) {
    case "frontmatter": {
      const raw = frontmatter
        ? frontmatter[settings.groupFrontmatterKey]
        : undefined;

      return raw === undefined || raw === null ? "" : String(raw);
    }

    case "fixed":
      return settings.groupFixedValue;

    case "folder":
    default:
      return topFolder(path);
  }
}

// resolveMetadata builds a memory's metadata from the fixed labels plus the allowlisted frontmatter
// keys, which override a fixed label of the same key.
//
// It is an allowlist rather than "copy every frontmatter key", deliberately: a vault's frontmatter
// carries plugin bookkeeping, tag arrays, and dates that would fill the memory's metadata budget
// with noise the note author never meant as classification.
//
// Values are stringified and anything that cannot become a usable key/value pair is skipped rather
// than failing the sync - a note is not worth refusing to store over one awkward frontmatter entry.
export function resolveMetadata(
  settings: HippocampusSettings,
  frontmatter: Frontmatter,
): Record<string, string> | undefined {
  const out: Record<string, string> = {};

  for (const line of (settings.metadataFixed || "").split("\n")) {
    const trimmed = line.trim();
    const at = trimmed.indexOf("=");

    if (at <= 0) {
      continue;
    }

    const key = normaliseMetadataKey(trimmed.slice(0, at));

    if (key !== "") {
      out[key] = trimmed.slice(at + 1).trim();
    }
  }

  for (const name of settings.metadataFrontmatterKeys || []) {
    const raw = frontmatter ? frontmatter[name] : undefined;

    if (raw === undefined || raw === null) {
      continue;
    }

    const key = normaliseMetadataKey(name);

    // An array (Obsidian's tags, aliases, ...) is joined rather than dropped, since a list of
    // values is still a meaningful label; an object is not, and is skipped.
    const value = Array.isArray(raw)
      ? raw.map((v) => String(v)).join(",")
      : typeof raw === "object"
        ? ""
        : String(raw);

    if (key !== "" && value !== "") {
      out[key] = value;
    }
  }

  return Object.keys(out).length === 0 ? undefined : out;
}

// normaliseMetadataKey rewrites a frontmatter key into the service's metadata key charset
// ([A-Za-z0-9][A-Za-z0-9._:/-]*), lowercasing it and replacing anything else with "_". It returns ""
// for a name with no usable leading character, since a key must start alphanumeric. Frontmatter keys
// routinely contain spaces and capitals, which the service would otherwise reject.
export function normaliseMetadataKey(name: string): string {
  let out = "";

  for (const ch of name.trim().toLowerCase()) {
    const alphanumeric = /[a-z0-9]/.test(ch);

    if (alphanumeric) {
      out += ch;
    } else if (out === "") {
      // A key must begin alphanumeric, so leading punctuation is dropped rather than
      // substituted - a leading "_" would itself be invalid.
      continue;
    } else if (/[._:/-]/.test(ch)) {
      out += ch;
    } else {
      out += "_";
    }
  }

  return out.slice(0, 64);
}

// stripFrontmatter removes a single leading YAML frontmatter block, leaving the note body. It is a
// no-op when the content has no frontmatter.
export function stripFrontmatter(content: string): string {
  const match = content.match(/^---\r?\n[\s\S]*?\r?\n---\r?\n?/);

  if (!match) {
    return content;
  }

  return content.slice(match[0].length).replace(/^\s+/, "");
}
