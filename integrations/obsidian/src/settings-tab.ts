import { App, PluginSettingTab, Setting } from "obsidian";

import type HippocampusPlugin from "./main";
import type { GroupSource } from "./settings";

export class HippocampusSettingTab extends PluginSettingTab {
  private readonly plugin: HippocampusPlugin;

  constructor(app: App, plugin: HippocampusPlugin) {
    super(app, plugin);
    this.plugin = plugin;
  }

  display(): void {
    const { containerEl } = this;

    containerEl.empty();

    const s = this.plugin.settings;

    new Setting(containerEl).setName("Connection").setHeading();

    new Setting(containerEl)
      .setName("Server URL")
      .setDesc(
        "Base URL of the Hippocampus HTTP gateway (requires gateway.port > 0 on the service).",
      )
      .addText((text) =>
        text
          .setPlaceholder("http://127.0.0.1:8080")
          .setValue(s.baseUrl)
          .onChange(async (value) => {
            s.baseUrl = value.trim();
            await this.plugin.saveSettings();
          }),
      );

    new Setting(containerEl)
      .setName("Bearer token")
      .setDesc(
        "Sent as 'Authorization: Bearer <token>' when the service requires auth. Leave empty otherwise.",
      )
      .addText((text) => {
        text
          .setPlaceholder("(none)")
          .setValue(s.token)
          .onChange(async (value) => {
            s.token = value.trim();
            await this.plugin.saveSettings();
          });
        text.inputEl.type = "password";
      });

    new Setting(containerEl).addButton((btn) =>
      btn
        .setButtonText("Test connection")
        .onClick(() => this.plugin.testConnection()),
    );

    new Setting(containerEl).setName("Memory mapping").setHeading();

    new Setting(containerEl)
      .setName("Default significance")
      .setDesc(
        "Significance used when a note has no significance frontmatter. Higher survives longer.",
      )
      .addText((text) =>
        text.setValue(String(s.defaultSignificance)).onChange(async (value) => {
          const n = Number(value);
          s.defaultSignificance = Number.isFinite(n) ? n : 0;
          await this.plugin.saveSettings();
        }),
      );

    new Setting(containerEl)
      .setName("Significance frontmatter key")
      .setDesc(
        "A note's frontmatter value under this key overrides the default significance.",
      )
      .addText((text) =>
        text.setValue(s.significanceFrontmatterKey).onChange(async (value) => {
          s.significanceFrontmatterKey = value.trim();
          await this.plugin.saveSettings();
        }),
      );

    new Setting(containerEl)
      .setName("Group source")
      .setDesc("How a memory's group label is derived from a note.")
      .addDropdown((dd) =>
        dd
          .addOption("folder", "Top-level folder")
          .addOption("frontmatter", "Frontmatter key")
          .addOption("fixed", "Fixed value")
          .setValue(s.groupSource)
          .onChange(async (value) => {
            s.groupSource = value as GroupSource;
            await this.plugin.saveSettings();
            this.display();
          }),
      );

    if (s.groupSource === "frontmatter") {
      new Setting(containerEl)
        .setName("Group frontmatter key")
        .addText((text) =>
          text.setValue(s.groupFrontmatterKey).onChange(async (value) => {
            s.groupFrontmatterKey = value.trim();
            await this.plugin.saveSettings();
          }),
        );
    }

    if (s.groupSource === "fixed") {
      new Setting(containerEl).setName("Group value").addText((text) =>
        text.setValue(s.groupFixedValue).onChange(async (value) => {
          s.groupFixedValue = value.trim();
          await this.plugin.saveSettings();
        }),
      );
    }

    new Setting(containerEl)
      .setName("Metadata frontmatter keys")
      .setDesc(
        "Comma-separated frontmatter keys copied onto each memory's metadata. Named " +
          "explicitly rather than copying all frontmatter, so plugin bookkeeping and dates " +
          "stay out of the labels.",
      )
      .addText((text) =>
        text
          .setPlaceholder("project, author")
          .setValue((s.metadataFrontmatterKeys || []).join(", "))
          .onChange(async (value) => {
            s.metadataFrontmatterKeys = value
              .split(",")
              .map((k) => k.trim())
              .filter((k) => k !== "");
            await this.plugin.saveSettings();
          }),
      );

    new Setting(containerEl)
      .setName("Fixed metadata")
      .setDesc(
        "One key=value per line, stamped on every memory this plugin stores.",
      )
      .addTextArea((text) =>
        text
          .setPlaceholder("source=obsidian\nvault=work")
          .setValue(s.metadataFixed)
          .onChange(async (value) => {
            s.metadataFixed = value;
            await this.plugin.saveSettings();
          }),
      );

    new Setting(containerEl)
      .setName("Strip frontmatter from body")
      .setDesc(
        "Remove a note's leading YAML frontmatter before storing it as a memory body.",
      )
      .addToggle((toggle) =>
        toggle.setValue(s.stripFrontmatter).onChange(async (value) => {
          s.stripFrontmatter = value;
          await this.plugin.saveSettings();
        }),
      );

    new Setting(containerEl).setName("Search").setHeading();

    new Setting(containerEl).setName("Result limit").addText((text) =>
      text.setValue(String(s.searchLimit)).onChange(async (value) => {
        const n = Number(value);
        s.searchLimit = Number.isFinite(n) && n > 0 ? n : 10;
        await this.plugin.saveSettings();
      }),
    );

    new Setting(containerEl)
      .setName("Reinforce matches")
      .setDesc(
        "Recall (reinforce) matched memories when searching, resetting their decay clock.",
      )
      .addToggle((toggle) =>
        toggle.setValue(s.searchReinforce).onChange(async (value) => {
          s.searchReinforce = value;
          await this.plugin.saveSettings();
        }),
      );

    new Setting(containerEl).setName("Auto-sync").setHeading();

    new Setting(containerEl)
      .setName("Enable auto-sync")
      .setDesc(
        "Push notes under the sync folder into Hippocampus as they are edited, keeping each note's memory up to date.",
      )
      .addToggle((toggle) =>
        toggle.setValue(s.autoSync).onChange(async (value) => {
          s.autoSync = value;
          await this.plugin.saveSettings();
          this.plugin.refreshSyncRegistration();
        }),
      );

    new Setting(containerEl)
      .setName("Sync folder")
      .setDesc(
        "Vault-relative folder whose notes are synced. Empty means the whole vault.",
      )
      .addText((text) =>
        text.setValue(s.syncFolder).onChange(async (value) => {
          s.syncFolder = value.trim().replace(/^\/+|\/+$/g, "");
          await this.plugin.saveSettings();
        }),
      );

    new Setting(containerEl)
      .setName("Debounce (seconds)")
      .setDesc("How long after the last edit to a note before it is synced.")
      .addText((text) =>
        text.setValue(String(s.syncDebounceSeconds)).onChange(async (value) => {
          const n = Number(value);
          s.syncDebounceSeconds = Number.isFinite(n) && n >= 0 ? n : 5;
          await this.plugin.saveSettings();
        }),
      );
  }
}
