package main

import (
	"testing"

	"github.com/spf13/viper"
)

// TestBackfillEmbedderFromViper_DisabledIsNoop covers the default: a backfill with no embedder
// configured re-indexes text only, and must not fail for want of a model server.
func TestBackfillEmbedderFromViper_DisabledIsNoop(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	embedder := backfillEmbedderFromViper()

	if embedder == nil {
		t.Fatal("expected a no-op embedder rather than nil")
	}

	if embedder.Enabled() {
		t.Error("expected the embedder to report itself disabled")
	}
}

// TestBackfillEmbedderFromViper_Enabled covers the configured arm. The backfill re-embeds rather
// than re-reading vectors, because vectors live only in the index and never in the primary store -
// so this is what makes --backfill-search --reindex able to rebuild a semantic index at all.
func TestBackfillEmbedderFromViper_Enabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	viper.Set("llm.embedding.enabled", true)
	viper.Set("llm.embedding.address", "http://127.0.0.1:11434")
	viper.Set("llm.embedding.model", "nomic-embed-text")
	viper.Set("llm.embedding.dimensions", 768)

	embedder := backfillEmbedderFromViper()

	if !embedder.Enabled() {
		t.Fatal("expected a configured embedder to report itself enabled")
	}

	if got := embedder.Model(); got != "nomic-embed-text" {
		t.Errorf("expected the configured model, got %q", got)
	}
}
