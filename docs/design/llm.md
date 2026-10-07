# Summarisation and embedding

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `summarise/`

`summarise/` — the optional embedded-LLM summariser (`llm.enabled`, off by default;
`summarise.Summariser` interface with a no-op and an `Ollama` implementation). The `Ollama`
impl is a small hand-rolled HTTP client to Ollama's `POST /api/generate` (`stream:false`, no new
module dependency), bounding the prompt by body count and total characters and never sending
binary bodies. It is the one component with visibility into memory content. Wired into
`hippocampus.Server` via the `summarise.Summariser` field (nil-safe through `summariser()`, like
`searchIdx()`): the `SummariseMemories` RPC reads an event's memories, generates a summary, and
replaces them through the same `insertSummary` path `ReplaceMemoriesWithSummary` uses; the sleep
cycle's `autoSummariseCandidates` (gated on `llm.autoSummarise`, off by default) does the same
for the scan's candidates, best-effort. All viper reads stay in main.go, which builds the no-op or
`Ollama` or `OpenAI` from the `llm.*` keys. **Both packages carry two clients** since item 97: the
native Ollama API, and an `openai` one speaking `POST {base}/chat/completions` /
`POST {base}/embeddings`, which covers OpenAI, Azure, vLLM, llama.cpp, LiteLLM, OpenRouter, a
corporate gateway - and Ollama's own `/v1`. Four things carry that. `llm.provider` and
`llm.embedding.provider` are **independent**, because nothing requires the two halves to share an
endpoint. The prompt bounding (`summarise.buildPrompt`) and the embedding truncation
(`embed.truncate`) are **package functions rather than methods**, so how much of the store leaves
the process cannot become a per-provider accident. `address` is taken **exactly as given** under
`openai` (so it normally ends in `/v1`, the `base_url` convention every SDK uses), and
`configProblems` refuses an `openai` provider still on the Ollama default, which would 404 every
call and read as a server that is up and refusing. And the OpenAI embedder **places vectors by the
response's `index`**, not by arrival order - that API does not promise order, and appending would
mis-assign every vector in a batch silently. The `ollama.*` keys are a deprecated alias block
resolved by `resolveLLMAliases` in `main.go` **before `setStartupDefaults`** (viper's `IsSet`
answers true for a defaulted key, so afterwards there is no way left to ask whether an operator
set one), reported by `--check-config`'s `deprecated` field and held by
`TestLLMAliasesCoverEveryKey`. An optional `ollama` compose profile ships it alongside the
service. Deliberately off the MCP tool surface (it deletes memories, like the omitted
`ReplaceMemoriesWithSummary`).
