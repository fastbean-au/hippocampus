package main

import (
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestLinkCommands_HappyPaths covers the six link commands. Memories and events take the same three
// shapes, so each pair differs only in which RPC it calls - and that is precisely what a test has to
// pin, since a handler wired to the wrong graph would still compile and still render.
func TestLinkCommands_HappyPaths(t *testing.T) {
	t.Run("memory link", func(t *testing.T) {
		req, _, err := runCommand(t, "memory link", []string{"--id", "m1", "--link", "m2:5"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.LinkMemoriesRequest)
		if !ok {
			t.Fatalf("expected a LinkMemoriesRequest, got %T", req)
		}

		if in.GetId() != "m1" || len(in.GetLinks()) != 1 {
			t.Fatalf("unexpected request: %+v", in)
		}

		if in.GetLinks()[0].GetId() != "m2" || in.GetLinks()[0].GetSignificance() != 5 {
			t.Errorf("unexpected link: %+v", in.GetLinks()[0])
		}
	})

	t.Run("event link", func(t *testing.T) {
		req, _, err := runCommand(t, "event link", []string{"--id", "e1", "--link", "e2:3"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.LinkEventsRequest)
		if !ok {
			t.Fatalf("expected a LinkEventsRequest, got %T", req)
		}

		if in.GetId() != "e1" || len(in.GetLinks()) != 1 || in.GetLinks()[0].GetId() != "e2" {
			t.Errorf("unexpected request: %+v", in)
		}
	})

	t.Run("memory unlink", func(t *testing.T) {
		req, _, err := runCommand(t, "memory unlink", []string{"--id", "m1", "--target", "m2"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.UnlinkMemoriesRequest)
		if !ok {
			t.Fatalf("expected an UnlinkMemoriesRequest, got %T", req)
		}

		if in.GetId() != "m1" || len(in.GetIds()) != 1 || in.GetIds()[0] != "m2" {
			t.Errorf("unexpected request: %+v", in)
		}
	})

	t.Run("event unlink takes positional targets", func(t *testing.T) {
		req, _, err := runCommand(t, "event unlink", []string{"--id", "e1", "e2", "e3"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.UnlinkEventsRequest)
		if !ok {
			t.Fatalf("expected an UnlinkEventsRequest, got %T", req)
		}

		if len(in.GetIds()) != 2 || in.GetIds()[0] != "e2" || in.GetIds()[1] != "e3" {
			t.Errorf("expected the positional args as targets, got %+v", in.GetIds())
		}
	})

	t.Run("memory links", func(t *testing.T) {
		req, _, err := runCommand(t, "memory links", []string{"--id", "m1", "--direction", "inbound"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.GetMemoryLinksRequest)
		if !ok {
			t.Fatalf("expected a GetMemoryLinksRequest, got %T", req)
		}

		if in.GetDirection() != contract.LinkDirection_LINK_DIRECTION_INBOUND {
			t.Errorf("unexpected direction: %s", in.GetDirection())
		}
	})

	t.Run("event links defaults to both", func(t *testing.T) {
		req, _, err := runCommand(t, "event links", []string{"--id", "e1"}, &fakeClient{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		in, ok := req.(*contract.GetEventLinksRequest)
		if !ok {
			t.Fatalf("expected a GetEventLinksRequest, got %T", req)
		}

		// The graph is valued symmetrically, which is what makes BOTH the useful default.
		if in.GetDirection() != contract.LinkDirection_LINK_DIRECTION_BOTH {
			t.Errorf("expected BOTH by default, got %s", in.GetDirection())
		}
	})
}

// TestLinkCommands_Rejections covers the argument validation the six commands share, so a bad
// invocation is refused locally rather than turned into a request the service has to reject.
func TestLinkCommands_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		command string
		args    []string
		message string
	}{
		{
			name:    "link without an id",
			command: "memory link",
			args:    []string{"--link", "m2:5"},
			message: "--id is required",
		},
		{
			name:    "link without any links",
			command: "memory link",
			args:    []string{"--id", "m1"},
			message: "at least one --link",
		},
		{
			name:    "event link without an id",
			command: "event link",
			args:    []string{"--link", "e2:5"},
			message: "--id is required",
		},
		{
			name:    "unlink without an id",
			command: "memory unlink",
			args:    []string{"--target", "m2"},
			message: "--id is required",
		},
		{
			name:    "unlink without targets",
			command: "memory unlink",
			args:    []string{"--id", "m1"},
			message: "at least one memory id to unlink",
		},
		{
			name:    "event unlink without an id",
			command: "event unlink",
			args:    []string{"--target", "e2"},
			message: "--id is required",
		},
		{
			name:    "links without an id",
			command: "memory links",
			args:    []string{},
			message: "--id is required",
		},
		{
			name:    "event links without an id",
			command: "event links",
			args:    []string{},
			message: "--id is required",
		},
		{
			name:    "invalid direction",
			command: "memory links",
			args:    []string{"--id", "m1", "--direction", "sideways"},
			message: "invalid --direction",
		},
		{
			name:    "invalid event direction",
			command: "event links",
			args:    []string{"--id", "e1", "--direction", "sideways"},
			message: "invalid --direction",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := runCommand(t, test.command, test.args, &fakeClient{})
			if err == nil {
				t.Fatal("expected the invocation to be refused")
			}

			if !strings.Contains(err.Error(), test.message) {
				t.Errorf("expected the message to mention %q, got %q", test.message, err)
			}
		})
	}
}

// TestLinkCommands_RPCFailuresPropagate covers each handler's transport-error arm. They are one
// line apiece, but between them they are every way a link command can fail after its arguments were
// accepted, and a swallowed error here would report success for a link that was never written.
func TestLinkCommands_RPCFailuresPropagate(t *testing.T) {
	commands := []struct {
		name string
		args []string
	}{
		{name: "memory link", args: []string{"--id", "m1", "--link", "m2:5"}},
		{name: "event link", args: []string{"--id", "e1", "--link", "e2:5"}},
		{name: "memory unlink", args: []string{"--id", "m1", "--target", "m2"}},
		{name: "event unlink", args: []string{"--id", "e1", "--target", "e2"}},
		{name: "memory links", args: []string{"--id", "m1", "--direction", "outbound"}},
		{name: "event links", args: []string{"--id", "e1", "--direction", "outbound"}},
	}

	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			fake := &fakeClient{err: errRPC}

			if _, _, err := runCommand(t, command.name, command.args, fake); err == nil {
				t.Fatal("expected the RPC failure to reach the caller")
			}
		})
	}
}

// errRPC stands in for any transport or service failure.
var errRPC = &rpcError{}

type rpcError struct{}

func (*rpcError) Error() string { return "rpc failed" }
