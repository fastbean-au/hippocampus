import { Notice, TAbstractFile, TFile } from "obsidian";

import { HippocampusError } from "./client";
import {
  isUnder,
  resolveGroup,
  resolveMetadata,
  resolveSignificance,
  stripFrontmatter,
} from "./mapping";
import type HippocampusPlugin from "./main";

// SyncEngine keeps notes under the configured folder mirrored into Hippocampus. It is idempotent:
// a note's memory id is remembered in the plugin's saved data, so re-saving updates that memory
// rather than storing a duplicate, and a note the sleep cycle has forgotten is re-created.
export class SyncEngine {
  private readonly plugin: HippocampusPlugin;
  private readonly timers: Map<string, number> = new Map();

  constructor(plugin: HippocampusPlugin) {
    this.plugin = plugin;
  }

  private isSyncable(file: TAbstractFile): file is TFile {
    if (!(file instanceof TFile) || file.extension !== "md") {
      return false;
    }

    return isUnder(file.path, this.plugin.settings.syncFolder);
  }

  // onModify debounces per-note so a burst of keystrokes results in a single sync.
  onModify(file: TAbstractFile): void {
    if (!this.plugin.settings.autoSync || !this.isSyncable(file)) {
      return;
    }

    const path = file.path;
    const existing = this.timers.get(path);

    if (existing !== undefined) {
      window.clearTimeout(existing);
    }

    const delay = Math.max(0, this.plugin.settings.syncDebounceSeconds) * 1000;
    const timer = window.setTimeout(() => {
      this.timers.delete(path);
      void this.syncFile(file as TFile);
    }, delay);

    this.timers.set(path, timer);
  }

  // onRename follows the note: the remembered id moves to the new path.
  onRename(file: TAbstractFile, oldPath: string): void {
    const map = this.plugin.settings.pathToId;

    if (map[oldPath] !== undefined) {
      map[oldPath === file.path ? oldPath : file.path] = map[oldPath];

      if (oldPath !== file.path) {
        delete map[oldPath];
      }

      void this.plugin.saveSettings();
    }
  }

  // onDelete removes the note's memory and forgets the mapping.
  onDelete(file: TAbstractFile): void {
    const map = this.plugin.settings.pathToId;
    const id = map[file.path];

    if (id === undefined) {
      return;
    }

    delete map[file.path];
    void this.plugin.saveSettings();
    void this.plugin.client.deleteMemories([id]).catch(() => {
      /* best-effort: the memory may already be gone */
    });
  }

  // syncFile stores or updates the memory for a single note.
  async syncFile(file: TFile): Promise<void> {
    const content = await this.plugin.app.vault.cachedRead(file);
    const frontmatter =
      this.plugin.app.metadataCache.getFileCache(file)?.frontmatter;
    const settings = this.plugin.settings;

    const body = settings.stripFrontmatter
      ? stripFrontmatter(content)
      : content;

    if (body.trim() === "") {
      return;
    }

    const significance = resolveSignificance(
      frontmatter,
      settings.significanceFrontmatterKey,
      settings.defaultSignificance,
    );
    const group = resolveGroup(settings, file.path, frontmatter);
    const metadata = resolveMetadata(settings, frontmatter);
    const map = settings.pathToId;
    const knownId = map[file.path];

    try {
      if (knownId !== undefined) {
        await this.plugin.client.updateMemory(
          knownId,
          body,
          significance,
          metadata,
        );

        return;
      }

      await this.store(file.path, body, significance, group, metadata);
    } catch (err) {
      if (err instanceof HippocampusError && err.status === 404) {
        // The sleep cycle deleted this memory; re-create it.
        delete map[file.path];
        await this.store(file.path, body, significance, group, metadata);

        return;
      }

      throw err;
    }
  }

  private async store(
    path: string,
    body: string,
    significance: number,
    group: string,
    metadata?: Record<string, string>,
  ): Promise<void> {
    const result = await this.plugin.client.storeMemory({
      body,
      significance,
      group,
      metadata,
    });

    if (result.rejected || result.id === "") {
      return;
    }

    this.plugin.settings.pathToId[path] = result.id;
    await this.plugin.saveSettings();
  }

  // syncAll pushes every syncable note in the vault once (the "Sync folder now" command).
  async syncAll(): Promise<{ synced: number; failed: number }> {
    const files = this.plugin.app.vault
      .getMarkdownFiles()
      .filter((file) => this.isSyncable(file));
    let synced = 0;
    let failed = 0;

    for (const file of files) {
      try {
        await this.syncFile(file);
        synced += 1;
      } catch (err) {
        failed += 1;
        console.error("hippocampus: failed to sync", file.path, err);
      }
    }

    new Notice(
      `Hippocampus: synced ${synced} note(s)` +
        (failed > 0 ? `, ${failed} failed` : ""),
    );

    return { synced, failed };
  }

  dispose(): void {
    for (const timer of this.timers.values()) {
      window.clearTimeout(timer);
    }

    this.timers.clear();
  }
}
