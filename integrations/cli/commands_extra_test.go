package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestCallbackKindFromFlag covers the operator-facing spelling, including the short forms and the
// rejection - a mistyped kind must not silently widen the query to every kind.
func TestCallbackKindFromFlag(t *testing.T) {
	cases := map[string]contract.CallbackKind{
		"":                  contract.CallbackKind_CALLBACK_KIND_UNSPECIFIED,
		"memory-forgotten":  contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN,
		"memory":            contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN,
		"Event-Forgotten":   contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN,
		"  sleep-completed": contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
		"sleep":             contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
	}

	for input, want := range cases {
		fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
		fs.String("kind", input, "")

		got, err := callbackKindFromFlag(fs, "kind")
		if err != nil {
			t.Fatalf("callbackKindFromFlag(%q): %s", input, err)
		}

		if got != want {
			t.Errorf("callbackKindFromFlag(%q) = %v, want %v", input, got, want)
		}
	}

	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("kind", "nonsense", "")

	if _, err := callbackKindFromFlag(fs, "kind"); err == nil {
		t.Error("an unknown kind was accepted, and would have read as 'every kind'")
	}
}

// TestCallbackClearRequiresABound pins the refusal: discarding every pending notification must not
// be something a bare command does.
func TestCallbackClearRequiresABound(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	fs.String("before", "", "")
	fs.Bool("all", false, "")

	err := runCallbackClear(context.Background(), &fakeClient{}, fs, &renderer{})
	if err == nil {
		t.Fatal("a bare 'callbacks clear' was accepted")
	}

	if !strings.Contains(err.Error(), "--before or --all") {
		t.Errorf("the error does not say what is needed: %s", err.Error())
	}
}
