package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// watchFlags parses args against the watch command's own flags.
func watchFlags(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()

	fs := pflag.NewFlagSet("watch", pflag.ContinueOnError)
	commands()["watch"].flags(fs)

	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %s", args, err)
	}

	return fs
}

var watchedEvents = []*contract.WatchEvent{
	{
		Kind:  contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED,
		Time:  1,
		Items: []*contract.WatchItem{{Id: "m1", Group: "g"}},
	},
	{
		Kind:    contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN,
		Cause:   contract.DeleteCause_DELETE_CAUSE_CONSOLIDATION,
		Time:    2,
		Items:   []*contract.WatchItem{{Id: "m1"}, {Id: "m2"}},
		Dropped: 3,
	},
	{
		Kind:  contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
		Time:  3,
		Cycle: &contract.CycleReport{MemoriesConsolidated: 2},
	},
}

func TestRunWatchRendersEachEventOnALine(t *testing.T) {
	client := &fakeClient{watchEvents: watchedEvents}

	var out bytes.Buffer

	fs := watchFlags(t, "--kind", "memory-stored", "--kind", "memory-forgotten", "--kind", "sleep")

	if err := runWatch(context.Background(), client, fs, &renderer{out: &out}); err != nil {
		t.Fatalf("runWatch: %s", err)
	}

	request, ok := client.req.(*contract.WatchRequest)
	if !ok || len(request.GetKinds()) != 3 || request.GetKinds()[2] != contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED {
		t.Errorf("the request is %v, want the three kinds named", client.req)
	}

	text := out.String()

	for _, want := range []string{"memory-stored  m1", "memory-forgotten (consolidation)  m1, m2", "3 events dropped", "consolidated 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the output does not contain %q:\n%s", want, text)
		}
	}
}

func TestRunWatchWritesOneJSONObjectPerLine(t *testing.T) {
	var out bytes.Buffer

	if err := runWatch(context.Background(), &fakeClient{watchEvents: watchedEvents}, watchFlags(t), &renderer{out: &out, json: true}); err != nil {
		t.Fatalf("runWatch: %s", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	if len(lines) != len(watchedEvents) {
		t.Fatalf("wrote %d lines for %d events:\n%s", len(lines), len(watchedEvents), out.String())
	}

	for _, line := range lines {
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			t.Errorf("a line is not one JSON object: %s", line)
		}
	}
}

func TestRunWatchRefusesAnUnknownKind(t *testing.T) {
	if err := runWatch(context.Background(), &fakeClient{}, watchFlags(t, "--kind", "everything"), &renderer{out: &bytes.Buffer{}}); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

// TestWatchIsExemptFromTheRequestTimeout: --timeout-seconds would end a watch after thirty seconds.
func TestWatchIsExemptFromTheRequestTimeout(t *testing.T) {
	if !commands()["watch"].streaming {
		t.Error("watch is not marked streaming, so the per-request timeout ends it")
	}
}

// TestTheHTTPTransportRefusesWatch: the gateway cannot stream, so the HTTP client says so rather
// than issuing a request no route answers.
func TestTheHTTPTransportRefusesWatch(t *testing.T) {
	_, err := echoGatewayClient(t).Watch(context.Background(), &contract.WatchRequest{})

	if status.Code(err) != codes.Unimplemented {
		t.Errorf("Watch over HTTP = %v, want Unimplemented", err)
	}
}
