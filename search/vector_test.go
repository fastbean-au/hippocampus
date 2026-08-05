package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The SQL backend has no vector index, so a vector query must be refused rather than quietly
// answered as a keyword search - a caller who asked for meaning and silently got word matching
// would conclude semantic search works badly rather than that it is absent.
func TestSQLRefusesVectorQueries(t *testing.T) {
	idx, err := NewSQL(&fakeContentStore{available: true})
	if err != nil {
		t.Fatalf("NewSQL: %s", err)
	}

	if idx.SupportsVectors() {
		t.Error("the SQL backend claims vector support")
	}

	_, err = idx.Search(context.Background(), Query{Text: "anything", Vector: []float32{0.1, 0.2}})
	if !errors.Is(err, ErrSemanticUnavailable) {
		t.Errorf("got %v, want ErrSemanticUnavailable", err)
	}
}

func TestNoopReportsNoVectorSupport(t *testing.T) {
	if NewNoop().SupportsVectors() {
		t.Error("the no-op index claims vector support")
	}
}

// The mapping chosen at index creation decides whether semantic search is possible at all, since
// index.knn cannot be set afterwards.
func TestOpenSearchMappingIncludesVectorsOnlyWhenConfigured(t *testing.T) {
	plain := &OpenSearch{}

	if strings.Contains(plain.mapping(), "knn_vector") {
		t.Error("a deployment without an embedder got a vector mapping")
	}

	withVectors := &OpenSearch{vectorDimension: 768}
	mapping := withVectors.mapping()

	if !strings.Contains(mapping, "knn_vector") {
		t.Fatal("the vector mapping has no knn_vector field")
	}

	if !strings.Contains(mapping, `"dimension": 768`) {
		t.Error("the vector mapping does not carry the configured dimension")
	}

	if !strings.Contains(mapping, `"index.knn": true`) {
		t.Error("the vector mapping does not enable index.knn")
	}

	// Both mappings must remain valid JSON, since a malformed one fails index creation at runtime
	// rather than at compile time.
	for name, body := range map[string]string{"plain": plain.mapping(), "vector": mapping} {
		var parsed map[string]any

		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Errorf("the %s mapping is not valid JSON: %s", name, err)
		}
	}
}

// Configuration alone does not make k-NN answerable: an index created before semantic search was
// enabled has no vector field and cannot gain one in place.
func TestOpenSearchRefusesVectorQueryUntilTheIndexHasTheField(t *testing.T) {
	idx := &OpenSearch{vectorDimension: 768}

	if idx.SupportsVectors() {
		t.Error("SupportsVectors is true before the index was confirmed to carry the field")
	}

	_, err := idx.Search(context.Background(), Query{Text: "anything", Vector: []float32{0.1}})
	if !errors.Is(err, ErrSemanticUnavailable) {
		t.Errorf("got %v, want ErrSemanticUnavailable", err)
	}

	idx.vectorReady.Store(true)

	if !idx.SupportsVectors() {
		t.Error("SupportsVectors is false after the index was confirmed to carry the field")
	}
}

func TestMappingHasVectorField(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "present",
			raw:  `{"properties":{"body":{"type":"text"},"vector":{"type":"knn_vector","dimension":768}}}`,
			want: true,
		},
		{
			name: "absent",
			raw:  `{"properties":{"body":{"type":"text"}}}`,
			want: false,
		},
		{
			// A field named vector that is not a knn_vector cannot answer a k-NN query.
			name: "wrong type",
			raw:  `{"properties":{"vector":{"type":"text"}}}`,
			want: false,
		},
		{name: "no properties", raw: `{}`, want: false},

		// Unparseable answers false: refusing semantic search costs a log line and a --reindex,
		// where wrongly allowing it fails every query at the cluster.
		{name: "malformed", raw: `not json`, want: false},
		{name: "empty", raw: ``, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mappingHasVectorField(json.RawMessage(test.raw)); got != test.want {
				t.Errorf("mappingHasVectorField(%s) = %v, want %v", test.raw, got, test.want)
			}
		})
	}
}

// The two query shapes must stay distinct: a bool query summing a bm25 score and a cosine
// similarity would be adding numbers on unrelated scales, which is why hybrid fuses above this
// package instead.
func TestOpenSearchSearchQueryShape(t *testing.T) {
	idx := &OpenSearch{vectorDimension: 3}

	filters := []any{map[string]any{"term": map[string]any{"group": "ops"}}}

	keyword := idx.searchQuery(Query{Text: "deployment", Limit: 5}, filters)

	if _, ok := keyword["bool"]; !ok {
		t.Errorf("a keyword query built %v, want a bool query", keyword)
	}

	semantic := idx.searchQuery(Query{Text: "deployment", Vector: []float32{1, 2, 3}, Limit: 5}, filters)

	knn, ok := semantic["knn"].(map[string]any)
	if !ok {
		t.Fatalf("a vector query built %v, want a knn query", semantic)
	}

	field, ok := knn["vector"].(map[string]any)
	if !ok {
		t.Fatalf("the knn query has no vector field: %v", knn)
	}

	if field["k"] != 5 {
		t.Errorf("k is %v, want the query's limit of 5", field["k"])
	}

	// The filter must be inside the knn clause so it narrows the traversal rather than discarding
	// results after it - otherwise a group-scoped semantic search returns fewer hits than asked for.
	if _, ok := field["filter"]; !ok {
		t.Error("the knn query carries no filter; a scoped search would under-return")
	}

	// And a vector query with no filters must not carry an empty one.
	unfiltered := idx.searchQuery(Query{Vector: []float32{1, 2, 3}, Limit: 5}, nil)
	unfilteredField := unfiltered["knn"].(map[string]any)["vector"].(map[string]any)

	if _, ok := unfilteredField["filter"]; ok {
		t.Error("an unfiltered knn query carries an empty filter")
	}
}
