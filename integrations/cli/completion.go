package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/pflag"

	"github.com/fastbean-au/hippocampus/contract"
)

// shortGlobalFlags maps the single-letter global flags to their long names so completion treats
// e.g. "-o json" the same as "--output json".
var shortGlobalFlags = map[string]string{
	"t": "transport",
	"a": "address",
	"o": "output",
}

// enumFlagValues lists the candidate values for flags whose input is a small closed set, so
// completion can offer them. Keys are long flag names.
var enumFlagValues = map[string][]string{
	"transport":  {"grpc", "http"},
	"output":     {"text", "json"},
	"log-level":  {"trace", "debug", "info", "warn", "error", "fatal", "panic"},
	"place-mode": {"above", "below", "between"},
	"extremum":   {"highest", "lowest"},
	"order-by":   {"significance", "timestamp"},
	"direction":  {"both", "outbound", "inbound"},

	// The tri-state list filters. They are string flags rather than pflag bools precisely so
	// "false" is expressible, so both values are worth offering.
	"recalled":  {"true", "false"},
	"summary":   {"true", "false"},
	"binary":    {"true", "false"},
	"has-event": {"true", "false"},
}

// completionShells are the shells `hippo completion` can emit a script for.
var completionShells = []string{"bash", "zsh", "fish"}

// runCompletionCmd is the handler for `hippo completion <shell>`: it prints the completion script
// for the named shell to the output stream. It touches no service, so the client is unused.
func runCompletionCmd(_ context.Context, _ contract.HippocampusClient, fs *pflag.FlagSet, r *renderer) error {
	shell := fs.Arg(0)
	if shell == "" {
		return fmt.Errorf("a shell is required: one of %s", strings.Join(completionShells, ", "))
	}

	script, ok := completionScript(shell)
	if !ok {
		return fmt.Errorf("unknown shell %q (expected one of %s)", shell, strings.Join(completionShells, ", "))
	}

	_, _ = fmt.Fprint(r.out, script)

	return nil
}

// runComplete backs the hidden `hippo __complete` command that the emitted shell scripts call at
// completion time. prior is the already-typed words after `hippo` (excluding the word under the
// cursor); it prints one candidate per line for the position being completed. It never errors —
// completion must stay quiet — and needs no service connection.
func runComplete(prior []string, out io.Writer) error {
	for _, candidate := range completeArgs(prior) {
		_, _ = fmt.Fprintln(out, candidate)
	}

	return nil
}

// completeArgs computes the completion candidates for the next word given the already-typed prior
// words. The shell filters the returned list by the current partial word, so this returns every
// candidate valid at the position rather than prefix-matching itself.
func completeArgs(prior []string) []string {
	positional := nonFlagPositional(prior)

	// If the previous word is a flag expecting a value, offer that flag's values (for the closed-set
	// flags) or nothing (for free-form values), rather than the next flag/command.
	if len(prior) > 0 {
		if name, isFlag, inline := flagInfo(prior[len(prior)-1]); isFlag && !inline {
			if values, ok := enumFlagValues[name]; ok {
				return values
			}

			if takesNonBoolValue(positional, name) {
				return nil
			}
		}
	}

	if key, resolved := resolveKey(positional); resolved {
		candidates := flagCandidates(key, prior)

		// `completion` takes a shell name as its first positional argument.
		if key == "completion" && len(positional) == 1 {
			return append(append([]string{}, completionShells...), candidates...)
		}

		return candidates
	}

	switch len(positional) {

	case 0:
		return firstTokens()

	case 1:
		if isGroup(positional[0]) {
			return verbsFor(positional[0])
		}
	}

	return nil
}

// flagInfo classifies a command-line word: whether it is a flag, its long name (single-letter global
// flags are normalised to their long name), and whether it already carries an inline =value.
func flagInfo(word string) (name string, isFlag bool, inlineValue bool) {
	switch {

	case strings.HasPrefix(word, "--"):
		if body, _, found := strings.Cut(word[2:], "="); found {
			return body, true, true
		}

		return word[2:], true, false

	case strings.HasPrefix(word, "-") && len(word) > 1:
		body, _, inline := strings.Cut(word[1:], "=")

		if long, ok := shortGlobalFlags[body]; ok {
			return long, true, inline
		}

		return body, true, inline

	default:
		return "", false, false
	}
}

// nonFlagPositional strips flags (and the values of value-taking global flags) from words, leaving
// the positional tokens used to locate the command. Stripping only global value-flags is enough:
// command-specific flags appear after the one or two command tokens, which resolveKey reads first.
func nonFlagPositional(words []string) []string {
	globals := globalValueFlags()

	var out []string

	skipValue := false

	for _, word := range words {
		if skipValue {
			skipValue = false

			continue
		}

		name, isFlag, inline := flagInfo(word)
		if isFlag {
			if !inline && globals[name] {
				skipValue = true
			}

			continue
		}

		out = append(out, word)
	}

	return out
}

// resolveKey locates the registry command from the positional tokens, preferring the two-word key.
func resolveKey(positional []string) (string, bool) {
	registry := commands()

	if len(positional) >= 2 {
		if _, ok := registry[positional[0]+" "+positional[1]]; ok {
			return positional[0] + " " + positional[1], true
		}
	}

	if len(positional) >= 1 {
		if _, ok := registry[positional[0]]; ok {
			return positional[0], true
		}
	}

	return "", false
}

// firstTokens returns the distinct first words of every command (group names plus single-word
// commands), sorted.
func firstTokens() []string {
	seen := map[string]bool{}

	var out []string

	for key := range commands() {
		head, _, _ := strings.Cut(key, " ")

		if !seen[head] {
			seen[head] = true

			out = append(out, head)
		}
	}

	sort.Strings(out)

	return out
}

// isGroup reports whether token is the first word of a two-word command (e.g. "memory").
func isGroup(token string) bool {
	for key := range commands() {
		if strings.HasPrefix(key, token+" ") {
			return true
		}
	}

	return false
}

// verbsFor returns the second words of every two-word command in the given group, sorted.
func verbsFor(group string) []string {
	prefix := group + " "

	var out []string

	for key := range commands() {
		if verb, found := strings.CutPrefix(key, prefix); found {
			out = append(out, verb)
		}
	}

	sort.Strings(out)

	return out
}

// flagCandidates returns the "--flag" names available for the resolved command (its own flags plus
// the global flags), excluding non-repeatable flags already present in prior, sorted.
func flagCandidates(key string, prior []string) []string {
	fs := pflag.NewFlagSet(key, pflag.ContinueOnError)
	registerGlobalFlags(fs)
	commands()[key].flags(fs)

	used := map[string]bool{}

	for _, word := range prior {
		name, isFlag, _ := flagInfo(word)
		if !isFlag {
			continue
		}

		if f := fs.Lookup(name); f != nil && !isRepeatable(f) {
			used[name] = true
		}
	}

	var out []string

	fs.VisitAll(func(f *pflag.Flag) {
		if !used[f.Name] {
			out = append(out, "--"+f.Name)
		}
	})

	sort.Strings(out)

	return out
}

// takesNonBoolValue reports whether the named flag (global, or a flag of the resolved command)
// expects a value, so completion can decline to offer flag/command candidates in a value position.
func takesNonBoolValue(positional []string, name string) bool {
	fs := pflag.NewFlagSet("complete", pflag.ContinueOnError)
	registerGlobalFlags(fs)

	if key, ok := resolveKey(positional); ok {
		commands()[key].flags(fs)
	}

	if f := fs.Lookup(name); f != nil {
		return f.Value.Type() != "bool"
	}

	return false
}

// globalValueFlags returns the set of global flag names that take a value (everything but the bools).
func globalValueFlags() map[string]bool {
	fs := pflag.NewFlagSet("globals", pflag.ContinueOnError)
	registerGlobalFlags(fs)

	set := map[string]bool{}

	fs.VisitAll(func(f *pflag.Flag) {
		if f.Value.Type() != "bool" {
			set[f.Name] = true
		}
	})

	return set
}

// isRepeatable reports whether a flag accepts multiple values (a slice/array flag), so completion
// keeps offering it after one value.
func isRepeatable(f *pflag.Flag) bool {
	t := f.Value.Type()

	return strings.Contains(t, "Slice") || strings.Contains(t, "Array")
}

// completionScript returns the shell completion script for the named shell. Each script wires the
// shell's completion mechanism to call `hippo __complete` with the already-typed words, so the
// candidates always match the registered commands and flags.
func completionScript(shell string) (string, bool) {
	switch shell {

	case "bash":
		return bashCompletion, true

	case "zsh":
		return zshCompletion, true

	case "fish":
		return fishCompletion, true

	default:
		return "", false
	}
}

const bashCompletion = `# hippo bash completion. Load with: source <(hippo completion bash)
_hippo() {
    local cur prior candidates
    cur="${COMP_WORDS[COMP_CWORD]}"
    prior=("${COMP_WORDS[@]:1:COMP_CWORD-1}")
    local IFS=$'\n'
    candidates="$(hippo __complete "${prior[@]}" 2>/dev/null)"
    COMPREPLY=($(compgen -W "${candidates}" -- "${cur}"))
}
complete -F _hippo hippo
`

const zshCompletion = `# hippo zsh completion. Load with: source <(hippo completion zsh)
# (requires 'autoload -U compinit; compinit' to have run)
_hippo() {
    local -a prior candidates
    prior=("${words[@]:1:$((CURRENT-2))}")
    candidates=("${(@f)$(hippo __complete "${prior[@]}" 2>/dev/null)}")
    compadd -- "${candidates[@]}"
}
compdef _hippo hippo
`

const fishCompletion = `# hippo fish completion. Load with: hippo completion fish | source
function __hippo_complete
    set -l prior (commandline -opc)
    set -e prior[1]
    hippo __complete $prior 2>/dev/null
end
complete -c hippo -f -a '(__hippo_complete)'
`
