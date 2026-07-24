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

	// Search.
	searchLimit: number;
	searchReinforce: boolean;

	// Auto-sync.
	autoSync: boolean;
	syncFolder: string;
	syncDebounceSeconds: number;

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
	searchLimit: 10,
	searchReinforce: false,
	autoSync: false,
	syncFolder: "",
	syncDebounceSeconds: 5,
	pathToId: {},
};
