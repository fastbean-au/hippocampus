# Design record

Why Hippocampus is built the way it is: the alternatives that were rejected, the incidents behind
each guard, and the invariants a change has to keep. Until TODO-3 item 176 this lived in
`CLAUDE.md`; it was moved here verbatim so that file could be short. Read the page for a subsystem
before changing it.

| Page | Covers |
| :--- | :----- |
| [Commands and tooling](tooling.md) | every command and script with its notes, the demo |
| [The service binary](service.md) | `cmd/hippocampus`: bootstrap, auth wiring, the console, RED metrics, the alert rules; `stats/` |
| [The configuration wizard](config-wizard.md) | `cmd/config-wizard` |
| [Consolidation and forgetting transparency](consolidation.md) | the sleep cycle, preview, explain, retention, ancillary storage, footprint, the forgotten log, the callback backlog policy |
| [The deployment topology view](topology.md) | `GetTopology`: probes, peers, declared and observed components |
| [Group scoping, predicate deletion and purge](scope-and-deletion.md) | `hippocampus/scope.go`, `predicate.go`, `Purge` |
| [The storage layer](storage.md) | `db/`: dialects, the schema ledger, compression, the drivers; the link graph |
| [The contract](contract.md) | `contract/`, `types/` |
| [Content search](search.md) | `search/`: the store's own index, ranking, semantic search, OpenSearch |
| [Summarisation and embedding](llm.md) | `summarise/`, `embed/` |
| [Export, import and transfer](archive.md) | `archive/`, the transfer RPCs |
| [Authentication and authorisation](auth.md) | `auth/` |
| [The integrations](integrations.md) | MCP, CLI, Python, broker bridges, ingestor, object-storage agents, `dial/` |
| [Observability](observability.md) | `observability/` |
| [Conventions](conventions.md) | the conventions section as it stood |

These pages are a record, and some of their counts and details describe the moment they were
written. The code and its tests are authoritative; where they disagree, fix the page.
