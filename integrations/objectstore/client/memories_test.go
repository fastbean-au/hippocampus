package client

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// fakeHippo implements the three RPCs this integration is allowed to make.
type fakeHippo struct {
	recalled    [][]string
	recallHits  int
	recallErr   error
	explained   [][]string
	held        map[string]bool
	explainErr  error
	pages       []*contract.GetForgottenMemoriesResponse
	requests    []*contract.GetForgottenMemoriesRequest
	forgottenAt int
	forgotErr   error
}

func (f *fakeHippo) RecallMemories(
	ctx context.Context,
	in *contract.RecallMemoriesRequest,
	opts ...grpc.CallOption,
) (*contract.GetMemoriesResponse, error) {
	f.recalled = append(f.recalled, in.GetIds())

	if f.recallErr != nil {
		return nil, f.recallErr
	}

	memories := make([]*contract.Memory, 0, f.recallHits)

	for i := range f.recallHits {
		memories = append(memories, &contract.Memory{Id: fmt.Sprintf("hit-%d", i)})
	}

	return &contract.GetMemoriesResponse{Memories: memories}, nil
}

func (f *fakeHippo) ExplainConsolidation(
	ctx context.Context,
	in *contract.ExplainConsolidationRequest,
	opts ...grpc.CallOption,
) (*contract.ExplainConsolidationResponse, error) {
	f.explained = append(f.explained, in.GetMemoryIds())

	if f.explainErr != nil {
		return nil, f.explainErr
	}

	response := &contract.ExplainConsolidationResponse{}

	for _, v := range in.GetMemoryIds() {
		if !f.held[v] {
			continue
		}

		response.Valuations = append(response.Valuations, &contract.MemoryValuation{Id: v})
	}

	return response, nil
}

func (f *fakeHippo) GetForgottenMemories(
	ctx context.Context,
	in *contract.GetForgottenMemoriesRequest,
	opts ...grpc.CallOption,
) (*contract.GetForgottenMemoriesResponse, error) {
	f.requests = append(f.requests, in)

	if f.forgotErr != nil {
		return nil, f.forgotErr
	}

	if f.forgottenAt >= len(f.pages) {
		return &contract.GetForgottenMemoriesResponse{}, nil
	}

	page := f.pages[f.forgottenAt]
	f.forgottenAt++

	return page, nil
}

func TestRecallReportsTheHitRate(t *testing.T) {
	fake := &fakeHippo{recallHits: 2}
	memories := NewMemories(fake, time.Second)

	hits, err := memories.Recall(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Recall failed: %s", err.Error())
	}

	if hits != 2 {
		t.Errorf("expected two hits, got %d", hits)
	}
}

func TestRecallOfNothingCallsNothing(t *testing.T) {
	fake := &fakeHippo{}
	memories := NewMemories(fake, time.Second)

	if _, err := memories.Recall(context.Background(), nil); err != nil {
		t.Fatalf("Recall failed: %s", err.Error())
	}

	if len(fake.recalled) != 0 {
		t.Error("expected no RPC for an empty batch")
	}
}

// A group-scoped token turns a miss into NotFound for the whole batch. That is a misconfiguration
// to be logged, not a reason for the gateway in front of it to stop serving objects.
func TestRecallAbsorbsNotFound(t *testing.T) {
	fake := &fakeHippo{recallErr: status.Error(codes.NotFound, "no such memory")}
	memories := NewMemories(fake, time.Second)

	hits, err := memories.Recall(context.Background(), []string{"a"})
	if err != nil {
		t.Fatalf("expected NotFound to be absorbed: %s", err.Error())
	}

	if hits != 0 {
		t.Errorf("expected no hits, got %d", hits)
	}
}

func TestRecallReportsOtherFailures(t *testing.T) {
	fake := &fakeHippo{recallErr: status.Error(codes.Unavailable, "down")}
	memories := NewMemories(fake, time.Second)

	if _, err := memories.Recall(context.Background(), []string{"a"}); err == nil {
		t.Error("expected the failure to surface")
	}
}

func TestHeldReportsOnlyWhatTheStoreStillHas(t *testing.T) {
	fake := &fakeHippo{held: map[string]bool{"a": true}}
	memories := NewMemories(fake, time.Second)

	held, err := memories.Held(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Held failed: %s", err.Error())
	}

	if !held["a"] || held["b"] {
		t.Errorf("expected only 'a' to be held, got %v", held)
	}
}

// The service caps one call at 200 ids, so a sweep batch must be split rather than refused.
func TestHeldChunksAtTheServiceCap(t *testing.T) {
	fake := &fakeHippo{held: map[string]bool{}}
	memories := NewMemories(fake, time.Second)

	ids := make([]string, 0, explainChunkSize+1)

	for i := range explainChunkSize + 1 {
		ids = append(ids, fmt.Sprintf("id-%d", i))
	}

	if _, err := memories.Held(context.Background(), ids); err != nil {
		t.Fatalf("Held failed: %s", err.Error())
	}

	if len(fake.explained) != 2 {
		t.Fatalf("expected two calls, got %d", len(fake.explained))
	}

	if len(fake.explained[0]) != explainChunkSize || len(fake.explained[1]) != 1 {
		t.Errorf("expected chunks of %d and 1, got %d and %d",
			explainChunkSize, len(fake.explained[0]), len(fake.explained[1]))
	}
}

// A replica refuses the RPC outright, and the sweep must be able to tell that apart from a
// transport failure - and from "the store does not hold these".
func TestHeldNamesAReplica(t *testing.T) {
	fake := &fakeHippo{explainErr: status.Error(codes.FailedPrecondition, "consolidation is disabled on this instance")}
	memories := NewMemories(fake, time.Second)

	_, err := memories.Held(context.Background(), []string{"a"})
	if !errors.Is(err, ErrConsolidationDisabled) {
		t.Errorf("expected ErrConsolidationDisabled, got %v", err)
	}
}

func TestForgottenPagesTheLog(t *testing.T) {
	fake := &fakeHippo{
		pages: []*contract.GetForgottenMemoriesResponse{
			{
				Enabled:  true,
				NextSeq:  10,
				Memories: []*contract.ForgottenMemory{{Id: "a"}},
			},
			{
				Enabled:  true,
				Memories: []*contract.ForgottenMemory{{Id: "b"}},
			},
		},
	}

	memories := NewMemories(fake, time.Second)

	var seen []string

	enabled, err := memories.Forgotten(context.Background(), time.Now().Add(-time.Hour), func(page []*contract.ForgottenMemory) error {
		for _, v := range page {
			seen = append(seen, v.GetId())
		}

		return nil
	})
	if err != nil {
		t.Fatalf("Forgotten failed: %s", err.Error())
	}

	if !enabled {
		t.Error("expected the log to be reported as enabled")
	}

	if len(seen) != 2 || seen[0] != "a" || seen[1] != "b" {
		t.Errorf("expected both pages, got %v", seen)
	}

	if len(fake.requests) != 2 || fake.requests[1].GetAfterSeq() != 10 {
		t.Error("expected the second request to carry the cursor from the first")
	}
}

func TestForgottenReportsADisabledLog(t *testing.T) {
	fake := &fakeHippo{pages: []*contract.GetForgottenMemoriesResponse{{Enabled: false}}}
	memories := NewMemories(fake, time.Second)

	enabled, err := memories.Forgotten(context.Background(), time.Now(), func([]*contract.ForgottenMemory) error {
		return nil
	})
	if err != nil {
		t.Fatalf("Forgotten failed: %s", err.Error())
	}

	if enabled {
		t.Error("expected the log to be reported as not recording")
	}
}

func TestForgottenStopsOnTheCallersError(t *testing.T) {
	fake := &fakeHippo{
		pages: []*contract.GetForgottenMemoriesResponse{
			{Enabled: true, NextSeq: 10, Memories: []*contract.ForgottenMemory{{Id: "a"}}},
			{Enabled: true, Memories: []*contract.ForgottenMemory{{Id: "b"}}},
		},
	}

	memories := NewMemories(fake, time.Second)

	_, err := memories.Forgotten(context.Background(), time.Now(), func([]*contract.ForgottenMemory) error {
		return fmt.Errorf("the bucket is unreachable")
	})
	if err == nil {
		t.Fatal("expected the caller's error to stop the walk")
	}

	if len(fake.requests) != 1 {
		t.Errorf("expected the walk to stop after the first page, got %d requests", len(fake.requests))
	}
}

func TestForgottenReportsAFailure(t *testing.T) {
	fake := &fakeHippo{forgotErr: status.Error(codes.Unavailable, "down")}
	memories := NewMemories(fake, time.Second)

	if _, err := memories.Forgotten(context.Background(), time.Now(), nil); err == nil {
		t.Error("expected the failure to surface")
	}
}

// A non-positive timeout leaves each call bounded only by the caller's context, which is what a
// command passing 0 gets.
func TestTheCallTimeoutIsOptional(t *testing.T) {
	memories := NewMemories(&fakeHippo{}, 0)

	ctx, cancel := memories.callContext(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); ok {
		t.Error("expected no deadline with a zero timeout")
	}

	bounded := NewMemories(&fakeHippo{}, time.Second)

	boundedCtx, boundedCancel := bounded.callContext(context.Background())
	defer boundedCancel()

	if _, ok := boundedCtx.Deadline(); !ok {
		t.Error("expected a deadline with a timeout configured")
	}
}
