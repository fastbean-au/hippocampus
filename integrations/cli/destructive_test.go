package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// The destructive commands' flag mapping (TODO-3 item 166). A flag that silently fails to reach its
// field does not fail the request: it produces a WIDER filter that is still valid, and the service
// deletes accordingly. So every flag is pinned to the one field it sets, and the request must equal
// exactly that - an extra field is as wrong as a missing one - and a flag added to a command without
// a row here fails TestEveryDestructiveFlagIsMapped.

const flagTime = "2026-01-02T03:04:05Z"

func flagNanos(t *testing.T) int64 {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, flagTime)
	if err != nil {
		t.Fatalf("parse: %s", err)
	}

	return parsed.UnixNano()
}

type flagCase struct {
	flag  string
	value string
	want  func(t *testing.T) proto.Message
}

func memoryDeleteCases() []flagCase {
	request := func(edit func(r *contract.DeleteMemoriesByFilterRequest)) func(t *testing.T) proto.Message {
		return func(t *testing.T) proto.Message {
			r := &contract.DeleteMemoriesByFilterRequest{}
			edit(r)

			return r
		}
	}

	return []flagCase{
		{"timestamp-min", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteMemoriesByFilterRequest{TimestampMin: flagNanos(t)}
		}},
		{"timestamp-max", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteMemoriesByFilterRequest{TimestampMax: flagNanos(t)}
		}},
		{"significance-min", "3", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.SignificanceMin = 3 })},
		{"significance-max", "4", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.SignificanceMax = 4 })},
		{"group", "acme", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.Group = "acme" })},
		{"extremum", "lowest", request(func(r *contract.DeleteMemoriesByFilterRequest) {
			r.SignificanceExtremum = contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST
		})},
		{"metadata", "tenant=acme", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.Metadata = []string{"tenant=acme"} })},
		{"recalled", "false", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.Recalled = contract.Bool_FALSE })},
		{"summary", "true", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.IsSummary = contract.Bool_TRUE })},
		{"binary", "false", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.IsBinary = contract.Bool_FALSE })},
		{"recall-count-min", "2", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.RecallCountMin = 2 })},
		{"recall-count-max", "5", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.RecallCountMax = 5 })},
		{"recalled-after", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteMemoriesByFilterRequest{TimeRecalledMin: flagNanos(t)}
		}},
		{"recalled-before", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteMemoriesByFilterRequest{TimeRecalledMax: flagNanos(t)}
		}},
		{"event", "e1", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.EventId = "e1" })},
		{"has-event", "true", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.HasEvent = contract.Bool_TRUE })},
		{"max-deletions", "10", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.MaxDeletions = 10 })},
		{"delete-empty-events", "true", request(func(r *contract.DeleteMemoriesByFilterRequest) { r.DeleteEmptyEvents = true })},
	}
}

func eventDeleteCases() []flagCase {
	request := func(edit func(r *contract.DeleteEventsByFilterRequest)) func(t *testing.T) proto.Message {
		return func(t *testing.T) proto.Message {
			r := &contract.DeleteEventsByFilterRequest{}
			edit(r)

			return r
		}
	}

	return []flagCase{
		{"time-start-min", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteEventsByFilterRequest{TimeStartMin: flagNanos(t)}
		}},
		{"time-start-max", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteEventsByFilterRequest{TimeStartMax: flagNanos(t)}
		}},
		{"time-end-min", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteEventsByFilterRequest{TimeEndMin: flagNanos(t)}
		}},
		{"time-end-max", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteEventsByFilterRequest{TimeEndMax: flagNanos(t)}
		}},
		{"significance-min", "3", request(func(r *contract.DeleteEventsByFilterRequest) { r.SignificanceMin = 3 })},
		{"significance-max", "4", request(func(r *contract.DeleteEventsByFilterRequest) { r.SignificanceMax = 4 })},
		{"group", "acme", request(func(r *contract.DeleteEventsByFilterRequest) { r.Group = "acme" })},
		{"extremum", "highest", request(func(r *contract.DeleteEventsByFilterRequest) {
			r.SignificanceExtremum = contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST
		})},
		{"metadata", "tenant=acme", request(func(r *contract.DeleteEventsByFilterRequest) { r.Metadata = []string{"tenant=acme"} })},
		{"ended", "true", request(func(r *contract.DeleteEventsByFilterRequest) { r.Ended = contract.Bool_TRUE })},
		{"name-contains", "deploy", request(func(r *contract.DeleteEventsByFilterRequest) { r.NameContains = "deploy" })},
		{"max-deletions", "7", request(func(r *contract.DeleteEventsByFilterRequest) { r.MaxDeletions = 7 })},
		{"delete-memories", "true", request(func(r *contract.DeleteEventsByFilterRequest) { r.DeleteMemories = true })},
	}
}

func forgottenClearCases() []flagCase {
	return []flagCase{
		{"before", flagTime, func(t *testing.T) proto.Message {
			return &contract.DeleteForgottenMemoriesRequest{BeforeTime: flagNanos(t)}
		}},
		{"all", "true", func(*testing.T) proto.Message { return &contract.DeleteForgottenMemoriesRequest{All: true} }},
	}
}

// confirmed is the flags a destructive command needs to run at all: --yes for the predicate deletes,
// and nothing for forgotten clear, whose own --before/--all are the confirmation.
var destructiveCommands = map[string]struct {
	confirm []string
	cases   func() []flagCase
}{
	"memory delete-by-filter": {[]string{"--yes"}, memoryDeleteCases},
	"event delete-by-filter":  {[]string{"--yes"}, eventDeleteCases},
	"forgotten clear":         {nil, forgottenClearCases},
}

func TestDestructiveFlagsMapToExactlyTheirField(t *testing.T) {
	for command, spec := range destructiveCommands {
		for _, c := range spec.cases() {
			t.Run(command+"/"+c.flag, func(t *testing.T) {
				args := append(append([]string{}, spec.confirm...), "--"+c.flag+"="+c.value)

				req, _, err := runCommand(t, command, args, &fakeClient{})
				if err != nil {
					t.Fatalf("run: %v", err)
				}

				if want := c.want(t); !proto.Equal(req, want) {
					t.Errorf("--%s=%s built %v, want exactly %v", c.flag, c.value, req, want)
				}
			})
		}
	}
}

// TestEveryDestructiveFlagIsMapped is what makes the table above a guard rather than a sample: a flag
// registered on one of these commands with no row fails here, so a new filter cannot ship without its
// mapping being pinned.
func TestEveryDestructiveFlagIsMapped(t *testing.T) {
	global := pflag.NewFlagSet("global", pflag.ContinueOnError)
	registerGlobalFlags(global)

	for command, spec := range destructiveCommands {
		covered := map[string]bool{"yes": true}

		for _, c := range spec.cases() {
			covered[c.flag] = true
		}

		fs := pflag.NewFlagSet(command, pflag.ContinueOnError)
		commands()[command].flags(fs)

		fs.VisitAll(func(f *pflag.Flag) {
			if global.Lookup(f.Name) != nil || covered[f.Name] {
				return
			}

			t.Errorf("%s registers --%s with no mapping case in this file", command, f.Name)
		})
	}
}

// TestDestructiveCommandsRefuseWithoutConfirmation: nothing reaches the service without --yes (or,
// for the forgotten log, --before/--all), whatever else is set.
func TestDestructiveCommandsRefuseWithoutConfirmation(t *testing.T) {
	cases := map[string][]string{
		"memory delete-by-filter": {"--group", "acme"},
		"event delete-by-filter":  {"--group", "acme", "--delete-memories"},
		"forgotten clear":         nil,
	}

	for command, args := range cases {
		t.Run(command, func(t *testing.T) {
			fake := &fakeClient{}

			_, _, err := runCommand(t, command, args, fake)
			if err == nil {
				t.Fatal("ran without confirmation")
			}

			if fake.req != nil {
				t.Errorf("sent %v before refusing", fake.req)
			}

			if !strings.Contains(err.Error(), "--") {
				t.Errorf("the refusal %q does not say what to pass", err)
			}
		})
	}
}

// TestDestructiveCommandsRejectMalformedValues: a value that does not parse fails the command rather
// than being dropped, which would widen the filter by exactly that bound.
func TestDestructiveCommandsRejectMalformedValues(t *testing.T) {
	cases := []struct {
		command string
		args    []string
	}{
		{"memory delete-by-filter", []string{"--yes", "--timestamp-min", "yesterday"}},
		{"memory delete-by-filter", []string{"--yes", "--recalled-after", "yesterday"}},
		{"memory delete-by-filter", []string{"--yes", "--extremum", "middle"}},
		{"memory delete-by-filter", []string{"--yes", "--recalled", "maybe"}},
		{"memory delete-by-filter", []string{"--yes", "--has-event", "maybe"}},
		{"event delete-by-filter", []string{"--yes", "--time-end-max", "soon"}},
		{"event delete-by-filter", []string{"--yes", "--extremum", "middle"}},
		{"event delete-by-filter", []string{"--yes", "--ended", "maybe"}},
		{"forgotten clear", []string{"--before", "yesterday"}},
	}

	for _, c := range cases {
		t.Run(c.command+" "+strings.Join(c.args, " "), func(t *testing.T) {
			fake := &fakeClient{}

			if _, _, err := runCommand(t, c.command, c.args, fake); err == nil {
				t.Fatalf("accepted %v", c.args)
			}

			if fake.req != nil {
				t.Errorf("sent %v despite a malformed value", fake.req)
			}
		})
	}
}
