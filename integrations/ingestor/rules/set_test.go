package rules

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// setDoc wraps one mutation block in a minimal rules file.
func setDoc(set string) string {
	return `{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","set":` + set + `}]}`
}

// mustParseSet loads a one-rule file and returns the rule's compiled mutation.
func mustParseSet(t *testing.T, set string) *Mutation {
	t.Helper()

	ruleset, err := Parse([]byte(setDoc(set)), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	decision, err := ruleset.Evaluate(context.Background(), Facts{})
	if err != nil {
		t.Fatalf("Evaluate: %s", err)
	}

	if decision.Mutation == nil {
		t.Fatal("the decision carries no mutation")
	}

	return decision.Mutation
}

// TestSetRejects covers what a mutation block can get wrong at LOAD time. Every one of these is
// found when the file is written (or by --check-rules) rather than on the first event that reaches
// the rule, which is the whole reason the expressions are compiled and type-checked at load.
func TestSetRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"set on a dropping rule",
			`{"defaultAction":"promote","rules":[{"name":"r","expr":"true","action":"drop","set":{"event":{"significance":"1"}}}]}`,
			"only meaningful with action",
		},
		{
			"set naming neither scope",
			setDoc(`{}`),
			"names neither event nor memory",
		},
		{
			"an event block setting nothing",
			setDoc(`{"event":{}}`),
			"set.event is present but sets no field",
		},
		{
			"a memory block setting nothing",
			setDoc(`{"memory":{}}`),
			"set.memory is present but sets no field",
		},
		{
			// The separate EventSet/MemorySet types exist for exactly this: a shared struct would
			// have accepted `name` here and silently done nothing with it.
			"a field the memory scope does not have",
			setDoc(`{"memory":{"name":"'x'"}}`),
			"unknown field",
		},
		{
			"significance that is not an int",
			setDoc(`{"event":{"significance":"'high'"}}`),
			"must evaluate to int",
		},
		{
			// A double is the one worth a specific case: scaling a mean is the natural thing to
			// write, and truncating it silently would decide a rank the file never stated. The
			// operator is told to say int(...) instead.
			"significance that is a double",
			setDoc(`{"event":{"significance":"event.significance_mean * 2.0"}}`),
			"must evaluate to int",
		},
		{
			"a group that is not a string",
			setDoc(`{"event":{"group":"1"}}`),
			"must evaluate to string",
		},
		{
			"metadata that is not a map",
			setDoc(`{"event":{"metadata":"'x'"}}`),
			"must evaluate to map(string, string)",
		},
		{
			"metadata whose values are not strings",
			setDoc(`{"event":{"metadata":"{'n': 1}"}}`),
			"must evaluate to map(string, string)",
		},
		{
			"an expression that does not compile",
			setDoc(`{"event":{"significance":"event.no_such_field"}}`),
			"compiling expression",
		},
		{
			// `memory` is declared only in the memory scope, so reaching for it from the event scope
			// is a compile error rather than a nil at evaluation.
			"the singular memory is not in scope for an event expression",
			setDoc(`{"event":{"significance":"memory.significance"}}`),
			"set.event.significance",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc), Options{})
			if err == nil {
				t.Fatal("expected the file to be rejected")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected the error to mention %q, got: %s", c.want, err.Error())
			}
		})
	}
}

// TestEventOverrides evaluates a full event block against the facts the rule matched on.
func TestEventOverrides(t *testing.T) {
	mutation := mustParseSet(t, `{"event":{
		"significance": "math.least(100, event.significance * 4)",
		"group": "'central/' + event.group",
		"name": "'[edge] ' + event.name",
		"description": "'promoted from the edge'",
		"metadata": "{'promoted': 'true', 'memories': string(event.memory_count)}"
	}}`)

	facts := Facts{Event: Event{
		Name:         "checkout failed",
		Group:        "payments",
		Significance: 40,
		MemoryCount:  3,
		Metadata:     map[string]string{"severity": "error"},
	}}

	overrides, err := mutation.EventOverrides(context.Background(), facts)
	if err != nil {
		t.Fatalf("EventOverrides: %s", err)
	}

	// math.least is what makes a computed significance safe to write: 40 * 4 clamps to 100.
	if overrides.Significance == nil || *overrides.Significance != 100 {
		t.Errorf("expected significance 100, got %v", overrides.Significance)
	}

	if overrides.Group == nil || *overrides.Group != "central/payments" {
		t.Errorf("expected group 'central/payments', got %v", overrides.Group)
	}

	if overrides.Name == nil || *overrides.Name != "[edge] checkout failed" {
		t.Errorf("expected the prefixed name, got %v", overrides.Name)
	}

	if overrides.Description == nil || *overrides.Description != "promoted from the edge" {
		t.Errorf("expected the set description, got %v", overrides.Description)
	}

	// The stamped entries are MERGED over what the event already carried, not substituted for it.
	want := map[string]string{"severity": "error", "promoted": "true", "memories": "3"}

	if len(overrides.Metadata) != len(want) {
		t.Fatalf("expected metadata %v, got %v", want, overrides.Metadata)
	}

	for k, v := range want {
		if overrides.Metadata[k] != v {
			t.Errorf("expected metadata[%q] = %q, got %q", k, v, overrides.Metadata[k])
		}
	}
}

// TestMemoryOverridesSeeTheirSiblings pins that a per-memory expression is evaluated with the event
// and the whole memory list in scope, not just the one memory. Ranking a memory against its own
// event is most of what the per-memory scope is for.
func TestMemoryOverridesSeeTheirSiblings(t *testing.T) {
	mutation := mustParseSet(t, `{"memory":{
		"significance": "memory.significance >= int(event.significance_mean) ? 90 : 10",
		"group": "event.group",
		"metadata": "{'siblings': string(size(memories))}"
	}}`)

	facts := Facts{
		Event: Event{Group: "payments", SignificanceMean: 5},
		Memories: []Memory{
			{Id: "m1", Significance: 2},
			{Id: "m2", Significance: 8},
		},
	}

	cases := []struct {
		memory Memory
		want   int32
	}{
		{Memory{Id: "m1", Significance: 2}, 10},
		{Memory{Id: "m2", Significance: 8}, 90},
	}

	for _, c := range cases {
		t.Run(c.memory.Id, func(t *testing.T) {
			overrides, err := mutation.MemoryOverrides(context.Background(), facts, c.memory)
			if err != nil {
				t.Fatalf("MemoryOverrides: %s", err)
			}

			if overrides.Significance == nil || *overrides.Significance != c.want {
				t.Errorf("expected significance %d, got %v", c.want, overrides.Significance)
			}

			if overrides.Group == nil || *overrides.Group != "payments" {
				t.Errorf("expected the event's group, got %v", overrides.Group)
			}

			if overrides.Metadata["siblings"] != "2" {
				t.Errorf("expected the memory list to be in scope, got %v", overrides.Metadata)
			}
		})
	}
}

// TestSetRejectsValuesTheTargetWouldRefuse is the load-time type check's runtime counterpart: an
// expression can compile and still produce something the target instance would reject. Catching it
// here matters because the alternative is an ImportBatch that fails for the whole event, every
// pass, with the reason buried on the far side.
func TestSetRejectsValuesTheTargetWouldRefuse(t *testing.T) {
	long := strings.Repeat("x", 2000)

	// Nine 500-byte values: each is within the per-value cap, but together they are past the
	// 4096-byte total. The total is the bound that actually protects the store's byte accounting,
	// and it is the one a per-field check would miss.
	entries := make([]string, 0, 9)

	for i := range 9 {
		entries = append(entries, "'k"+string(rune('0'+i))+"': '"+strings.Repeat("y", 500)+"'")
	}

	bulky := "{" + strings.Join(entries, ", ") + "}"

	cases := []struct {
		name string
		set  string
		want string
	}{
		{
			"a negative significance",
			`{"event":{"significance":"-1"}}`,
			"out of range",
		},
		{
			"a significance beyond int32",
			`{"event":{"significance":"2147483648"}}`,
			"out of range",
		},
		{
			"a group past the column's limit",
			`{"event":{"group":"'` + long + `'"}}`,
			"exceeds the 128-byte limit",
		},
		{
			// An event with no name cannot be imported at all, so this would fail the whole batch.
			"an empty event name",
			`{"event":{"name":"''"}}`,
			"must not be empty",
		},
		{
			"a description past its limit",
			`{"event":{"description":"'` + long + `'"}}`,
			"exceeds the 1024-byte limit",
		},
		{
			// Validated with the service's own validator, so the charset cannot drift from it.
			"a metadata key outside the permitted charset",
			`{"memory":{"metadata":"{'not a key': 'v'}"}}`,
			"metadata key",
		},
		{
			"metadata beyond the total byte budget",
			`{"memory":{"metadata":"` + bulky + `"}}`,
			"metadata too large",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutation := mustParseSet(t, c.set)

			var err error

			if strings.Contains(c.set, `"memory"`) {
				_, err = mutation.MemoryOverrides(context.Background(), Facts{}, Memory{Id: "m1"})
			} else {
				_, err = mutation.EventOverrides(context.Background(), Facts{})
			}

			if err == nil {
				t.Fatal("expected the value to be refused")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected the error to mention %q, got: %s", c.want, err.Error())
			}
		})
	}
}

// TestSetErrorNamesItsRuleAndField is what makes a mutation failure diagnosable: the rule so the
// per-rule error counter can attribute it, and the dotted field path so an operator with five
// expressions in one block knows which one to fix.
func TestSetErrorNamesItsRuleAndField(t *testing.T) {
	ruleset, err := Parse([]byte(`{
		"defaultAction": "drop",
		"rules": [{
			"name": "scorer",
			"expr": "true",
			"action": "promote",
			"set": {"memory": {"significance": "int(memory.metadata['weight'])"}}
		}]
	}`), Options{})
	if err != nil {
		t.Fatalf("Parse: %s", err)
	}

	decision, err := ruleset.Evaluate(context.Background(), Facts{})
	if err != nil {
		t.Fatalf("Evaluate: %s", err)
	}

	// The unguarded metadata index - the mistake every rules file makes first - now in a mutation.
	_, err = decision.Mutation.MemoryOverrides(context.Background(), Facts{}, Memory{Id: "m1"})
	if err == nil {
		t.Fatal("expected the missing key to fail the evaluation")
	}

	var evalErr *EvalError

	if !errors.As(err, &evalErr) {
		t.Fatalf("expected an *EvalError, got %T", err)
	}

	if evalErr.Rule != "scorer" {
		t.Errorf("expected the error to name rule 'scorer', got %q", evalErr.Rule)
	}

	if !strings.Contains(err.Error(), "set.memory.significance") {
		t.Errorf("expected the error to name the field, got: %s", err.Error())
	}
}

// TestSetNeedsMemories pins the read-avoidance decision. A memory-scoped block forces the memories
// to be read WHATEVER its expressions reference, because the promoter must hold them in order to
// rewrite them.
func TestSetNeedsMemories(t *testing.T) {
	cases := []struct {
		name string
		set  string
		want bool
	}{
		{"an event block reading only event fields", `{"event":{"significance":"event.significance * 2"}}`, false},
		{"an event block ranging over the memories", `{"event":{"significance":"size(memories)"}}`, true},
		{"a memory block, however constant", `{"memory":{"significance":"1"}}`, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ruleset, err := Parse([]byte(setDoc(c.set)), Options{})
			if err != nil {
				t.Fatalf("Parse: %s", err)
			}

			if ruleset.NeedsMemories() != c.want {
				t.Errorf("expected NeedsMemories %v", c.want)
			}

			if ruleset.Mutating() != 1 {
				t.Errorf("expected one mutating rule, got %d", ruleset.Mutating())
			}
		})
	}
}

// TestMutationScopesAreIndependent pins that a rule declaring one scope leaves the other inert,
// so the promoter can skip the work rather than evaluating an empty block per memory.
func TestMutationScopesAreIndependent(t *testing.T) {
	eventOnly := mustParseSet(t, `{"event":{"significance":"1"}}`)

	if !eventOnly.HasEvent() || eventOnly.HasMemory() {
		t.Errorf("expected an event-only mutation, got event=%v memory=%v", eventOnly.HasEvent(), eventOnly.HasMemory())
	}

	overrides, err := eventOnly.MemoryOverrides(context.Background(), Facts{}, Memory{})
	if err != nil {
		t.Fatalf("MemoryOverrides: %s", err)
	}

	if !overrides.Empty() {
		t.Errorf("expected an undeclared scope to produce nothing, got %+v", overrides)
	}

	memoryOnly := mustParseSet(t, `{"memory":{"significance":"1"}}`)

	if memoryOnly.HasEvent() || !memoryOnly.HasMemory() {
		t.Errorf("expected a memory-only mutation, got event=%v memory=%v", memoryOnly.HasEvent(), memoryOnly.HasMemory())
	}

	eventOverrides, err := memoryOnly.EventOverrides(context.Background(), Facts{})
	if err != nil {
		t.Fatalf("EventOverrides: %s", err)
	}

	if !eventOverrides.Empty() {
		t.Errorf("expected an undeclared scope to produce nothing, got %+v", eventOverrides)
	}

	// A rule with no set block at all carries no mutation, and the nil is safe to interrogate.
	var absent *Mutation

	if absent.HasEvent() || absent.HasMemory() {
		t.Error("expected a nil mutation to report neither scope")
	}
}

// TestSetLeavesUnnamedFieldsAlone is the reason Overrides uses pointers: significance 0 means
// "unranked" and an empty group means "no group", so a zero value cannot double as "not set".
func TestSetLeavesUnnamedFieldsAlone(t *testing.T) {
	mutation := mustParseSet(t, `{"event":{"significance":"0"}}`)

	overrides, err := mutation.EventOverrides(context.Background(), Facts{Event: Event{Group: "payments"}})
	if err != nil {
		t.Fatalf("EventOverrides: %s", err)
	}

	if overrides.Significance == nil || *overrides.Significance != 0 {
		t.Errorf("expected an explicit significance of 0, got %v", overrides.Significance)
	}

	if overrides.Group != nil || overrides.Name != nil || overrides.Description != nil || overrides.Metadata != nil {
		t.Errorf("expected the unnamed fields to be left alone, got %+v", overrides)
	}
}
