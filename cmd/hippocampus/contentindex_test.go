package main

import (
	"testing"

	"github.com/spf13/viper"
)

// TestContentIndexSetting pins the derived default and its override, on the reflectionSetting
// precedent this follows.
//
// The judgement in the derivation is that a deployment running OpenSearch reads none of the store's
// own index while paying for all of it: the write is inside the storage boundary, before compression
// ever sees the body, and it is the largest non-body cost the store carries on every dialect. The
// override has to work in BOTH directions - keeping the index alongside OpenSearch is what a
// deployment falls back to when the cluster is unreachable - which is why the key is read through
// viper.IsSet rather than GetBool.
func TestContentIndexSetting(t *testing.T) {
	cases := []struct {
		name       string
		openSearch bool
		set        bool
		value      bool
		want       bool
	}{
		{name: "on by default without opensearch", want: true},
		{name: "off by default with opensearch", openSearch: true, want: false},
		{name: "explicitly off without opensearch", set: true, value: false, want: false},
		{name: "explicitly on with opensearch", openSearch: true, set: true, value: true, want: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			viper.Set("opensearch.enabled", testCase.openSearch)

			if testCase.set {
				viper.Set("search.contentIndex.enabled", testCase.value)
			}

			enabled, reason := contentIndexSetting()

			if enabled != testCase.want {
				t.Errorf("contentIndexSetting() = %v, want %v", enabled, testCase.want)
			}

			if reason == "" {
				t.Error("contentIndexSetting returned no reason - the startup line would say nothing about which default it took")
			}
		})
	}
}

// TestContentIndexOptions: the decision has to reach initSchema, which runs inside the constructor,
// so it travels as a db.Option rather than a setter. Keeping the index is the absence of an option,
// not an option saying so, which is what makes every existing call site correct unchanged.
func TestContentIndexOptions(t *testing.T) {
	if opts := contentIndexOptions(true); len(opts) != 0 {
		t.Errorf("contentIndexOptions(true) returned %d options, want none", len(opts))
	}

	if opts := contentIndexOptions(false); len(opts) != 1 {
		t.Errorf("contentIndexOptions(false) returned %d options, want 1", len(opts))
	}
}
