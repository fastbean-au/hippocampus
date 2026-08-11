package rules

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestDeclaredEnvironment pins the variable set a rules file may reference. It is not a tautology:
// the environment is a contract with every deployed rules file, and dropping or renaming a field
// turns one of those into a compile error at the next reload - which keeps the last good ruleset
// serving (good) but silently stops the operator's intended change from landing (not good). A
// deliberate change here should be a deliberate change to this list.
func TestDeclaredEnvironment(t *testing.T) {
	eventFields := celTags(t, Event{})
	memoryFields := celTags(t, Memory{})

	wantEvent := []string{
		"body_bytes", "description", "duration_seconds", "group", "id", "memory_count",
		"metadata", "name", "significance", "significance_max", "significance_mean",
		"significance_min", "time_end", "time_start",
	}

	wantMemory := []string{
		"body", "id", "is_binary", "is_summary", "metadata", "recall_count", "significance",
		"time_stamp",
	}

	if !reflect.DeepEqual(eventFields, wantEvent) {
		t.Errorf("event fields changed:\n got %v\nwant %v", eventFields, wantEvent)
	}

	if !reflect.DeepEqual(memoryFields, wantMemory) {
		t.Errorf("memory fields changed:\n got %v\nwant %v", memoryFields, wantMemory)
	}
}

func celTags(t *testing.T, v any) []string {
	t.Helper()

	typ := reflect.TypeOf(v)
	out := make([]string, 0, typ.NumField())

	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("cel")
		if tag == "" {
			t.Errorf("%s.%s has no cel tag; the CEL name would fall back to the Go name", typ.Name(), typ.Field(i).Name)

			continue
		}

		out = append(out, tag)
	}

	sort.Strings(out)

	return out
}

// TestParseRejects covers everything a rules file can get wrong that is worth a specific message.
// All of these are load-time failures, which is the whole point of compiling at load: an operator
// finds out when they write the file, not when the first event that reaches the rule arrives.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"missing default action",
			`{"rules":[]}`,
			"defaultAction",
		},
		{
			"unknown default action",
			`{"defaultAction":"maybe","rules":[]}`,
			"unknown action",
		},
		{
			"unknown field",
			`{"defaultAction":"drop","rulez":[]}`,
			"unknown field",
		},
		{
			"rule without a name",
			`{"defaultAction":"drop","rules":[{"expr":"true","action":"promote"}]}`,
			"name is required",
		},
		{
			"rule without an expression",
			`{"defaultAction":"drop","rules":[{"name":"r","action":"promote"}]}`,
			"expr is required",
		},
		{
			"duplicate rule names",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"drop"},{"name":"r","expr":"true","action":"drop"}]}`,
			"duplicate rule name",
		},
		{
			"expression that does not compile",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"event.nope == 1","action":"promote"}]}`,
			"compiling expression",
		},
		{
			"expression naming an undeclared variable",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"whatever","action":"promote"}]}`,
			"compiling expression",
		},
		{
			"expression that is not a bool",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"event.name","action":"promote"}]}`,
			"must evaluate to bool",
		},
		{
			"reduce on a drop",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"drop","reduce":{"keepTopN":1}}]}`,
			"only meaningful with action",
		},
		{
			"summarise combined with a selection",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","reduce":{"summarise":true,"keepTopN":2}}]}`,
			"cannot be combined",
		},
		{
			"reduce that selects nothing",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","reduce":{}}]}`,
			"selects nothing",
		},
		{
			"negative keepTopN",
			`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","reduce":{"keepTopN":-1}}]}`,
			"must not be negative",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc), Options{})
			if err == nil {
				t.Fatalf("expected a parse failure")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected an error mentioning %q, got %q", c.want, err.Error())
			}
		})
	}
}

// TestEvaluateFirstMatchWins covers the ordinary path: rules are tried in file order, the first
// match decides, and an event matching none takes the default action.
func TestEvaluateFirstMatchWins(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [
			{"name":"errors","expr":"'severity' in event.metadata && event.metadata['severity'] == 'error'","action":"promote"},
			{"name":"big","expr":"event.memory_count >= 50","action":"promote","reduce":{"keepTopN":10}},
			{"name":"everything-else-named-x","expr":"event.name == 'x'","action":"drop"}
		]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	if set.Rules() != 3 {
		t.Errorf("expected 3 rules, got %d", set.Rules())
	}

	if set.DefaultAction() != ActionDrop {
		t.Errorf("expected the default action to be drop, got %q", set.DefaultAction())
	}

	cases := []struct {
		name  string
		facts Facts
		want  Decision
	}{
		{
			"first rule matches",
			Facts{Event: Event{Metadata: map[string]string{"severity": "error"}, MemoryCount: 100}},
			Decision{Rule: "errors", Action: ActionPromote},
		},
		{
			"a later rule matches and carries its reduction",
			Facts{Event: Event{MemoryCount: 60}},
			Decision{Rule: "big", Action: ActionPromote, Reduce: Reduce{KeepTopN: 10}},
		},
		{
			"no match takes the default",
			Facts{Event: Event{Name: "y", MemoryCount: 1}},
			Decision{Action: ActionDrop},
		},
		{
			"a matching rule may itself be a drop",
			Facts{Event: Event{Name: "x", MemoryCount: 1}},
			Decision{Rule: "everything-else-named-x", Action: ActionDrop},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := set.Evaluate(context.Background(), c.facts)
			if err != nil {
				t.Fatalf("Evaluate: %s", err)
			}

			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %+v, got %+v", c.want, got)
			}
		})
	}
}

// TestEvaluateErrorDoesNotSilentlyMatchOrBlock is the behaviour an admission gate must have: a rule
// that errors at evaluation (here, indexing a metadata key the event does not carry) does not match,
// is reported so it can be logged, and does not stop the rules after it from being tried.
func TestEvaluateErrorDoesNotSilentlyMatchOrBlock(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [
			{"name":"unguarded","expr":"event.metadata['severity'] == 'error'","action":"drop"},
			{"name":"fallback","expr":"event.name == 'keep me'","action":"promote"}
		]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	got, err := set.Evaluate(context.Background(), Facts{Event: Event{Name: "keep me"}})

	if err == nil {
		t.Fatal("expected the broken rule's evaluation error to be reported")
	}

	if !strings.Contains(err.Error(), "unguarded") {
		t.Errorf("expected the error to name the rule, got %q", err.Error())
	}

	if got.Rule != "fallback" || got.Action != ActionPromote {
		t.Errorf("a broken rule must not stop the ones after it; got %+v", got)
	}
}

// TestEvaluateOptionalMetadataAccess pins the guarded forms an operator is pointed at when they hit
// the missing-key error above - both of them, since the optional-types one only works because the
// environment enables it.
func TestEvaluateOptionalMetadataAccess(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [
			{"name":"optional","expr":"event.metadata[?'severity'].orValue('') == 'error'","action":"promote"},
			{"name":"in-guard","expr":"'tier' in event.metadata && event.metadata['tier'] == 'gold'","action":"promote"}
		]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	// Neither form errors on an event carrying no metadata at all.
	got, err := set.Evaluate(context.Background(), Facts{Event: Event{Name: "bare"}})
	if err != nil {
		t.Fatalf("the guarded forms must not error on missing keys: %s", err)
	}

	if got.Action != ActionDrop {
		t.Errorf("expected the default action, got %+v", got)
	}

	got, err = set.Evaluate(context.Background(), Facts{Event: Event{Metadata: map[string]string{"severity": "error"}}})
	if err != nil {
		t.Fatalf("Evaluate: %s", err)
	}

	if got.Rule != "optional" {
		t.Errorf("expected the optional-index rule to match, got %+v", got)
	}
}

// TestNeedsMemories covers the flag that lets the promoter skip materialising bodies entirely: it is
// true only when some rule actually reads the memories list.
func TestNeedsMemories(t *testing.T) {
	cases := []struct {
		name string
		expr string
		want bool
	}{
		{"event-only rule", "event.memory_count > 3", false},
		{"aggregates are event fields, not memory reads", "event.significance_max > 5 && event.body_bytes < 1000", false},
		{"a comprehension over the memories", "memories.exists(m, m.body.contains('checkout'))", true},
		{"a plain reference to the list", "size(memories) > 2", true},
		{"the word appearing in a string literal is not a reference", "event.name == 'memories'", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := `{"defaultAction":"drop","rules":[{"name":"r","expr":` + quote(c.expr) + `,"action":"promote"}]}`

			set, err := Parse([]byte(doc), Options{})
			if err != nil {
				t.Fatalf("Parse: %s", err)
			}

			if set.NeedsMemories() != c.want {
				t.Errorf("expected NeedsMemories %v for %q", c.want, c.expr)
			}
		})
	}
}

// TestEvaluateMemoryFacts drives a content rule over the memory list, which is the half of the
// environment that gives the promoter visibility into memory bodies.
func TestEvaluateMemoryFacts(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [{
			"name": "mentions-checkout",
			"expr": "memories.exists(m, !m.is_binary && m.body.lowerAscii().contains('checkout'))",
			"action": "promote",
			"reduce": {"summarise": true}
		}]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	got, err := set.Evaluate(context.Background(), Facts{
		Event: Event{Id: "e1", MemoryCount: 2},
		Memories: []Memory{
			{Id: "m1", Body: "nothing to see"},
			{Id: "m2", Body: "CHECKOUT failed twice"},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %s", err)
	}

	if got.Rule != "mentions-checkout" || !got.Reduce.Summarise {
		t.Errorf("expected the content rule to match with a summarise reduction, got %+v", got)
	}

	// A rule referencing memories must not error when the promoter passes none.
	if _, err := set.Evaluate(context.Background(), Facts{Event: Event{Id: "e2"}}); err != nil {
		t.Errorf("an unbound memories list would fail here: %s", err)
	}
}

// TestEvaluateCostLimit pins that a pathological expression is stopped rather than allowed to run
// against every completed event. The limit is deliberately tiny here; the shipped default is large
// enough that ordinary rules never approach it.
func TestEvaluateCostLimit(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "promote",
		"rules": [{"name":"expensive","expr":"memories.exists(m, m.body.contains('x'))","action":"drop"}]
	}`), Options{CostLimit: 1})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	memories := make([]Memory, 200)
	for i := range memories {
		memories[i] = Memory{Id: "m", Body: strings.Repeat("y", 64)}
	}

	got, err := set.Evaluate(context.Background(), Facts{Event: Event{Id: "e1"}, Memories: memories})

	if err == nil {
		t.Fatal("expected the cost limit to stop the evaluation")
	}

	// The default still applies, so an event is never left un-judged by a rule that blew its budget.
	if got.Action != ActionPromote {
		t.Errorf("expected the default action after a cost trip, got %+v", got)
	}
}

// TestEvaluateHonoursContextCancellation pins that the evaluation timeout is real - the program is
// built with an interrupt check frequency precisely so ContextEval can be cut short.
func TestEvaluateHonoursContextCancellation(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [{"name":"r","expr":"memories.exists(m, m.body.contains('x'))","action":"promote"}]
	}`), Options{EvalTimeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	memories := make([]Memory, 500)
	for i := range memories {
		memories[i] = Memory{Id: "m", Body: strings.Repeat("y", 128)}
	}

	if _, err := set.Evaluate(context.Background(), Facts{Memories: memories}); err == nil {
		t.Fatal("expected the evaluation timeout to fire")
	}
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// TestEvalErrorNamesItsRule pins the typed error the promoter needs to attribute a rule failure to
// the rule that caused it. Without the name, "some rule is erroring" is all a metric could say, and
// a rule that errors on every event never matches - so it silently changes what is promoted.
func TestEvalErrorNamesItsRule(t *testing.T) {
	set, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [{"name":"unguarded","expr":"event.metadata['team'] == 'x'","action":"promote"}]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	_, err = set.Evaluate(context.Background(), Facts{Event: Event{Id: "e1"}})
	if err == nil {
		t.Fatal("expected an evaluation error")
	}

	var evalErr *EvalError

	if !errors.As(err, &evalErr) {
		t.Fatalf("expected an *EvalError, got %T", err)
	}

	if evalErr.Rule != "unguarded" {
		t.Errorf("expected the rule name, got %q", evalErr.Rule)
	}

	if evalErr.Unwrap() == nil {
		t.Error("expected the underlying CEL error to be reachable")
	}

	if !strings.Contains(evalErr.Error(), "unguarded") {
		t.Errorf("expected the message to name the rule, got %q", evalErr.Error())
	}
}
