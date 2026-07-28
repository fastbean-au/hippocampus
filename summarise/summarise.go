// Package summarise provides the optional embedded-LLM summariser used to condense the memories
// of an event into a single summary. It is strictly secondary to the primary store: the service
// still owns what is deleted and inserted (via db.ReplaceMemoriesWithSummary); the summariser only
// turns a set of opaque memory bodies into the summary text a summary memory carries.
//
// Disabled by default (ollama.enabled is false): the no-op implementation makes SummariseMemories
// fail with FAILED_PRECONDITION and the sleep cycle's auto-summarisation a no-op, so the service
// behaves exactly as it did before, with the client supplying summaries via
// ReplaceMemoriesWithSummary.
package summarise

import (
	"context"
	"errors"
)

// ErrDisabled is returned by Summarise when no summariser is configured (ollama.enabled is false).
var ErrDisabled = errors.New("summarisation is not enabled (ollama.enabled is false)")

// Request carries everything the summariser needs to condense one event's memories. Bodies are the
// opaque memory bodies in the order the store returned them; EventName and Group give the model
// light context. Binary memory bodies are excluded by the caller - they are not text.
type Request struct {
	EventName string
	Group     string
	Bodies    []string
}

// Summariser condenses a set of related memory bodies into a single summary string. It is the only
// component in the service with visibility into memory content, and it is optional: the no-op
// implementation reports Enabled() false and returns ErrDisabled.
type Summariser interface {
	// Summarise returns a single summary of the request's bodies, or an error (including
	// ErrDisabled when no summariser is configured). It must respect the context's deadline.
	Summarise(ctx context.Context, req Request) (string, error)

	// Enabled reports whether a real summariser is configured; the no-op returns false.
	Enabled() bool
}

// noop is the disabled implementation: the service behaves exactly as it does without an embedded
// LLM configured.
type noop struct{}

// NewNoop returns the disabled summariser.
func NewNoop() Summariser {
	return noop{}
}

func (noop) Summarise(ctx context.Context, req Request) (string, error) {
	return "", ErrDisabled
}

func (noop) Enabled() bool {
	return false
}

// Compile-time check that noop satisfies Summariser.
var _ Summariser = noop{}
