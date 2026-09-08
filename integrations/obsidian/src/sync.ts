import { Notice, TAbstractFile, TFile } from "obsidian";

import { HippocampusError } from "./client";
import { linkDelta, linkPath, resolveLinkIds } from "./links";
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

  // syncFile stores or updates the memory for a single note, and by default reconciles its links.
  //
  // withLinks is false only for the first pass of a whole-vault sync, which runs its own link pass
  // once every note has a memory - doing it per note there would resolve half a vault's wikilinks
  // against a map that is still being built, and then do it all again.
  async syncFile(file: TFile, withLinks = true): Promise<void> {
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
    } finally {
      // After the body, always, and never allowed to fail the note: the links are enrichment, and a
      // note that stored but whose links did not is a better outcome than one that did neither.
      // Deliberately outside the store/update branch too, so a note whose text is unchanged but
      // whose wikilinks moved still has its edges corrected.
      if (withLinks) {
        await this.syncLinks(file);
      }
    }
  }

  // syncLinks makes a note's memory hold exactly the outbound links its wikilinks name.
  //
  // It runs AFTER the note is stored, and on a whole-vault sync it runs again in a second pass over
  // every note, for the reason ImportMemories documents about archives: a link routinely names a
  // target that appears later, and in a vault it always does - a note linked before it is written is
  // ordinary Obsidian, not an error. So the first pass creates the memories and the second resolves
  // between them.
  //
  // Everything here is best-effort. An unresolvable wikilink is dropped silently (it is normal), and
  // a refused write is logged rather than raised - most often the service's per-item link cap, which
  // counts BOTH directions, so a hub note can be past it on inbound links alone with nothing the
  // plugin can do about it.
  async syncLinks(file: TFile): Promise<void> {
    if (!this.plugin.settings.syncLinks) {
      return;
    }

    const id = this.plugin.settings.pathToId[file.path];

    if (id === undefined) {
      return;
    }

    const desired = resolveLinkIds(
      this.linkedPaths(file),
      this.plugin.settings.pathToId,
      id,
    );

    try {
      const { add, remove } = linkDelta(
        desired,
        await this.plugin.client.getMemoryLinks(id),
      );

      await this.plugin.client.linkMemories(
        id,
        add.map((target) => ({
          id: target,
          significance: this.plugin.settings.linkSignificance,
        })),
      );

      await this.plugin.client.unlinkMemories(id, remove);
    } catch (err) {
      console.error("hippocampus: failed to sync links for", file.path, err);
    }
  }

  // linkedPaths resolves the note's wikilinks and embeds to vault paths, in document order.
  //
  // Embeds count as links: a transclusion is the strongest statement one note makes about another,
  // and Obsidian's own graph draws it. Resolution is the metadata cache's rather than the plugin's,
  // so shortest-path names, relative paths and aliases behave exactly as they do in the vault, and
  // a link inside a code fence is not one - which is precisely what a hand-rolled regex over the
  // body would get wrong.
  private linkedPaths(file: TFile): string[] {
    const cache = this.plugin.app.metadataCache.getFileCache(file);

    if (!cache) {
      return [];
    }

    const out: string[] = [];

    for (const ref of [...(cache.links ?? []), ...(cache.embeds ?? [])]) {
      const path = linkPath(ref.link);

      if (path === "") {
        continue;
      }

      const target = this.plugin.app.metadataCache.getFirstLinkpathDest(
        path,
        file.path,
      );

      if (target !== null) {
        out.push(target.path);
      }
    }

    return out;
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
        await this.syncFile(file, false);
        synced += 1;
      } catch (err) {
        failed += 1;
        console.error("hippocampus: failed to sync", file.path, err);
      }
    }

    // The second pass. Every note now has a memory, so a wikilink whose target was written later in
    // the walk - or simply later in the vault - resolves this time round. syncFile has already run
    // its own pass, which is what keeps a single-note sync working; this one is what makes a
    // whole-vault sync converge in one command instead of two.
    if (this.plugin.settings.syncLinks) {
      for (const file of files) {
        await this.syncLinks(file);
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
