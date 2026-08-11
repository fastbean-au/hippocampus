// Package rules is the ingestor's admission policy: a JSON file of CEL expressions that decides,
// for each completed event, whether it is promoted to the central instance, promoted after being
// reduced, or dropped.
//
// It is deliberately a hard, deterministic gate - the opposite of the decay model the service
// itself implements. A dropped event is gone; nothing recalls it later.
package rules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

// Action is what happens to an event once a rule matches it.
type Action string

const (
	// ActionPromote sends the event and its memories to the target instance, then drains both from
	// the source.
	ActionPromote Action = "promote"

	// ActionDrop deletes the event and its memories from the source without sending anything.
	ActionDrop Action = "drop"
)

// Default bounds on one evaluation. Both exist because a rules file is operator input that runs
// against every completed event: an expression ranging over a large event's memories must not be
// able to stall the pass, and the cost limit catches that statically-ish (CEL costs the program as
// it runs) while the timeout catches whatever the cost model under-counts.
const (
	DefaultCostLimit   = uint64(1_000_000)
	DefaultEvalTimeout = 2 * time.Second

	// interruptCheckFrequency is how often the evaluator checks for context cancellation, in
	// evaluation steps. Low enough that the timeout is honoured promptly, high enough not to be the
	// dominant cost of a cheap expression.
	interruptCheckFrequency = 100
)

// Reduce narrows which of an event's memories are promoted. KeepTopN and MinSignificance compose
// (both are applied); Summarise is exclusive of them, because it replaces the whole set with one
// generated memory and there is nothing left for the other two to select from.
type Reduce struct {
	// KeepTopN promotes only the N most significant memories. 0 means no limit.
	KeepTopN int `json:"keepTopN,omitempty"`

	// MinSignificance promotes only memories at or above this significance. 0 means no bound, per
	// this system's usual rule.
	MinSignificance int32 `json:"minSignificance,omitempty"`

	// Summarise asks the SOURCE instance to replace the event's memories with one generated summary
	// (the SummariseMemories RPC) before promoting. It requires ollama.enabled on that instance;
	// the promoter fails the event loudly rather than quietly promoting everything if it is not.
	Summarise bool `json:"summarise,omitempty"`
}

// empty reports whether the reduction would do nothing, so the promoter can skip it entirely.
func (r Reduce) empty() bool {
	return r.KeepTopN <= 0 && r.MinSignificance <= 0 && !r.Summarise
}

// Rule is one entry in the file: a CEL expression and what to do with an event it matches.
type Rule struct {
	Name   string  `json:"name"`
	Expr   string  `json:"expr"`
	Action Action  `json:"action"`
	Reduce *Reduce `json:"reduce,omitempty"`

	// Set rewrites fields on the promoted copy - significance above all, which is what decides how
	// long the central store keeps what this rule admits. See set.go.
	Set *Set `json:"set,omitempty"`
}

// file is the on-disk JSON shape. defaultAction has no default value on purpose: a rules file that
// omitted it would silently drop every event no rule matched, which is the worst failure this
// component has, so it is required.
type file struct {
	DefaultAction Action `json:"defaultAction"`
	Rules         []Rule `json:"rules"`
}

// Options bound one evaluation. Zero values select the package defaults. They are process
// configuration (flags) rather than fields of the rules file, which stays about rules.
type Options struct {
	CostLimit   uint64
	EvalTimeout time.Duration
}

func (o Options) costLimit() uint64 {
	if o.CostLimit == 0 {
		return DefaultCostLimit
	}

	return o.CostLimit
}

func (o Options) evalTimeout() time.Duration {
	if o.EvalTimeout <= 0 {
		return DefaultEvalTimeout
	}

	return o.EvalTimeout
}

// compiledRule pairs a rule with its compiled programs. Compilation happens once, at load, so a
// broken expression is a load failure rather than a surprise on the first event that reaches it.
type compiledRule struct {
	rule     Rule
	program  cel.Program
	mutation *Mutation
}

// Ruleset is a loaded, compiled rules file: immutable once built, which is what lets the Watcher
// swap a whole one in atomically and lets one event be judged by one consistent set of rules.
type Ruleset struct {
	defaultAction Action
	rules         []compiledRule
	needsMemories bool
	evalTimeout   time.Duration
}

// Decision is the outcome of judging one event. Rule is the name of the rule that matched, or empty
// when the default action applied.
type Decision struct {
	Rule   string
	Action Action
	Reduce Reduce

	// Mutation is the matched rule's compiled Set, or nil where it declared none. It is carried on
	// the decision rather than looked up again by name so the promoter applies the mutation of
	// exactly the rule that decided, even across a ruleset reload.
	Mutation *Mutation
}

// DefaultAction returns the action taken when no rule matches.
func (r *Ruleset) DefaultAction() Action { return r.defaultAction }

// Rules returns how many rules the set holds.
func (r *Ruleset) Rules() int { return len(r.rules) }

// Mutating returns how many rules rewrite fields on the promoted copy. It is reported by
// --check-rules because a mutation is invisible in the promote/drop outcome a rules file is
// otherwise read for: an operator checking a file should be told that some of it also re-ranks.
func (r *Ruleset) Mutating() int {
	count := 0

	for _, compiled := range r.rules {
		if compiled.mutation == nil {
			continue
		}

		count++
	}

	return count
}

// NeedsMemories reports whether any rule references the memories list. When it is false the
// promoter can judge an event without materialising its memory bodies at all, which for a large
// event is the difference between reading a page of counts and reading the whole event.
func (r *Ruleset) NeedsMemories() bool { return r.needsMemories }

// Load reads and compiles a rules file.
func Load(path string, opts Options) (*Ruleset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading rules file %q: %w", path, err)
	}

	set, err := Parse(data, opts)
	if err != nil {
		return nil, fmt.Errorf("rules file %q: %w", path, err)
	}

	return set, nil
}

// Parse compiles a rules document. Every expression is compiled here, so the returned Ruleset is
// ready to evaluate and cannot fail for a reason the operator could have been told about earlier.
func Parse(data []byte, opts Options) (*Ruleset, error) {
	var f file

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&f); err != nil {
		return nil, fmt.Errorf("parsing rules: %w", err)
	}

	if err := validAction(f.DefaultAction); err != nil {
		return nil, fmt.Errorf("defaultAction: %w (it is required - a rules file that omitted it would silently drop every unmatched event)", err)
	}

	// Three environments, differing only in whether they declare `memories` and `memory`. The probe
	// is the same as the match environment minus `memories`, which is how NeedsMemories is decided:
	// the rule has already compiled cleanly against the full environment, so the only thing that can
	// newly fail against the probe is the reference this is looking for.
	envs, err := newEnvironments()
	if err != nil {
		return nil, err
	}

	set := &Ruleset{
		defaultAction: f.DefaultAction,
		evalTimeout:   opts.evalTimeout(),
	}

	names := make(map[string]struct{}, len(f.Rules))

	for i, rule := range f.Rules {
		label := rule.Name
		if label == "" {
			label = fmt.Sprintf("rule %d", i+1)
		}

		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}

		if _, seen := names[rule.Name]; seen {
			return nil, fmt.Errorf("%s: duplicate rule name", label)
		}

		names[rule.Name] = struct{}{}

		program, err := compileRule(envs.match, rule, opts.costLimit())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}

		mutation, err := compileSet(envs, rule, opts.costLimit())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", label, err)
		}

		if mutation != nil {
			mutation.evalTimeout = set.evalTimeout
		}

		if ruleNeedsMemories(envs.probe, rule) {
			set.needsMemories = true
		}

		set.rules = append(set.rules, compiledRule{rule: rule, program: program, mutation: mutation})
	}

	return set, nil
}

// validateRule checks everything about a rule that does not need the compiler.
func validateRule(rule Rule) error {
	if rule.Name == "" {
		return fmt.Errorf("name is required")
	}

	if rule.Expr == "" {
		return fmt.Errorf("expr is required")
	}

	if err := validAction(rule.Action); err != nil {
		return err
	}

	if err := validateSet(rule); err != nil {
		return err
	}

	if rule.Reduce == nil {
		return nil
	}

	if rule.Action != ActionPromote {
		return fmt.Errorf("reduce is only meaningful with action %q", ActionPromote)
	}

	if rule.Reduce.KeepTopN < 0 {
		return fmt.Errorf("reduce.keepTopN must not be negative")
	}

	if rule.Reduce.MinSignificance < 0 {
		return fmt.Errorf("reduce.minSignificance must not be negative")
	}

	// Summarise replaces the whole memory set with one generated memory, so there is nothing for a
	// selection to select from. Refusing the combination is better than silently ignoring half of
	// what the operator wrote.
	if rule.Reduce.Summarise && (rule.Reduce.KeepTopN > 0 || rule.Reduce.MinSignificance > 0) {
		return fmt.Errorf("reduce.summarise cannot be combined with keepTopN or minSignificance - it replaces every memory with one summary")
	}

	if rule.Reduce.empty() {
		return fmt.Errorf("reduce is present but selects nothing; omit it or set keepTopN, minSignificance or summarise")
	}

	return nil
}

func validAction(action Action) error {
	switch action {

	case ActionPromote, ActionDrop:
		return nil

	case "":
		return fmt.Errorf("action is required (%q or %q)", ActionPromote, ActionDrop)

	default:
		return fmt.Errorf("unknown action %q (expected %q or %q)", action, ActionPromote, ActionDrop)

	}
}

// compileRule type-checks an expression against the environment and builds its program. The result
// type is required to be bool: an expression yielding anything else would otherwise be silently
// falsy at evaluation, which reads as "this rule never matches" and is indistinguishable from a
// rule that is simply not being reached.
func compileRule(env *cel.Env, rule Rule, costLimit uint64) (cel.Program, error) {
	ast, issues := env.Compile(rule.Expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compiling expression: %w", issues.Err())
	}

	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("expression must evaluate to bool, got %s", ast.OutputType())
	}

	program, err := env.Program(
		ast,
		cel.CostLimit(costLimit),
		cel.InterruptCheckFrequency(interruptCheckFrequency),
	)
	if err != nil {
		return nil, fmt.Errorf("building program: %w", err)
	}

	return program, nil
}

// ruleNeedsMemories reports whether judging under this rule requires the event's memories to be
// read at all.
//
// A memory-scoped mutation forces it regardless of what its expressions reference: the promoter
// must hold the memories in order to rewrite them, even for a block as constant as
// `{"significance": "1"}`.
func ruleNeedsMemories(probeEnv *cel.Env, rule Rule) bool {
	if referencesMemories(probeEnv, rule.Expr) {
		return true
	}

	if rule.Set == nil {
		return false
	}

	if rule.Set.Memory != nil {
		return true
	}

	if rule.Set.Event == nil {
		return false
	}

	for _, expr := range rule.Set.Event.expressions() {
		if referencesMemories(probeEnv, expr) {
			return true
		}
	}

	return false
}

// referencesMemories reports whether an expression reads the memories list, by compiling it against
// an environment that does not declare one. The expression has already compiled against the full
// environment by the time this runs, so a failure here can only be the missing declaration.
func referencesMemories(probeEnv *cel.Env, expr string) bool {
	if expr == "" {
		return false
	}

	_, issues := probeEnv.Compile(expr)

	return issues != nil && issues.Err() != nil
}

// Evaluate judges one event, returning the first matching rule's decision or the default action.
//
// An expression that ERRORS at evaluation - a missing metadata key indexed directly, a cost limit
// trip, the timeout - does not match, and the error is returned alongside a usable decision so the
// caller can log it. That is deliberate: treating an evaluation error as a silent false would let a
// broken rule quietly change what is promoted, which is precisely the failure an admission gate
// must not have. Evaluation continues to the remaining rules, so one broken rule does not disable
// the ones after it.
func (r *Ruleset) Evaluate(ctx context.Context, facts Facts) (Decision, error) {
	activation := facts.activation()

	var firstErr error

	for _, compiled := range r.rules {
		matched, err := r.match(ctx, compiled, activation)
		if err != nil {
			if firstErr == nil {
				firstErr = &EvalError{Rule: compiled.rule.Name, Err: err}
			}

			continue
		}

		if !matched {
			continue
		}

		decision := Decision{
			Rule:     compiled.rule.Name,
			Action:   compiled.rule.Action,
			Mutation: compiled.mutation,
		}

		if compiled.rule.Reduce != nil {
			decision.Reduce = *compiled.rule.Reduce
		}

		return decision, firstErr
	}

	return Decision{Action: r.defaultAction}, firstErr
}

// match evaluates one compiled rule under the evaluation timeout.
func (r *Ruleset) match(ctx context.Context, compiled compiledRule, activation map[string]any) (bool, error) {
	evalCtx, cancel := context.WithTimeout(ctx, r.evalTimeout)
	defer cancel()

	out, _, err := compiled.program.ContextEval(evalCtx, activation)
	if err != nil {
		return false, err
	}

	return isTrue(out), nil
}

// EvalError is an evaluation failure, carrying the rule it came from so a caller can report and
// COUNT it per rule - a rule that errors on every event never matches, which is indistinguishable
// from a rule that simply does not apply unless the failure is attributed.
type EvalError struct {
	Rule string
	Err  error
}

func (e *EvalError) Error() string {
	return fmt.Sprintf("rule %q: %s", e.Rule, e.Err.Error())
}

func (e *EvalError) Unwrap() error {
	return e.Err
}

// isTrue reads the evaluation result. The compiler has already required a bool output type, so this
// is the runtime half of that guarantee rather than a conversion.
func isTrue(out ref.Val) bool {
	value, ok := out.(types.Bool)

	return ok && bool(value)
}
