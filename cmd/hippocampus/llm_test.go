package main

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestResolveLLMAliases_HonoursTheLegacyBlock verifies an ollama.* key an operator still sets is
// read into its llm.* replacement, and reported so the caller can warn about it once.
func TestResolveLLMAliases_HonoursTheLegacyBlock(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("ollama.enabled", true)
	viper.Set("ollama.address", "http://legacy:11434")
	viper.Set("ollama.model", "llama3.2")

	honoured := resolveLLMAliases()

	if !viper.GetBool("llm.enabled") || viper.GetString("llm.address") != "http://legacy:11434" || viper.GetString("llm.model") != "llama3.2" {
		t.Error("the legacy keys were not read into their replacements")
	}

	if len(honoured) != 3 {
		t.Errorf("honoured = %v, want the three legacy keys that were set", honoured)
	}

	for _, key := range honoured {
		if !strings.HasPrefix(key, "ollama.") {
			t.Errorf("honoured %q, want the legacy name so the warning names what to change", key)
		}
	}
}

// TestResolveLLMAliases_NewKeyWins is the migration case: both blocks set at once, which is what a
// deployment looks like part-way through the rename. The explicit new value must win, or renaming a
// key would silently have no effect until the old one was also deleted.
func TestResolveLLMAliases_NewKeyWins(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("ollama.address", "http://legacy:11434")
	viper.Set("llm.address", "https://api.openai.com/v1")

	honoured := resolveLLMAliases()

	if got := viper.GetString("llm.address"); got != "https://api.openai.com/v1" {
		t.Errorf("llm.address = %q, want the explicitly configured value to win", got)
	}

	for _, key := range honoured {
		if key == "ollama.address" {
			t.Error("reported ollama.address as honoured when the new key overrode it")
		}
	}
}

// TestResolveLLMAliases_Quiet verifies a configuration using none of the legacy names reports
// nothing, so the deprecation warning is silent for everyone who has already migrated.
func TestResolveLLMAliases_Quiet(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("llm.enabled", true)

	if honoured := resolveLLMAliases(); len(honoured) != 0 {
		t.Errorf("honoured = %v, want nothing for a configuration using only the new names", honoured)
	}
}

// TestResolveLLMAliases_MustPrecedeTheDefaults pins the ordering constraint the whole alias layer
// rests on. viper.IsSet answers true for a key carrying only a default, so once setStartupDefaults
// has run there is no way left to ask whether an operator set an llm.* key - and the legacy value
// would then never be read, silently, against exactly the configurations the aliases exist for.
func TestResolveLLMAliases_MustPrecedeTheDefaults(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("ollama.address", "http://legacy:11434")

	setStartupDefaults()

	if honoured := resolveLLMAliases(); len(honoured) != 0 {
		t.Fatal("the aliases resolved after the defaults - this test no longer pins anything")
	}

	if got := viper.GetString("llm.address"); got != defaultOllamaAddress {
		t.Errorf("llm.address = %q; want the default, demonstrating the legacy value was lost", got)
	}
}

// TestLLMProblems covers the provider validation on both blocks. Each is optional and each fails
// late by nature - a summariser is only reached by an RPC, an embedder's failures are swallowed by
// a best-effort index - so a mistake here is otherwise invisible until somebody notices a feature
// has quietly never worked.
func TestLLMProblems(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func()
		want  string
	}{
		{
			name:  "disabled blocks are not checked",
			setup: func() { viper.Set("llm.provider", "nonsense") },
		},
		{
			name: "the default provider is valid",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.provider", providerOllama)
				viper.Set("llm.address", defaultOllamaAddress)
			},
		},
		{
			name: "an unset provider is treated as the default",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.address", defaultOllamaAddress)
			},
		},
		{
			name: "an unknown provider is refused",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.provider", "anthropic")
			},
			want: "llm.provider must be 'ollama' or 'openai', got 'anthropic'",
		},
		{
			name: "an openai provider on the ollama default address is refused",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.provider", providerOpenAI)
				viper.Set("llm.address", defaultOllamaAddress)
			},
			want: "still the Ollama default",
		},
		{
			name: "ollama's own openai-compatible endpoint is accepted",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.provider", providerOpenAI)
				viper.Set("llm.address", defaultOllamaAddress+"/v1")
			},
		},
		{
			name: "the embedding block is checked separately",
			setup: func() {
				viper.Set("llm.embedding.enabled", true)
				viper.Set("llm.embedding.provider", providerOpenAI)
				viper.Set("llm.embedding.address", defaultOllamaAddress)
			},
			want: "llm.embedding.address is still the Ollama default",
		},
		{
			name: "the two providers are independent",
			setup: func() {
				viper.Set("llm.enabled", true)
				viper.Set("llm.provider", providerOpenAI)
				viper.Set("llm.address", "https://api.openai.com/v1")
				viper.Set("llm.embedding.enabled", true)
				viper.Set("llm.embedding.provider", providerOllama)
				viper.Set("llm.embedding.address", defaultOllamaAddress)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			tc.setup()

			problems := llmProblems()

			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}

				return
			}

			if len(problems) == 0 {
				t.Fatalf("expected a problem containing %q", tc.want)
			}

			joined := ""
			for _, problem := range problems {
				joined += problem.Error() + "\n"
			}

			if !strings.Contains(joined, tc.want) {
				t.Errorf("problems = %q, want one containing %q", joined, tc.want)
			}
		})
	}
}

// llmKeysWithoutLegacyName are llm.* keys that are genuinely new in v0.42.0, with the reason. A key
// here has nothing to be aliased from; every other one must appear in llmAliases.
var llmKeysWithoutLegacyName = map[string]string{
	"llm.provider":           "new in v0.42.0: there was one provider before it, so nothing named it",
	"llm.apiKey":             "new in v0.42.0: the native Ollama API sends no credential, so no key existed to carry over",
	"llm.embedding.provider": "as above, for the embedding half",
	"llm.embedding.apiKey":   "as above, for the embedding half",
}

// TestLLMAliasesCoverEveryKey is the drift guard on the rename. An llm.* key the service reads with
// neither an alias nor an entry above is one an existing deployment's configuration would stop
// reaching - silently, since a missing key reads as its zero value rather than as an error, and the
// features behind these keys are the ones that fail late enough for nobody to notice.
//
// It reuses serviceConfigKeys, which walks the whole module rather than main.go alone: llm.* keys
// are read in three packages (here, hippocampus/server.go for autoSummarise, and
// hippocampus/topology.go for the node specs), and a guard that looked only at main.go would have
// declared two of the aliases dead.
func TestLLMAliasesCoverEveryKey(t *testing.T) {
	aliased := make(map[string]bool, len(llmAliases))

	for _, alias := range llmAliases {
		aliased[alias.key] = true
	}

	read := make(map[string]bool)

	for key := range serviceConfigKeys(t) {
		if !strings.HasPrefix(key, "llm.") {
			continue
		}

		read[key] = true

		if aliased[key] {
			continue
		}

		if _, isNew := llmKeysWithoutLegacyName[key]; isNew {
			continue
		}

		t.Errorf("the service reads %q, which has no entry in llmAliases - add one, or record it in llmKeysWithoutLegacyName with the reason it is genuinely new", key)
	}

	if len(read) == 0 {
		t.Fatal("found no llm.* reads - serviceConfigKeys no longer sees them")
	}

	// The other direction: an alias for a key nothing reads any more would keep honouring a legacy
	// name whose replacement has been removed, which is worse than useless.
	for _, alias := range llmAliases {
		if !read[alias.key] {
			t.Errorf("llmAliases maps %q onto %q, which the service no longer reads - drop the entry", alias.legacy, alias.key)
		}
	}

	for key := range llmKeysWithoutLegacyName {
		if !read[key] {
			t.Errorf("llmKeysWithoutLegacyName names %q, which the service no longer reads - drop the entry", key)
		}
	}
}
