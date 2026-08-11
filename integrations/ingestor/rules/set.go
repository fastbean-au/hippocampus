package rules

import (
	"context"
	"fmt"
	"maps"
	"math"
	"reflect"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"

	hippotypes "github.com/fastbean-au/hippocampus/types"
)

// Bounds on what an expression may produce, mirroring the service's own validation
// (types/event.go, types/memory.go). They are checked HERE rather than left to the target instance
// for one reason: a value the target rejects fails the whole ImportBatch, and the event then sits
// on the edge being re-judged and re-refused every pass. Catching it in the ingestor turns that
// into one attributable error naming the rule and the field.
const (
	maxSignificance      = math.MaxInt32
	maxGroupLength       = 128
	maxNameLength        = 256
	maxDescriptionLength = 1024
)

// Set is a rule's mutation block: expressions whose values are written onto the copy that crosses
// to the target instance. Nothing here touches the source - the edge is drained either way, so
// mutating it would be a write nobody reads.
//
// It is what makes the ingestor an admission gate that can also RE-RANK. An edge knows things the
// central store cannot: that this event came from production, that a memory mentioning a stack
// trace matters more than the ten around it. Significance is the number the whole decay model runs
// on, so setting it at the crossing decides how long the central store keeps what it accepts -
// which is a stronger lever than promote-or-drop alone.
type Set struct {
	Event  *EventSet  `json:"event,omitempty"`
	Memory *MemorySet `json:"memory,omitempty"`
}

// EventSet and MemorySet are deliberately SEPARATE types carrying almost the same fields, rather
// than one shared type. The parser rejects unknown fields, so `set.memory.name` - a field a memory
// does not have - is a load-time error naming the mistake, where a shared struct would accept it
// and silently do nothing.
type EventSet struct {
	Significance string `json:"significance,omitempty"`
	Group        string `json:"group,omitempty"`
	Name         string `json:"name,omitempty"`
	Description  string `json:"description,omitempty"`
	Metadata     string `json:"metadata,omitempty"`
}

// MemorySet is the per-memory half. There is deliberately no `body`: rewriting content is what the
// summarise reduction is for, and a rules file that could rewrite bodies would make the promoted
// copy something the edge never held.
type MemorySet struct {
	Significance string `json:"significance,omitempty"`
	Group        string `json:"group,omitempty"`
	Metadata     string `json:"metadata,omitempty"`
}

func (s *EventSet) empty() bool {
	return len(s.expressions()) == 0
}

// expressions lists the expressions the block declares, for the load-time checks that treat them
// uniformly (is anything set at all; does any of them read the memory list).
func (s *EventSet) expressions() []string {
	out := make([]string, 0, 5)

	for _, expr := range []string{s.Significance, s.Group, s.Name, s.Description, s.Metadata} {
		if expr == "" {
			continue
		}

		out = append(out, expr)
	}

	return out
}

func (s *MemorySet) empty() bool {
	return s.Significance == "" && s.Group == "" && s.Metadata == ""
}

// Overrides is what one evaluation produced: the fields to write onto the promoted copy, and only
// those. A nil pointer means the rule said nothing about that field, which is NOT the same as
// setting it to its zero value - significance 0 means "unranked" and an empty group means "no
// group", both of which a rule may legitimately ask for.
type Overrides struct {
	Significance *int32
	Group        *string
	Name         *string
	Description  *string

	// Metadata is the MERGED map - the record's existing entries with the expression's stamped over
	// them - not the expression's result alone. Merging here rather than at the call site is what
	// lets it be validated as the map that will actually be written, since the bounds that matter
	// (32 keys, 4096 serialised bytes) apply to the total.
	Metadata map[string]string
}

// Empty reports whether the evaluation asked for nothing at all.
func (o Overrides) Empty() bool {
	return o.Significance == nil && o.Group == nil && o.Name == nil && o.Description == nil && o.Metadata == nil
}

// fieldPrograms holds one scope's compiled expressions. A nil program is a field the rule did not
// mention; Name and Description are always nil in the memory scope.
type fieldPrograms struct {
	significance cel.Program
	group        cel.Program
	name         cel.Program
	description  cel.Program
	metadata     cel.Program
}

// Mutation is a compiled Set, carried on the Decision so the promoter can evaluate it against each
// record without knowing anything about CEL. It carries the rule's name so that a failure is
// attributable to the rule that caused it, exactly as a match failure is.
type Mutation struct {
	rule        string
	event       *fieldPrograms
	memory      *fieldPrograms
	evalTimeout time.Duration
}

// HasEvent and HasMemory report which scopes the rule declared, so the promoter can skip building
// an activation for a scope nothing will read.
func (m *Mutation) HasEvent() bool { return m != nil && m.event != nil }

func (m *Mutation) HasMemory() bool { return m != nil && m.memory != nil }

// EventOverrides evaluates the event-scoped expressions against the facts the rule was judged on -
// the same facts, deliberately: a mutation reasons about the event the operator's expression
// matched, not about a reduced or summarised version of it that no rule ever saw.
func (m *Mutation) EventOverrides(ctx context.Context, facts Facts) (Overrides, error) {
	if !m.HasEvent() {
		return Overrides{}, nil
	}

	return m.apply(ctx, evaluation{
		programs:   m.event,
		activation: facts.activation(),
		existing:   facts.Event.Metadata,
		kind:       "event",
	})
}

// MemoryOverrides evaluates the memory-scoped expressions for one memory. The activation carries
// the event and the whole memory list as well, so an expression can rank a memory against its
// siblings ("above the event's mean") rather than only against constants.
func (m *Mutation) MemoryOverrides(ctx context.Context, facts Facts, memory Memory) (Overrides, error) {
	if !m.HasMemory() {
		return Overrides{}, nil
	}

	return m.apply(ctx, evaluation{
		programs:   m.memory,
		activation: facts.memoryActivation(memory),
		existing:   memory.Metadata,
		kind:       "memory",
	})
}

// evaluation is one scope's inputs, bundled so apply stays a two-argument method.
type evaluation struct {
	programs   *fieldPrograms
	activation map[string]any
	existing   map[string]string
	kind       string
}

// apply evaluates every declared expression for one record and validates each result.
//
// All of them are evaluated against the SAME view of the record: nothing here reads back a value an
// earlier expression in the same block produced, so `group` and `metadata` both see the original
// significance whatever order they are written in. A block whose fields could observe each other
// would make the result depend on an evaluation order the file does not state.
func (m *Mutation) apply(ctx context.Context, ev evaluation) (Overrides, error) {
	var out Overrides

	if program := ev.programs.significance; program != nil {
		value, err := m.evalInt(ctx, program, ev.activation)
		if err != nil {
			return Overrides{}, m.fieldError(ev.kind, "significance", err)
		}

		if value < 0 || value > maxSignificance {
			return Overrides{}, m.fieldError(ev.kind, "significance", fmt.Errorf(
				"%d is out of range (0 to %d) - significance is a non-negative int32, and 0 means unranked",
				value, maxSignificance,
			))
		}

		significance := int32(value)
		out.Significance = &significance
	}

	if err := m.applyStrings(ctx, ev, &out); err != nil {
		return Overrides{}, err
	}

	if program := ev.programs.metadata; program != nil {
		stamped, err := m.evalMap(ctx, program, ev.activation)
		if err != nil {
			return Overrides{}, m.fieldError(ev.kind, "metadata", err)
		}

		merged := mergeMetadata(ev.existing, stamped)

		// Validated with the service's own validator rather than a copy of its rules, so the key
		// charset and the byte budget cannot drift from what the target will accept.
		if err := hippotypes.ValidateMetadata(merged, ev.kind); err != nil {
			return Overrides{}, m.fieldError(ev.kind, "metadata", err)
		}

		out.Metadata = merged
	}

	return out, nil
}

// applyStrings covers the three string-valued fields, whose only difference is their length bound.
func (m *Mutation) applyStrings(ctx context.Context, ev evaluation, out *Overrides) error {
	fields := []struct {
		name    string
		program cel.Program
		max     int
		target  **string
	}{
		{"group", ev.programs.group, maxGroupLength, &out.Group},
		{"name", ev.programs.name, maxNameLength, &out.Name},
		{"description", ev.programs.description, maxDescriptionLength, &out.Description},
	}

	for _, field := range fields {
		if field.program == nil {
			continue
		}

		value, err := m.evalString(ctx, field.program, ev.activation)
		if err != nil {
			return m.fieldError(ev.kind, field.name, err)
		}

		if len(value) > field.max {
			return m.fieldError(ev.kind, field.name, fmt.Errorf("%d bytes exceeds the %d-byte limit", len(value), field.max))
		}

		// An event with no name cannot be imported at all, so an expression that produced one would
		// fail the whole batch on the far side. Refused here, where the rule can be named.
		if field.name == "name" && value == "" {
			return m.fieldError(ev.kind, field.name, fmt.Errorf("an event name must not be empty"))
		}

		*field.target = &value
	}

	return nil
}

// fieldError attributes a failure to the rule, the scope and the field, so the operator is told
// which of several expressions in one block went wrong. The EvalError wrapper is what makes it
// countable per rule alongside match failures.
func (m *Mutation) fieldError(kind string, field string, err error) error {
	return &EvalError{Rule: m.rule, Err: fmt.Errorf("set.%s.%s: %w", kind, field, err)}
}

// evalValue runs one field expression under the same wall-clock bound a match evaluation gets. The
// cost limit is baked into the program at compile time, as it is for a match.
func (m *Mutation) evalValue(ctx context.Context, program cel.Program, activation map[string]any) (ref.Val, error) {
	evalCtx, cancel := context.WithTimeout(ctx, m.evalTimeout)
	defer cancel()

	out, _, err := program.ContextEval(evalCtx, activation)
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (m *Mutation) evalInt(ctx context.Context, program cel.Program, activation map[string]any) (int64, error) {
	out, err := m.evalValue(ctx, program, activation)
	if err != nil {
		return 0, err
	}

	value, err := out.ConvertToNative(reflect.TypeFor[int64]())
	if err != nil {
		return 0, fmt.Errorf("reading the result as an int: %w", err)
	}

	native, ok := value.(int64)
	if !ok {
		return 0, fmt.Errorf("expression produced %T, not an int", value)
	}

	return native, nil
}

func (m *Mutation) evalString(ctx context.Context, program cel.Program, activation map[string]any) (string, error) {
	out, err := m.evalValue(ctx, program, activation)
	if err != nil {
		return "", err
	}

	value, err := out.ConvertToNative(reflect.TypeFor[string]())
	if err != nil {
		return "", fmt.Errorf("reading the result as a string: %w", err)
	}

	native, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("expression produced %T, not a string", value)
	}

	return native, nil
}

// evalMap converts through ConvertToNative rather than reading ref.Val.Value() directly, because a
// map built by an expression holds ref.Vals rather than Go strings - and because that conversion is
// what turns a map(dyn, dyn) (which is what `{}` and a conditional's join compile to) into a clear
// error instead of a panic.
func (m *Mutation) evalMap(ctx context.Context, program cel.Program, activation map[string]any) (map[string]string, error) {
	out, err := m.evalValue(ctx, program, activation)
	if err != nil {
		return nil, err
	}

	value, err := out.ConvertToNative(reflect.TypeFor[map[string]string]())
	if err != nil {
		return nil, fmt.Errorf("reading the result as map(string, string): %w", err)
	}

	native, ok := value.(map[string]string)
	if !ok {
		return nil, fmt.Errorf("expression produced %T, not map(string, string)", value)
	}

	return native, nil
}

// mergeMetadata stamps the expression's entries over the record's existing ones.
//
// Merge rather than replace, which is the opposite of UpdateMemory's wholesale-replace semantics,
// and deliberately: CEL has no map union operator, so a replacing `metadata` could not express
// "keep what is there and add a provenance label" at all - the overwhelmingly common case here.
// The cost is that a promotion cannot REMOVE a key; nothing in the file can, today.
func mergeMetadata(existing map[string]string, stamped map[string]string) map[string]string {
	merged := make(map[string]string, len(existing)+len(stamped))

	maps.Copy(merged, existing)
	maps.Copy(merged, stamped)

	return merged
}

// valueSpec is one field expression awaiting compilation, bundled so compileInto stays within four
// parameters.
type valueSpec struct {
	// field is the dotted path as it appears in the file (`set.event.significance`), so a compile
	// error points at the line the operator wrote.
	field string
	expr  string

	// want is the required output type, and description names it in the error message. Requiring
	// int rather than accepting double is deliberate: `event.significance_mean * 2` is a double,
	// and truncating it silently would decide a rank the file did not state - `int(...)` says it.
	want        *cel.Type
	description string
}

// compileInto compiles one field expression when the file declared it, assigning the program
// through dst. An absent expression is not an error - a block sets the fields it names.
func compileInto(env *cel.Env, spec valueSpec, costLimit uint64, dst *cel.Program) error {
	if spec.expr == "" {
		return nil
	}

	ast, issues := env.Compile(spec.expr)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("%s: compiling expression: %w", spec.field, issues.Err())
	}

	if !ast.OutputType().IsExactType(spec.want) {
		return fmt.Errorf(
			"%s: expression must evaluate to %s, got %s",
			spec.field,
			spec.description,
			ast.OutputType(),
		)
	}

	program, err := env.Program(
		ast,
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(interruptCheckFrequency),
	)
	if err != nil {
		return fmt.Errorf("%s: building program: %w", spec.field, err)
	}

	*dst = program

	return nil
}

func intSpec(field string, expr string) valueSpec {
	return valueSpec{field: field, expr: expr, want: cel.IntType, description: "int"}
}

func stringSpec(field string, expr string) valueSpec {
	return valueSpec{field: field, expr: expr, want: cel.StringType, description: "string"}
}

func mapSpec(field string, expr string) valueSpec {
	return valueSpec{
		field:       field,
		expr:        expr,
		want:        cel.MapType(cel.StringType, cel.StringType),
		description: "map(string, string)",
	}
}

// compile builds the event scope's programs. The environment is the match environment, so an
// event-scoped expression sees exactly what the rule that matched saw.
func (s *EventSet) compile(env *cel.Env, costLimit uint64) (*fieldPrograms, error) {
	out := &fieldPrograms{}

	specs := []struct {
		spec valueSpec
		dst  *cel.Program
	}{
		{intSpec("set.event.significance", s.Significance), &out.significance},
		{stringSpec("set.event.group", s.Group), &out.group},
		{stringSpec("set.event.name", s.Name), &out.name},
		{stringSpec("set.event.description", s.Description), &out.description},
		{mapSpec("set.event.metadata", s.Metadata), &out.metadata},
	}

	for _, entry := range specs {
		if err := compileInto(env, entry.spec, costLimit, entry.dst); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// compile builds the memory scope's programs, against the environment that also declares `memory`.
func (s *MemorySet) compile(env *cel.Env, costLimit uint64) (*fieldPrograms, error) {
	out := &fieldPrograms{}

	specs := []struct {
		spec valueSpec
		dst  *cel.Program
	}{
		{intSpec("set.memory.significance", s.Significance), &out.significance},
		{stringSpec("set.memory.group", s.Group), &out.group},
		{mapSpec("set.memory.metadata", s.Metadata), &out.metadata},
	}

	for _, entry := range specs {
		if err := compileInto(env, entry.spec, costLimit, entry.dst); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// compileSet builds a rule's whole mutation block. envs carries the two environments because the
// two scopes differ by one declared variable.
func compileSet(envs environments, rule Rule, costLimit uint64) (*Mutation, error) {
	if rule.Set == nil {
		return nil, nil
	}

	mutation := &Mutation{rule: rule.Name}

	if rule.Set.Event != nil {
		programs, err := rule.Set.Event.compile(envs.match, costLimit)
		if err != nil {
			return nil, err
		}

		mutation.event = programs
	}

	if rule.Set.Memory != nil {
		programs, err := rule.Set.Memory.compile(envs.memory, costLimit)
		if err != nil {
			return nil, err
		}

		mutation.memory = programs
	}

	return mutation, nil
}

// validateSet checks everything about a mutation block that does not need the compiler.
func validateSet(rule Rule) error {
	if rule.Set == nil {
		return nil
	}

	if rule.Action != ActionPromote {
		return fmt.Errorf("set is only meaningful with action %q - a dropped event has no promoted copy to write to", ActionPromote)
	}

	if rule.Set.Event == nil && rule.Set.Memory == nil {
		return fmt.Errorf("set is present but names neither event nor memory")
	}

	if rule.Set.Event != nil && rule.Set.Event.empty() {
		return fmt.Errorf("set.event is present but sets no field")
	}

	if rule.Set.Memory != nil && rule.Set.Memory.empty() {
		return fmt.Errorf("set.memory is present but sets no field")
	}

	return nil
}
