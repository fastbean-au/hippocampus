// Settings data only — deliberately free of any Obsidian import so mapping/parse logic that
// references the settings shape stays unit-testable. The settings *tab* (which needs the Obsidian
// UI API) lives in settings-tab.ts.

export type GroupSource = "folder" | "frontmatter" | "fixed";

export interface HippocampusSettings {
  // Connection.
  baseUrl: string;
  token: string;

  // Significance / group mapping (shared by the manual commands and auto-sync).
  defaultSignificance: number;
  significanceFrontmatterKey: string;
  groupSource: GroupSource;
  groupFrontmatterKey: string;
  groupFixedValue: string;
  stripFrontmatter: boolean;

  // Frontmatter keys copied onto each memory's metadata (the multi-dimensional classification the
  // single group label cannot carry). An allowlist rather than "copy all frontmatter": a vault's
  // frontmatter holds plugin bookkeeping, tag arrays and dates that would fill the memory's
  // metadata budget with noise, and the keys are the note author's rather than the operator's.
  metadataFrontmatterKeys: string[];

  // Fixed labels stamped on every synced memory, as "key=value" lines.
  metadataFixed: string;

  // Search.
  searchLimit: number;
  searchReinforce: boolean;

  // Auto-sync.
  autoSync: boolean;
  syncFolder: string;
  syncDebounceSeconds: number;

  // Mirror the vault's [[wikilinks]] onto the memory link graph. On by default, because a synced
  // vault whose links are dropped is a store of isolated memories - and the link graph is the one
  // thing Obsidian has that the decay model most wants, since a link raises the effective
  // significance of both ends. It costs one extra read per synced note plus a write when the links
  // have changed, which is why it can be turned off.
  syncLinks: boolean;

  // The weight each wikilink carries. A wikilink has no weight in Obsidian, so one number stands
  // for all of them; it is summed across an item's links and then log1p-damped by the service, so
  // raising it has a much smaller effect than it looks like it should.
  linkSignificance: number;

  // Persisted note-path -> memory-id map (not shown in the settings UI). Kept here so it rides
  // along in the plugin's single saved-data blob.
  pathToId: Record<string, string>;
}

export const DEFAULT_SETTINGS: HippocampusSettings = {
  baseUrl: "http://127.0.0.1:8080",
  token: "",
  defaultSignificance: 5,
  significanceFrontmatterKey: "significance",
  groupSource: "folder",
  groupFrontmatterKey: "group",
  groupFixedValue: "",
  stripFrontmatter: true,
  metadataFrontmatterKeys: [],
  metadataFixed: "",
  searchLimit: 10,
  searchReinforce: false,
  autoSync: false,
  syncFolder: "",
  syncDebounceSeconds: 5,
  syncLinks: true,
  linkSignificance: 5,
  pathToId: {},
};
