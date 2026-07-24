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
			const raw = frontmatter ? frontmatter[settings.groupFrontmatterKey] : undefined;

			return raw === undefined || raw === null ? "" : String(raw);
		}

		case "fixed":
			return settings.groupFixedValue;

		case "folder":
		default:
			return topFolder(path);
	}
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
