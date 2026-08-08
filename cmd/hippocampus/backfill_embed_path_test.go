package main

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/search"
)

// countingEmbedder records what it was asked to embed and returns a fixed vector, so the backfill's
// re-embedding can be driven without a model server.
type countingEmbedder struct {
	calls int
}

func (e *countingEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	e.calls += len(texts)

	vectors := make([]embed.Vector, 0, len(texts))
	for range texts {
		vectors = append(vectors, embed.Vector{0.1, 0.2, 0.3})
	}

	return vectors, nil
}

func (e *countingEmbedder) Model() string { return "counting" }

func (e *countingEmbedder) Enabled() bool { return true }

// TestBackfillSearch_ReEmbedsEachMemory pins why the backfill needs a model server at all: vectors
// live only in the index and never in the primary store, so a rebuild has to re-embed rather than
// re-read. Every non-binary memory with a body must therefore be embedded exactly once.
func TestBackfillSearch_ReEmbedsEachMemory(t *testing.T) {
	const memoryCount = 3

	dir := seedSQLiteFixture(t, memoryCount)

	fake := &fakeOpenSearchServer{}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	embedder := &countingEmbedder{}

	backfillSearch(backfillConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Search: search.Config{
			Addresses:       []string{server.URL},
			Index:           "test-index",
			QueueSize:       16,
			VectorDimension: 3,
		},
		BatchSize: 10,
		Embed:     embedder,
	})

	if embedder.calls != memoryCount {
		t.Errorf("expected each of the %d memories to be embedded once, got %d calls", memoryCount, embedder.calls)
	}

	if got := fake.count("/_doc/"); got != memoryCount {
		t.Errorf("expected %d documents indexed, got %d: %v", memoryCount, got, fake.recorded())
	}
}

// TestBackfillSearch_DisabledEmbedderSkipsEmbedding is the other half: a backfill with no embedder
// configured re-indexes text only, and must not require a model server to run at all.
func TestBackfillSearch_DisabledEmbedderSkipsEmbedding(t *testing.T) {
	const memoryCount = 2

	dir := seedSQLiteFixture(t, memoryCount)

	fake := &fakeOpenSearchServer{}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	backfillSearch(backfillConfig{
		StorageDriver:    "sqlite",
		StorageDirectory: dir,
		Search: search.Config{
			Addresses: []string{server.URL},
			Index:     "test-index",
			QueueSize: 16,
		},
		BatchSize: 10,
		Embed:     embed.NewNoop(),
	})

	if got := fake.count("/_doc/"); got != memoryCount {
		t.Errorf("expected %d documents indexed, got %d: %v", memoryCount, got, fake.recorded())
	}
}
