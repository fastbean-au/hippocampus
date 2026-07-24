# Obsidian integration

Hippocampus works well as an **LLM context & memory layer** for an Obsidian vault. A personal
knowledge base accumulates a long tail of daily notes; most are noise ("fixed typo in README") and a
few are durable facts. Feeding all of it to an AI assistant bloats the context window with the
noise. Hippocampus keeps what matters — reinforcing notes that get recalled and letting the rest
decay under a finite budget — so the assistant reads a distilled memory instead of the raw pile.

There are two ways to connect a vault, and they compose:

1. **The Obsidian plugin** (`integrations/obsidian/`) — a first-party plugin that talks directly to
   the HTTP/JSON gateway. Store notes/selections as memories, search and recall them, and optionally
   auto-sync a folder.
2. **The MCP bridge** (`cmd/hippocampus-mcp`) — for AI assistants (Claude Desktop/Code, or an
   MCP-capable Obsidian plugin) that already speak the Model Context Protocol and want Hippocampus as
   their bounded memory store.

---

## 1. The Obsidian plugin

The plugin lives at [`integrations/obsidian/`](../integrations/obsidian/); its
[README](../integrations/obsidian/README.md) has the full build/install/configure walkthrough. In
short:

- It calls the Hippocampus **HTTP gateway** (`gateway.port` must be non-zero on the service — the
  `docker/config.sqlite.json` demo uses `8080`; the root `config.json` ships with it disabled). It
  uses Obsidian's `requestUrl`, so it is not blocked by renderer CORS and needs no server-side
  changes.
- Bearer-token auth and OS-trusted TLS are supported; `requestUrl` has no
  `insecureSkipVerify`, so use plaintext localhost or a properly CA-signed endpoint.

### What it does

- **Store note / selection as memory** — significance comes from a `significance:` frontmatter key
  (falling back to a configurable default); the `group` label comes from the note's top-level
  folder, a frontmatter key, or a fixed value.
- **Search memories and insert results** — content search (needs the service's `opensearch.enabled`
  index), optionally reinforcing the matches.
- **Auto-sync a folder** — notes under a configured folder are pushed in as they are edited,
  idempotently (one memory per note path, updated in place, re-created if consolidation has since
  forgotten it). This is what lets the sleep cycle prune the noise: notes you keep touching are
  reinforced and survive; notes you never revisit fade.

### Why the shape fits

- **Reinforcement through recall** — when you (or an assistant) repeatedly reference an old project
  note, recalling it resets its decay clock and raises its effective significance, so it survives.
- **Sleep & consolidation** — instead of an ever-growing vector index full of trivial daily logs,
  Hippocampus consolidates low-value memories away and can condense a pile of related-but-quiet
  memories into a single summary (see [consolidation](consolidation.md)).

---

## 2. The MCP bridge

If your assistant already speaks MCP, point it at `cmd/hippocampus-mcp` instead of (or alongside)
the plugin. The bridge exposes a curated, safe tool subset — `store_memory`, `recall_memories`,
`search_memories`, `list_memories`, `create_event`, `list_events`,
`get_summarization_candidates` — and deliberately omits the destructive/admin RPCs, so a model
cannot wipe or exfiltrate the store. See the [MCP server guide](mcp.md) for the tool reference,
transports (stdio/HTTP), auth, and TLS.

Typical setups:

- **Claude Code / Claude Desktop operating on a vault** — run `hippocampus-mcp` (stdio) against your
  running instance and let the assistant use it as long-term memory while it works over your notes.
- **An MCP-capable Obsidian AI plugin** — configure it to launch/connect to `hippocampus-mcp` the
  same way it would any other MCP server.

The two routes share one store: the plugin can populate memories from your notes while an MCP-based
assistant recalls and reinforces them.
