package main

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/summarise"
)

// Model providers. llm.provider / llm.embedding.provider select which client is built; the values
// are part of the configuration's contract, so they are a small closed set spelled once here and
// checked by configProblems rather than by whichever switch happens to read them.
const (
	providerOllama = "ollama"
	providerOpenAI = "openai"
)

// defaultOllamaAddress is the address both halves default to, and is also what configProblems
// compares against to catch the one configuration that is wrong in a way nothing else would notice:
// an openai provider left pointing at the Ollama default, which answers 404 to every call.
const defaultOllamaAddress = "http://localhost:11434"

// llmAliases maps each llm.* key to the ollama.* key it replaced in v0.42.0. The block was renamed
// because it stopped being about Ollama: "ollama.address" naming an Azure endpoint is a setting that
// reads as a mistake, and an operator looking for how to point the service at their own gateway
// would not think to look under it.
//
// llm.provider, llm.apiKey and their embedding counterparts have no entry here because they are new;
// there is nothing for them to be aliased from.
var llmAliases = []struct {
	key    string
	legacy string
}{
	{key: "llm.enabled", legacy: "ollama.enabled"},
	{key: "llm.address", legacy: "ollama.address"},
	{key: "llm.model", legacy: "ollama.model"},
	{key: "llm.timeoutSeconds", legacy: "ollama.timeoutSeconds"},
	{key: "llm.maxMemories", legacy: "ollama.maxMemories"},
	{key: "llm.promptCharLimit", legacy: "ollama.promptCharLimit"},
	{key: "llm.systemPrompt", legacy: "ollama.systemPrompt"},
	{key: "llm.temperature", legacy: "ollama.temperature"},
	{key: "llm.autoSummarise", legacy: "ollama.autoSummarise"},
	{key: "llm.embedding.enabled", legacy: "ollama.embedding.enabled"},
	{key: "llm.embedding.address", legacy: "ollama.embedding.address"},
	{key: "llm.embedding.model", legacy: "ollama.embedding.model"},
	{key: "llm.embedding.timeoutSeconds", legacy: "ollama.embedding.timeoutSeconds"},
	{key: "llm.embedding.batchSize", legacy: "ollama.embedding.batchSize"},
	{key: "llm.embedding.maxTextBytes", legacy: "ollama.embedding.maxTextBytes"},
	{key: "llm.embedding.dimensions", legacy: "ollama.embedding.dimensions"},
}

// llmProvider reads a provider key, treating an unset or blank value as the default provider rather
// than as a distinct third state. setStartupDefaults fills both provider keys, so a blank one only
// arises where the defaults did not run - a test driving run() directly, or a config that sets the
// key to an empty string - and in both cases the honest reading is "the operator did not choose",
// which is what the default means.
func llmProvider(key string) string {
	provider := strings.TrimSpace(viper.GetString(key))

	if provider == "" {
		return providerOllama
	}

	return provider
}

// resolveLLMAliases copies any ollama.* key an operator still sets onto its llm.* replacement, and
// returns the legacy keys it honoured so the caller can say so once. Nothing downstream reads an
// ollama.* key: every consumer - here, hippocampus/server.go and hippocampus/topology.go - reads the
// new name, and this is the single point that makes the old one mean it.
//
// It MUST run before setStartupDefaults. viper.IsSet answers true for a key carrying only a default,
// so once the llm.* defaults are in place there is no way left to ask whether an operator set one -
// and the alias would then never win, silently, against the very keys it exists to keep working.
//
// The new key is written with viper.Set (the highest precedence) rather than SetDefault, which is
// safe precisely because the copy only happens when the new key is unset: an explicit llm.* value
// always beats a legacy one, and the two being set together is the operator mid-migration.
func resolveLLMAliases() []string {
	var honoured []string

	for _, alias := range llmAliases {
		if viper.IsSet(alias.key) || !viper.IsSet(alias.legacy) {
			continue
		}

		viper.Set(alias.key, viper.Get(alias.legacy))

		honoured = append(honoured, alias.legacy)
	}

	return honoured
}

// backfillEmbedderFromViper builds the embedder the --backfill-search mode re-embeds with, or the
// no-op when semantic search is not configured.
//
// It fails the run rather than degrading to a keyword-only rebuild: a backfill is the tool an
// operator reaches for precisely when the index is wrong, and one that silently produced an index
// without vectors would look like it had fixed things.
func backfillEmbedderFromViper() embed.Embedder {
	if !viper.GetBool("llm.embedding.enabled") {
		return embed.NewNoop()
	}

	embedder, err := newEmbedder(llmProvider("llm.embedding.provider"), embedderConfigFromViper())
	if err != nil {
		log.Fatalf("failed to initialise the embedder for the backfill: %s", err.Error())
	}

	log.Infof("backfill will re-embed with model '%s'", embedder.Model())

	return embedder
}

// embedderConfigFromViper reads the llm.embedding.* keys into an embed.Config. Both providers take
// the same config - the provider only selects which client is built from it - so the startup path
// and the --backfill-search mode share one reading of it and cannot drift.
func embedderConfigFromViper() embed.Config {

	return embed.Config{
		Address:      viper.GetString("llm.embedding.address"),
		Model:        viper.GetString("llm.embedding.model"),
		APIKey:       viper.GetString("llm.embedding.apiKey"),
		Timeout:      time.Duration(viper.GetInt("llm.embedding.timeoutSeconds")) * time.Second,
		BatchSize:    viper.GetInt("llm.embedding.batchSize"),
		MaxTextBytes: viper.GetInt("llm.embedding.maxTextBytes"),
		Dimensions:   viper.GetInt("llm.embedding.dimensions"),
	}
}

// newEmbedder builds the embedder a provider names. The unknown case falls through to Ollama rather
// than failing, because configProblems has already refused an unrecognised provider by the time any
// caller reaches here - and a second, differently-worded refusal at this depth would only ever be
// reached by a code path that had skipped validation.
func newEmbedder(provider string, cfg embed.Config) (embed.Embedder, error) {
	if provider == providerOpenAI {
		return embed.NewOpenAI(cfg)
	}

	return embed.NewOllama(cfg)
}

// llmProblems checks the two model-provider blocks. Both are optional and both fail late by nature -
// a summariser is only reached by an RPC, and an embedder's failures are swallowed by a best-effort
// index - so a configuration mistake here is otherwise invisible until somebody notices that a
// feature has quietly never worked.
func llmProblems() []error {
	var problems []error

	for _, block := range []struct {
		enabled  string
		provider string
		address  string
	}{
		{enabled: "llm.enabled", provider: "llm.provider", address: "llm.address"},
		{enabled: "llm.embedding.enabled", provider: "llm.embedding.provider", address: "llm.embedding.address"},
	} {
		if !viper.GetBool(block.enabled) {
			continue
		}

		provider := llmProvider(block.provider)

		switch provider {

		case providerOllama:
			// Nothing further: the native client's defaults are this block's defaults.

		case providerOpenAI:
			// The defaults describe the Ollama provider, so an openai one that never named its own
			// endpoint is pointed at an Ollama port. It would answer 404 to every call, which
			// presents as a model server that is up and refusing rather than as a setting nobody
			// filled in. Ollama's own OpenAI-compatible endpoint is a legitimate target here, but it
			// lives at /v1 and so does not collide with this.
			if strings.TrimRight(viper.GetString(block.address), "/") == defaultOllamaAddress {
				problems = append(problems, fmt.Errorf(
					"%s is '%s' but %s is still the Ollama default '%s': set it to the base URL of your endpoint, including its version path (e.g. https://api.openai.com/v1, or %s/v1 for Ollama's own OpenAI-compatible API)",
					block.provider, providerOpenAI, block.address, defaultOllamaAddress, defaultOllamaAddress,
				))
			}

		default:
			problems = append(problems, fmt.Errorf(
				"%s must be '%s' or '%s', got '%s'",
				block.provider, providerOllama, providerOpenAI, provider,
			))

		}
	}

	return problems
}

// summariserConfig is what initSummariser needs to build the optional summariser.
type summariserConfig struct {
	Enabled  bool
	Provider string
	Config   summarise.Config
}

// embedderSetup is what initEmbedder needs to build the optional embedder.
type embedderSetup struct {
	Enabled bool

	// OpenSearchEnabled is read alongside, since an embedder without OpenSearch is refused.
	OpenSearchEnabled bool

	Provider string
	Config   embed.Config
}

// setLLMDefaults applies the llm.* defaults.
func setLLMDefaults() {
	// Generation. llm.provider selects the client: "ollama" (the default, and the native
	// /api/generate API) or "openai" (any endpoint speaking the OpenAI chat-completions API -
	// OpenAI, Azure, vLLM, LiteLLM, OpenRouter, a corporate gateway, and Ollama's own /v1). The
	// defaults below describe the default provider, which is why the address is Ollama's; an
	// openai-provider deployment must name its own base URL, and configProblems refuses one that
	// has not.
	viper.SetDefault("llm.provider", providerOllama)
	viper.SetDefault("llm.address", defaultOllamaAddress)
	viper.SetDefault("llm.model", "llama3.2")
	viper.SetDefault("llm.timeoutSeconds", 120)

	// Semantic-search embedding. Off by default and, unlike the summariser, gated on OpenSearch:
	// the k-NN index is the only vector store this service has. It carries its own provider,
	// address, key and model because nothing requires the two halves to share an endpoint - and
	// generating against a hosted model while embedding against a local Ollama is both reasonable
	// and much cheaper.
	viper.SetDefault("llm.embedding.provider", providerOllama)
	viper.SetDefault("llm.embedding.address", defaultOllamaAddress)
	viper.SetDefault("llm.embedding.model", "nomic-embed-text")
	viper.SetDefault("llm.embedding.timeoutSeconds", 30)
	viper.SetDefault("llm.embedding.batchSize", 32)
	viper.SetDefault("llm.embedding.dimensions", 768)
}

// summariserConfigFromViper reads the llm.* generation keys.
func summariserConfigFromViper() summariserConfig {
	log.Trace("func() summariserConfigFromViper")

	return summariserConfig{
		Enabled:  viper.GetBool("llm.enabled"),
		Provider: llmProvider("llm.provider"),
		Config: summarise.Config{
			Address:         viper.GetString("llm.address"),
			Model:           viper.GetString("llm.model"),
			APIKey:          viper.GetString("llm.apiKey"),
			Timeout:         time.Duration(viper.GetInt("llm.timeoutSeconds")) * time.Second,
			MaxBodies:       viper.GetInt("llm.maxMemories"),
			PromptCharLimit: viper.GetInt("llm.promptCharLimit"),
			SystemPrompt:    viper.GetString("llm.systemPrompt"),
			Temperature:     viper.GetFloat64("llm.temperature"),
		},
	}
}

// initSummariser builds the optional embedded-LLM summariser. Disabled by default: the no-op
// summariser makes SummariseMemories fail with FAILED_PRECONDITION and the sleep cycle's
// auto-summarisation a no-op, so the service behaves exactly as it did before. Construction only
// fails on unusable configuration (a missing address/model) - an unreachable model server must not
// prevent startup, since summarisation is optional and best-effort.
func initSummariser(cfg summariserConfig) (summarise.Summariser, error) {
	log.Trace("func() initSummariser")

	if !cfg.Enabled {
		return summarise.NewNoop(), nil
	}

	log.Debugf("initialising the %s summariser", cfg.Provider)

	var (
		summariser summarise.Summariser
		err        error
	)

	switch cfg.Provider {

	case providerOpenAI:
		summariser, err = summarise.NewOpenAI(cfg.Config)

	default:
		summariser, err = summarise.NewOllama(cfg.Config)

	}

	if err != nil {
		return nil, fmt.Errorf("failed to initialise the %s summariser: %w", cfg.Provider, err)
	}

	log.Debugf("%s summariser initialised", cfg.Provider)

	return summariser, nil
}

// embedderSetupFromViper reads the llm.embedding.* keys.
func embedderSetupFromViper() embedderSetup {
	log.Trace("func() embedderSetupFromViper")

	return embedderSetup{
		Enabled:           viper.GetBool("llm.embedding.enabled"),
		OpenSearchEnabled: viper.GetBool("opensearch.enabled"),
		Provider:          llmProvider("llm.embedding.provider"),
		Config:            embedderConfigFromViper(),
	}
}

// initEmbedder builds the optional text embedder backing semantic search. Disabled by default, and
// refused outright without OpenSearch: the embedder only produces vectors, and OpenSearch's k-NN
// index is the only place this service can put them, so enabling one without the other would cost
// every write an embedding call whose result nothing could ever search. Failing at startup says
// that once, where a warning would leave it to be inferred from search results that never improve.
func initEmbedder(cfg embedderSetup) (embed.Embedder, error) {
	log.Trace("func() initEmbedder")

	if !cfg.Enabled {
		return embed.NewNoop(), nil
	}

	if !cfg.OpenSearchEnabled {
		return nil, fmt.Errorf("llm.embedding.enabled requires opensearch.enabled: semantic search stores its vectors in the OpenSearch k-NN index, and no other backend provides one")
	}

	log.Debugf("initialising the %s embedder", cfg.Provider)

	embedder, err := newEmbedder(cfg.Provider, cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("failed to initialise the %s embedder: %w", cfg.Provider, err)
	}

	log.Infof("semantic search enabled, embedding with model '%s' via %s", embedder.Model(), cfg.Provider)

	return embedder, nil
}
