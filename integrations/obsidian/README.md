# Hippocampus Memory — Obsidian plugin

Use a [Hippocampus](../../README.md) instance as a **bounded, self-consolidating memory layer** for
your vault. Store notes (or selections) as memories, search and recall them from inside a note, and
let Hippocampus's sleep/consolidation cycle forget low-value noise while the high-value facts
survive — so an AI assistant working over your vault reads a distilled memory instead of thousands
of raw daily entries.

The plugin talks to Hippocampus over its **HTTP/JSON `/v1` gateway** using Obsidian's `requestUrl`
(which is not subject to renderer CORS). It needs no gRPC and does not use `hippocampus-mcp` — that
is a separate route for MCP-capable AI plugins (see [`docs/mcp.md`](../../docs/mcp.md) and
[`docs/obsidian.md`](../../docs/obsidian.md)).

## Requirements

- A running Hippocampus instance with the HTTP gateway enabled — set `gateway.port` to a non-zero
  value in its config (the shipped `docker/config.sqlite.json` uses `8080`; the root `config.json`
  ships with the gateway **disabled** at `0`).
- Node.js + npm to build the plugin from source (below).

## Build

```bash
cd integrations/obsidian
npm install
npm run build     # tsc typecheck + esbuild bundle -> main.js
npm test          # unit tests for the wire parsing and note→memory mapping
```

`npm run dev` runs esbuild in watch mode for iterative development.

## Install into a vault

Copy `manifest.json`, `styles.css`, and the built `main.js` into
`<your-vault>/.obsidian/plugins/hippocampus/`, then enable **Hippocampus Memory** under
*Settings → Community plugins*. (For development you can symlink the plugin folder there and rely on
`npm run dev`.)

## Configure

Open *Settings → Hippocampus Memory*:

- **Server URL** — e.g. `http://127.0.0.1:8080`.
- **Bearer token** — only if the service has auth enabled; sent as `Authorization: Bearer <token>`.
- **Default significance** and **significance frontmatter key** — a note's `significance:`
  frontmatter overrides the default; higher significance survives longer.
- **Group source** — how a memory's `group` label is derived: the note's top-level folder, a
  frontmatter key, or a fixed value.
- **Strip frontmatter from body** — drop a note's leading YAML before storing it.
- **Search** — result limit and whether searching also *reinforces* (recalls) the matches.
- **Auto-sync** — see below.

Use **Test connection** to confirm the URL/token reach a live gateway.

## Commands

| Command | What it does |
| :-- | :-- |
| **Store current note as memory** | Store (or idempotently update) the active note as one memory, keyed by its path. |
| **Store selection as memory** | Store the current editor selection as a standalone memory. |
| **Search memories and insert results** | Prompt for a query, search the content index, insert the matches at the cursor. |
| **Sync folder now** | Run the auto-sync pass once over the configured folder. |
| **Test connection** | Ping `/healthz`. |

"Search" requires the service's optional content-search index (`opensearch.enabled`).

## Auto-sync

When enabled, notes under the configured **sync folder** (empty = whole vault) are pushed into
Hippocampus as you edit them, debounced per note. Sync is **idempotent**: the plugin remembers each
note's memory id (in its saved data) and updates that memory on re-save rather than creating
duplicates. Renames follow the note; deletes remove the memory. If the consolidation cycle has
already forgotten a note's memory, the next sync re-creates it.

This lets the decay cycle do its job: reinforced/important notes persist, while notes you never
touch again fade under the store's capacity budget.

## TLS / localhost note

`requestUrl` trusts only the operating system's certificate store and has **no**
`insecureSkipVerify` equivalent. Use plaintext `http://` against localhost, or put the gateway
behind a properly CA-signed certificate (or a TLS-terminating reverse proxy). A self-signed cert the
OS does not trust will not work from the plugin.
