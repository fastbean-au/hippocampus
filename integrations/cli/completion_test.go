package main

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
)

func contains(items []string, want string) bool {
	return slices.Contains(items, want)
}

func TestCompleteFirstTokens(t *testing.T) {
	got := completeArgs(nil)

	for _, want := range []string{"memory", "event", "summary", "whoami", "completion", "import-batch"} {
		if !contains(got, want) {
			t.Errorf("first-token completion missing %q: %v", want, got)
		}
	}

	// A two-word command's group appears once, not its verbs.
	if contains(got, "store") {
		t.Errorf("first-token completion should not include verbs: %v", got)
	}
}

func TestCompleteVerbs(t *testing.T) {
	got := completeArgs([]string{"memory"})

	for _, want := range []string{"store", "update", "delete", "list", "recall", "search"} {
		if !contains(got, want) {
			t.Errorf("memory verb completion missing %q: %v", want, got)
		}
	}
}

func TestCompleteFlagsForCommand(t *testing.T) {
	got := completeArgs([]string{"memory", "store"})

	for _, want := range []string{"--body", "--significance", "--group", "--transport", "--output"} {
		if !contains(got, want) {
			t.Errorf("flag completion missing %q: %v", want, got)
		}
	}
}

func TestCompleteFlagsForOneWordCommand(t *testing.T) {
	got := completeArgs([]string{"whoami"})

	if !contains(got, "--transport") {
		t.Errorf("one-word command should complete global flags: %v", got)
	}
}

func TestCompleteEnumFlagValue(t *testing.T) {
	if got := completeArgs([]string{"--transport"}); !contains(got, "grpc") || !contains(got, "http") {
		t.Errorf("--transport values = %v", got)
	}

	if got := completeArgs([]string{"event", "significance", "--id", "e", "--place-mode"}); !contains(got, "between") {
		t.Errorf("--place-mode values = %v", got)
	}
}

func TestCompleteGlobalBeforeCommand(t *testing.T) {
	// After "--transport http", the value is consumed and the next word completes to commands.
	got := completeArgs([]string{"--transport", "http"})

	if !contains(got, "memory") {
		t.Errorf("expected command completion after a consumed global value: %v", got)
	}

	// And the group's verbs still resolve with a global flag in front.
	if got := completeArgs([]string{"--transport", "http", "memory"}); !contains(got, "store") {
		t.Errorf("expected verbs after global flag + group: %v", got)
	}
}

func TestCompleteNonEnumValueDeclines(t *testing.T) {
	if got := completeArgs([]string{"memory", "store", "--group"}); got != nil {
		t.Errorf("a free-form value flag should yield no candidates, got %v", got)
	}
}

func TestCompleteExcludesUsedScalarFlagButKeepsRepeatable(t *testing.T) {
	got := completeArgs([]string{"memory", "store", "--body", "x"})

	if contains(got, "--body") {
		t.Errorf("a used scalar flag should not be re-offered: %v", got)
	}

	// A repeatable slice flag stays available after a value.
	if got := completeArgs([]string{"memory", "delete", "--id", "a"}); !contains(got, "--id") {
		t.Errorf("repeatable --id should remain available: %v", got)
	}
}

func TestCompleteCompletionShells(t *testing.T) {
	got := completeArgs([]string{"completion"})

	for _, want := range []string{"bash", "zsh", "fish"} {
		if !contains(got, want) {
			t.Errorf("completion shell candidates missing %q: %v", want, got)
		}
	}
}

func TestFlagInfo(t *testing.T) {
	cases := []struct {
		word   string
		name   string
		isFlag bool
		inline bool
	}{
		{"--group", "group", true, false},
		{"--group=svc", "group", true, true},
		{"-o", "output", true, false},
		{"-o=json", "output", true, true},
		{"-x", "x", true, false},
		{"memory", "", false, false},
		{"-", "", false, false},
	}

	for _, tc := range cases {
		name, isFlag, inline := flagInfo(tc.word)
		if name != tc.name || isFlag != tc.isFlag || inline != tc.inline {
			t.Errorf("flagInfo(%q) = (%q,%t,%t), want (%q,%t,%t)", tc.word, name, isFlag, inline, tc.name, tc.isFlag, tc.inline)
		}
	}
}

func TestNonFlagPositional(t *testing.T) {
	got := nonFlagPositional([]string{"--transport", "http", "memory", "list", "--group", "svc"})

	// The global value (http) is stripped; "svc" trails a command flag and is harmless.
	if len(got) < 2 || got[0] != "memory" || got[1] != "list" {
		t.Errorf("nonFlagPositional = %v, want it to start with [memory list]", got)
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script, ok := completionScript(shell)
		if !ok || !strings.Contains(script, "hippo __complete") {
			t.Errorf("%s script missing or does not call __complete: ok=%t", shell, ok)
		}
	}

	if _, ok := completionScript("powershell"); ok {
		t.Error("unknown shell should not resolve")
	}
}

func TestRunCompletionCmd(t *testing.T) {
	out, _, err := runCommand(t, "completion", []string{"zsh"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	_ = out
}

func TestRunCompletionCmdBadShell(t *testing.T) {
	if _, _, err := runCommand(t, "completion", []string{"powershell"}, &fakeClient{}); err == nil || !strings.Contains(err.Error(), "unknown shell") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunCompletionCmdNoShell(t *testing.T) {
	if _, _, err := runCommand(t, "completion", nil, &fakeClient{}); err == nil || !strings.Contains(err.Error(), "a shell is required") {
		t.Fatalf("err = %v", err)
	}
}

// TestRunCompleteViaRun exercises the hidden __complete command through run(), confirming it needs
// no service connection and prints candidates.
func TestRunCompleteViaRun(t *testing.T) {
	var out bytes.Buffer

	if err := run(context.Background(), []string{"__complete", "memory"}, &out, &out); err != nil {
		t.Fatalf("run __complete: %v", err)
	}

	if !strings.Contains(out.String(), "store") {
		t.Fatalf("__complete output = %q", out.String())
	}
}

// TestCompletionEmittedByCompletionCommand confirms `completion bash` writes a real script.
func TestCompletionEmittedByCompletionCommand(t *testing.T) {
	var out bytes.Buffer

	if err := run(context.Background(), []string{"completion", "bash"}, &out, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "complete -F _hippo hippo") {
		t.Fatalf("bash script = %q", out.String())
	}
}

// TestCompleteOrderByIsCommandSpecific pins the one flag whose candidates depend on the command
// carrying it: the two listings sort on different columns, so completing --order-by against the
// shared table would offer values the service rejects.
func TestCompleteOrderByIsCommandSpecific(t *testing.T) {
	memories := completeArgs([]string{"memory", "list", "--order-by"})

	if !contains(memories, "recall_count") || !contains(memories, "time_recalled") {
		t.Errorf("memory --order-by values = %v", memories)
	}

	if contains(memories, "name") || contains(memories, "time_end") {
		t.Errorf("memory --order-by offered an event-only column: %v", memories)
	}

	events := completeArgs([]string{"event", "list", "--order-by"})

	if !contains(events, "name") || !contains(events, "time_end") {
		t.Errorf("event --order-by values = %v", events)
	}

	if contains(events, "recall_count") || contains(events, "time_recalled") {
		t.Errorf("event --order-by offered a memory-only column: %v", events)
	}

	// --order-dir is the same closed set on both, so it stays in the shared table.
	if got := completeArgs([]string{"memory", "list", "--order-dir"}); !contains(got, "asc") || !contains(got, "desc") {
		t.Errorf("--order-dir values = %v", got)
	}
}
