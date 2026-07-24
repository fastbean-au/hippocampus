import { Editor, Modal, Notice, Plugin, Setting, TAbstractFile } from "obsidian";

import { HippocampusClient } from "./client";
import { resolveGroup, resolveSignificance, stripFrontmatter } from "./mapping";
import { DEFAULT_SETTINGS, HippocampusSettings } from "./settings";
import { HippocampusSettingTab } from "./settings-tab";
import { SyncEngine } from "./sync";
import { MemoryView } from "./types";

export default class HippocampusPlugin extends Plugin {
	settings: HippocampusSettings = DEFAULT_SETTINGS;
	client!: HippocampusClient;
	private sync!: SyncEngine;

	async onload(): Promise<void> {
		await this.loadSettings();

		this.rebuildClient();
		this.sync = new SyncEngine(this);

		this.addSettingTab(new HippocampusSettingTab(this.app, this));

		this.addCommand({
			id: "store-current-note",
			name: "Store current note as memory",
			callback: () => void this.storeCurrentNote(),
		});

		this.addCommand({
			id: "store-selection",
			name: "Store selection as memory",
			editorCallback: (editor) => void this.storeSelection(editor),
		});

		this.addCommand({
			id: "search-memories",
			name: "Search memories and insert results",
			editorCallback: (editor) => this.openSearch(editor),
		});

		this.addCommand({
			id: "sync-folder-now",
			name: "Sync folder now",
			callback: () => void this.sync.syncAll(),
		});

		this.addCommand({
			id: "test-connection",
			name: "Test connection",
			callback: () => void this.testConnection(),
		});

		// The vault handlers self-gate on settings.autoSync (except rename/delete, which keep the
		// path->id map consistent for manually-stored notes too), so they are registered once.
		this.registerEvent(this.app.vault.on("modify", (file) => this.sync.onModify(file)));
		this.registerEvent(
			this.app.vault.on("rename", (file: TAbstractFile, oldPath: string) =>
				this.sync.onRename(file, oldPath),
			),
		);
		this.registerEvent(this.app.vault.on("delete", (file) => this.sync.onDelete(file)));
	}

	onunload(): void {
		this.sync?.dispose();
	}

	async loadSettings(): Promise<void> {
		this.settings = Object.assign({}, DEFAULT_SETTINGS, await this.loadData());
	}

	async saveSettings(): Promise<void> {
		await this.saveData(this.settings);
		this.rebuildClient();
	}

	rebuildClient(): void {
		this.client = new HippocampusClient({
			baseUrl: this.settings.baseUrl,
			token: this.settings.token,
		});
	}

	// refreshSyncRegistration exists for the settings toggle; the vault handlers self-gate on
	// settings.autoSync, so nothing needs re-wiring here.
	refreshSyncRegistration(): void {
		if (!this.settings.autoSync) {
			this.sync.dispose();
		}
	}

	async testConnection(): Promise<void> {
		try {
			await this.client.health();
			new Notice("Hippocampus: connection OK");
		} catch (err) {
			new Notice("Hippocampus: connection failed — " + errorMessage(err));
		}
	}

	// storeCurrentNote stores (or idempotently updates) the active note via the sync engine, so a
	// manual store and auto-sync share one memory per note path.
	private async storeCurrentNote(): Promise<void> {
		const file = this.app.workspace.getActiveFile();

		if (!file) {
			new Notice("Hippocampus: no active note");

			return;
		}

		try {
			await this.sync.syncFile(file);
			new Notice(`Hippocampus: stored "${file.basename}"`);
		} catch (err) {
			new Notice("Hippocampus: store failed — " + errorMessage(err));
		}
	}

	private async storeSelection(editor: Editor): Promise<void> {
		const selection = editor.getSelection();

		if (selection.trim() === "") {
			new Notice("Hippocampus: nothing selected");

			return;
		}

		const file = this.app.workspace.getActiveFile();
		const frontmatter = file
			? this.app.metadataCache.getFileCache(file)?.frontmatter
			: undefined;
		const significance = resolveSignificance(
			frontmatter,
			this.settings.significanceFrontmatterKey,
			this.settings.defaultSignificance,
		);
		const group = file ? resolveGroup(this.settings, file.path, frontmatter) : this.settings.groupFixedValue;

		try {
			const result = await this.client.storeMemory({
				body: this.settings.stripFrontmatter ? stripFrontmatter(selection) : selection,
				significance,
				group,
			});

			new Notice(
				result.rejected
					? "Hippocampus: selection rejected (below minimum significance)"
					: "Hippocampus: stored selection",
			);
		} catch (err) {
			new Notice("Hippocampus: store failed — " + errorMessage(err));
		}
	}

	private openSearch(editor: Editor): void {
		new SearchModal(this, async (query) => {
			try {
				const memories = await this.client.searchMemories({
					query,
					limit: this.settings.searchLimit,
					reinforce: this.settings.searchReinforce,
				});

				editor.replaceSelection(formatSearchResults(query, memories));
			} catch (err) {
				new Notice("Hippocampus: search failed — " + errorMessage(err));
			}
		}).open();
	}
}

// formatSearchResults renders matched memories as a Markdown list to insert at the cursor.
function formatSearchResults(query: string, memories: MemoryView[]): string {
	if (memories.length === 0) {
		return `> [!info] Hippocampus: no memories matched "${query}"\n`;
	}

	const lines = memories.map((memory) => {
		const meta = `sig ${memory.significance}` + (memory.group ? `, group ${memory.group}` : "");
		const body = memory.body.replace(/\n+/g, " ").trim();

		return `- ${body}  \n  <sub>${meta} · \`${memory.id}\`</sub>`;
	});

	return `**Hippocampus matches for "${query}":**\n\n` + lines.join("\n") + "\n";
}

function errorMessage(err: unknown): string {
	return err instanceof Error ? err.message : String(err);
}

// SearchModal prompts for a query string and invokes onSubmit with it.
class SearchModal extends Modal {
	private query = "";
	private readonly onSubmit: (query: string) => void;

	constructor(plugin: HippocampusPlugin, onSubmit: (query: string) => void) {
		super(plugin.app);
		this.onSubmit = onSubmit;
	}

	onOpen(): void {
		const { contentEl } = this;

		contentEl.createEl("h3", { text: "Search Hippocampus memories" });

		new Setting(contentEl).setName("Query").addText((text) => {
			text.onChange((value) => (this.query = value));
			text.inputEl.addEventListener("keydown", (evt) => {
				if (evt.key === "Enter") {
					evt.preventDefault();
					this.submit();
				}
			});
			window.setTimeout(() => text.inputEl.focus(), 0);
		});

		new Setting(contentEl).addButton((btn) =>
			btn
				.setButtonText("Search")
				.setCta()
				.onClick(() => this.submit()),
		);
	}

	private submit(): void {
		const query = this.query.trim();

		if (query === "") {
			return;
		}

		this.close();
		this.onSubmit(query);
	}

	onClose(): void {
		this.contentEl.empty();
	}
}
