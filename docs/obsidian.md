# Obsidian integration

Hippocampus works well as an **LLM context & memory layer** for an Obsidian vault. A personal
knowledge base accumulates a long tail of daily notes; most are noise ("fixed typo in README") and a
few are durable facts. Feeding all of it to an AI assistant bloats the context window with the
noise. Hippocampus keeps what matters — reinforcing notes that get recalled and letting the rest
decay under a finite budget — so the assistant reads a distilled memory instead of the raw pile.

There are two ways to connect a vault, and they compose:

1. **The Obsidian plugin** — a first-party plugin that talks directly to the HTTP/JSON gateway.
   Store notes/selections as memories, search and recall them, mirror the vault's `[[wikilinks]]`
   into the memory graph, and optionally auto-sync a folder. It lives in its own repository,
   [**fastbean-au/hippocampus-obsidian**](https://github.com/fastbean-au/hippocampus-obsidian),
   which is where its documentation, releases and install instructions are.
2. **The MCP bridge** (`integrations/mcp`) — for AI assistants (Claude Desktop/Code, or an
   MCP-capable Obsidian plugin) that already speak the Model Context Protocol and want Hippocampus as
   their bounded memory store.

---

## 1. The Obsidian plugin

The plugin is not in this repository. It has its own repository and its own version line, because
Obsidian's community registry lists a repository whose **root** holds `manifest.json`, which a
monorepo cannot offer — see TODO-2 item 113 for the reasoning, and
[`hippocampus-obsidian`](https://github.com/fastbean-au/hippocampus-obsidian) for the plugin itself.

Two things about it are worth knowing from this side, because they are properties of the **service**
rather than of the plugin:

- It calls the Hippocampus **HTTP gateway**, so `gateway.port` must be non-zero (the
  `deploy/compose/config.sqlite.json` demo uses `8080`; the root `config.json` ships with it
  disabled). It uses Obsidian's `requestUrl`, so it is not blocked by renderer CORS and needs no
  server-side changes.
- Its wire handling is hand-written against the `/v1` gateway rather than generated from
  `contract/hippocampus.proto`, so it holds itself to a vendored copy of
  `contract/hippocampus.swagger.json` and declares a minimum service version. A contract change that
  moves a route or drops a field turns that repository's CI red, not this one's.

### Why the shape fits

- **The link graph is the same idea on both sides** — Obsidian's entire model is `[[wikilinks]]`, and
  Hippocampus raises the effective significance of **both** ends of a link (`log1p`-damped, so a hub
  note cannot become unforgettable by being linked a thousand times). A well-connected note is
  therefore kept for the reason it deserves to be, without anybody assigning it a significance.
- **Reinforcement through recall** — when you (or an assistant) repeatedly reference an old project
  note, recalling it resets its decay clock and raises its effective significance, so it survives.
- **Sleep & consolidation** — instead of an ever-growing vector index full of trivial daily logs,
  Hippocampus consolidates low-value memories away and can condense a pile of related-but-quiet
  memories into a single summary (see [consolidation](consolidation.md)).

---

## 2. The MCP bridge

If your assistant already speaks MCP, point it at `integrations/mcp` instead of (or alongside)
the plugin. The bridge exposes a curated, safe tool subset — `store_memory`, `recall_memories`,
`search_memories`, `list_memories`, `create_event`, `list_events`,
`get_summarisation_candidates` — and deliberately omits the destructive/admin RPCs, so a model
cannot wipe or exfiltrate the store. See the [MCP server guide](mcp.md) for the tool reference,
transports (stdio/HTTP), auth, and TLS.

Typical setups:

- **Claude Code / Claude Desktop operating on a vault** — run `hippocampus-mcp` (stdio) against your
  running instance and let the assistant use it as long-term memory while it works over your notes.
- **An MCP-capable Obsidian AI plugin** — configure it to launch/connect to `hippocampus-mcp` the
  same way it would any other MCP server.

The two routes share one store: the plugin can populate memories from your notes while an MCP-based
assistant recalls and reinforces them.
