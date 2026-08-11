package rules

import (
	"fmt"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
)

// Event is what a rule sees of the completed event under judgement. The field names are the `cel`
// tags, not the Go names, so an expression is written in the same vocabulary as the API and the
// stored schema (group, significance, time_start) rather than in Go's.
//
// The whole struct is the declared CEL environment: adding a field here widens what every rules file
// may reference, and REMOVING or RENAMING one turns a deployed rules file into a compile error at
// the next reload - which keeps the last good ruleset serving but stops the operator's intended
// change from landing. TestDeclaredEnvironment pins the set for exactly that reason.
type Event struct {
	Id           string            `cel:"id"`
	Name         string            `cel:"name"`
	Description  string            `cel:"description"`
	Group        string            `cel:"group"`
	Significance int64             `cel:"significance"`
	Metadata     map[string]string `cel:"metadata"`

	// TimeStart and TimeEnd are UnixNano, as everywhere else in this system. DurationSeconds is
	// derived rather than left to the expression, because deriving it from the two would put a
	// division by 1e9 in every rules file that cared.
	TimeStart       int64   `cel:"time_start"`
	TimeEnd         int64   `cel:"time_end"`
	DurationSeconds float64 `cel:"duration_seconds"`

	// MemoryCount and BodyBytes describe the event's memories without the expression having to
	// range over them, so the common shape rules (too few to bother with, too large to promote
	// whole) need no comprehension - and, via Ruleset.NeedsMemories, let the promoter skip building
	// the per-memory list entirely, which would otherwise copy every body into a second slice.
	MemoryCount int64 `cel:"memory_count"`
	BodyBytes   int64 `cel:"body_bytes"`

	// SignificanceMin/Max/Mean are the aggregates over the event's memories, present for the same
	// reason - "promote when anything in here was significant" should not need a comprehension.
	SignificanceMin  int64   `cel:"significance_min"`
	SignificanceMax  int64   `cel:"significance_max"`
	SignificanceMean float64 `cel:"significance_mean"`
}

// Memory is what a rule sees of one of the event's memories. Body is the plain body, so a rule can
// match on content - which is what makes the promoter, like the service's summarise package, a
// component with visibility into memory content.
type Memory struct {
	Id           string            `cel:"id"`
	Body         string            `cel:"body"`
	Significance int64             `cel:"significance"`
	IsBinary     bool              `cel:"is_binary"`
	IsSummary    bool              `cel:"is_summary"`
	RecallCount  int64             `cel:"recall_count"`
	TimeStamp    int64             `cel:"time_stamp"`
	Metadata     map[string]string `cel:"metadata"`
}

// Facts is one event's judgement input: the event itself and, when any rule asks for them, its
// memories.
type Facts struct {
	Event    Event
	Memories []Memory
}

// The CEL type names the native type provider derives from the Go types above - package name plus
// type name. They are referenced when declaring the variables, never by a rules file.
const (
	eventTypeName  = "rules.Event"
	memoryTypeName = "rules.Memory"
)

// envOptions selects which of the optional variables an environment declares. The base environment
// (both false) is the probe described in Parse; `memories` is the list every match expression may
// range over; `memory` is the singular binding a memory-scoped mutation expression is evaluated
// once per memory against.
type envOptions struct {
	memories bool
	memory   bool
}

// environments are the three compilation environments, built once per load and shared by every
// rule in the file. They differ only in those two declarations.
type environments struct {
	// match compiles match expressions and event-scoped mutation expressions - the same vocabulary,
	// so a rule sets a field using exactly what it matched on.
	match *cel.Env

	// memory adds the singular `memory`, for the per-memory half of a mutation block.
	memory *cel.Env

	// probe is match minus `memories`, which is how NeedsMemories is decided. See Parse.
	probe *cel.Env
}

func newEnvironments() (environments, error) {
	var out environments
	var err error

	if out.match, err = newEnv(envOptions{memories: true}); err != nil {
		return environments{}, err
	}

	if out.memory, err = newEnv(envOptions{memories: true, memory: true}); err != nil {
		return environments{}, err
	}

	if out.probe, err = newEnv(envOptions{}); err != nil {
		return environments{}, err
	}

	return out, nil
}

// newEnv builds one CEL environment.
func newEnv(opts envOptions) (*cel.Env, error) {
	envOpts := []cel.EnvOption{
		ext.NativeTypes(
			ext.ParseStructTags(true),
			reflect.TypeOf(Event{}),
			reflect.TypeOf(Memory{}),
		),
		cel.Variable("event", cel.ObjectType(eventTypeName)),

		// Indexing a metadata key that is absent is an EVALUATION ERROR in CEL, not an empty
		// string - and since most events carry only some labels, that is the mistake every rules
		// file makes first. Evaluate reports such an error rather than treating it as false (see
		// its doc comment), so the rule visibly fails instead of quietly never matching, but the
		// operator still needs a way to write the guarded form. Both are available: the portable
		// `'k' in event.metadata && event.metadata['k'] == 'v'`, and, with optional types enabled
		// here, the shorter `event.metadata[?'k'].orValue('') == 'v'`.
		cel.OptionalTypes(),

		// The string helpers (lowerAscii, split, replace, join, ...) on top of the built-in
		// contains/startsWith/matches. Body matching is most of what a content rule does.
		ext.Strings(),

		// math.least/math.greatest, which are what make a mutation expression's arithmetic safe to
		// write: a computed significance must land in a bounded range, and clamping it is otherwise
		// a nest of conditionals. CEL has no min/max built in.
		ext.Math(),
	}

	if opts.memories {
		envOpts = append(envOpts, cel.Variable("memories", cel.ListType(cel.ObjectType(memoryTypeName))))
	}

	if opts.memory {
		envOpts = append(envOpts, cel.Variable("memory", cel.ObjectType(memoryTypeName)))
	}

	env, err := cel.NewEnv(envOpts...)
	if err != nil {
		return nil, fmt.Errorf("building the CEL environment: %w", err)
	}

	return env, nil
}

// activation is the variable binding one evaluation sees. Memories is bound even when no rule
// references it (as an empty list), so an expression added to a running instance's rules file
// cannot fail on an unbound variable before the ruleset's NeedsMemories is re-read.
func (f Facts) activation() map[string]any {
	memories := f.Memories
	if memories == nil {
		memories = []Memory{}
	}

	return map[string]any{
		"event":    f.Event,
		"memories": memories,
	}
}

// memoryActivation is the binding a memory-scoped mutation expression sees: the same event and
// memory list, plus the one memory being written. The siblings stay in scope on purpose - ranking a
// memory against its own event ("at or above the mean") is most of what a per-memory expression is
// for, and it cannot be done from the memory alone.
func (f Facts) memoryActivation(memory Memory) map[string]any {
	activation := f.activation()
	activation["memory"] = memory

	return activation
}
