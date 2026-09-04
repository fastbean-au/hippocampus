// Package notify provides the optional outbound callback sink: the component that tells an
// external system that Hippocampus has forgotten something, or that a sleep cycle has finished.
//
// Everything the service has built for forgetting transparency so far is PULL - PreviewConsolidation
// asks what would go, ExplainConsolidation where a memory stands, GetConsolidationStatus when the
// next cycle is due, and GetForgottenMemories what went. Each of them requires a client to be
// asking. A deployment that uses Hippocampus as the forgetting front-half of a larger system - an
// archive tier, a cache that must be invalidated, an index somewhere else - has no way to learn
// that a record has gone except by asking for it and finding nothing. This is the push half.
//
// The package is deliberately only the SINK. It holds no queue, knows nothing about the store, and
// is handed fully-rendered deliveries by the drain worker; durability is the persisted queue's job
// (db/callbacks.go), because a delivery about a deletion has the same property a search-index
// delete has - losing it is permanent, since nothing afterwards knows the record should have gone.
// Keeping the two apart is what lets a second transport (gRPC, a broker) be added later without the
// queue learning about it.
//
// Disabled by default (callbacks.enabled is false): the no-op implementation reports Enabled()
// false, which gates the recording and the draining together, so a store with callbacks off queues
// nothing at all.
package notify

import (
	"context"
	"errors"
)

// ErrDisabled is returned by Deliver when no sink is configured (callbacks.enabled is false).
var ErrDisabled = errors.New("callbacks are not enabled (callbacks.enabled is false)")

// Kind names what a delivery is about. It is a small closed set: it is carried on the wire, stored
// in the queue, and used as a metric attribute, so it must stay low-cardinality.
type Kind string

const (
	// KindMemoryForgotten reports memories that have been deleted.
	KindMemoryForgotten Kind = "memory_forgotten"

	// KindEventForgotten reports events that have been deleted.
	KindEventForgotten Kind = "event_forgotten"

	// KindSleepCompleted reports that a sleep cycle has finished, with what it forgot.
	KindSleepCompleted Kind = "sleep_completed"
)

// Cause names why records were deleted. Unlike Kind this is not a decay judgement - it separates
// the two decay paths from the client-initiated and administrative ones, which is what
// callbacks.allDeletions widens the feed to include.
type Cause string

const (
	// CauseConsolidation is the value-based sleep-cycle pass.
	CauseConsolidation Cause = "consolidation"

	// CauseEviction is the capacity-pressure pass.
	CauseEviction Cause = "eviction"

	// CauseClient is an explicit DeleteMemories/DeleteEvent from a caller.
	CauseClient Cause = "client"

	// CauseClear is the second half of an Export/Transfer move.
	CauseClear Cause = "clear"

	// CauseCascade is a memory going because its event did, or an event going because its last
	// memory did.
	CauseCascade Cause = "cascade"

	// CauseSummaryReplace is memories replaced by a summary memory.
	CauseSummaryReplace Cause = "summary_replace"

	// CausePurge is Purge - everything, at an operator's explicit request.
	CausePurge Cause = "purge"
)

// Item is one record a delivery is about.
//
// Body is present only when callbacks.includeBodies is set AND the body fitted under
// callbacks.maxBodyBytes. An oversized body is reported as BodyOmitted with Body empty rather than
// truncated: a truncated body is worse than none, because a receiver cannot tell it is looking at
// part of one.
type Item struct {
	Id           string `json:"id"`
	EventId      string `json:"event_id,omitempty"`
	Group        string `json:"group,omitempty"`
	Significance int32  `json:"significance,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Body         string `json:"body,omitempty"`
	BodyOmitted  bool   `json:"body_omitted,omitempty"`
}

// Cycle is the sleep-cycle summary carried by a KindSleepCompleted delivery. It repeats the counts
// GetConsolidationStatus already reports, so a receiver need not correlate to know what the ids it
// is holding amount to.
type Cycle struct {
	Trigger                 string `json:"trigger"`
	StartedAt               int64  `json:"started_at"`
	DurationMillis          int64  `json:"duration_millis"`
	MemoriesConsolidated    int    `json:"memories_consolidated"`
	EventsConsolidated      int    `json:"events_consolidated"`
	MemoriesEvicted         int    `json:"memories_evicted"`
	EventsEvicted           int    `json:"events_evicted"`
	BytesFreed              int64  `json:"bytes_freed"`
	SummarisationCandidates int    `json:"summarisation_candidates"`
	Success                 bool   `json:"success"`
	Failure                 string `json:"failure,omitempty"`
}

// Delivery is one callback: the unit the queue stores and the sink posts.
//
// A delivery is a BATCH. One consolidation chunk of five hundred deletions is one delivery, not five
// hundred - which is what makes the write cost affordable on the delete path, and is also the
// mechanism the chunked sleep payload needs. Chunk and Chunks describe that split: a cycle whose id
// list exceeds callbacks.maxIdsPerDelivery becomes several deliveries sharing one CycleId, numbered
// from 1, so a receiver can tell a partial view from a complete one.
type Delivery struct {
	Kind  Kind  `json:"kind"`
	Cause Cause `json:"cause,omitempty"`

	// QueuedAt is when the delivery was recorded, in UnixNano. It is the event's time, not the
	// delivery attempt's: a delivery that spent an hour backed off still reports when the memories
	// actually went.
	QueuedAt int64 `json:"queued_at"`

	// CycleId groups every delivery a single sleep cycle produced, including the per-batch
	// forgotten deliveries, so a receiver can assemble a cycle from them. Zero outside a cycle.
	CycleId int64 `json:"cycle_id,omitempty"`

	Chunk  int `json:"chunk,omitempty"`
	Chunks int `json:"chunks,omitempty"`

	Items []Item `json:"items,omitempty"`
	Cycle *Cycle `json:"cycle,omitempty"`
}

// Notifier delivers a callback to whatever is configured to receive one. It is optional: the no-op
// implementation reports Enabled() false and returns ErrDisabled.
//
// An implementation must respect the context's deadline, and must return an error for anything the
// receiver did not accept - the drain worker treats a returned error as "retry later", so reporting
// success for a rejected delivery loses it silently.
type Notifier interface {
	// Deliver sends one delivery, returning nil only when the receiver accepted it.
	Deliver(ctx context.Context, delivery Delivery) error

	// Enabled reports whether a real sink is configured; the no-op returns false. It gates the
	// queue's recording as well as its draining, so a store never queues what nothing will drain.
	Enabled() bool
}

// noop is the disabled implementation: the service behaves exactly as it does with no callback
// endpoint configured, queueing nothing and draining nothing.
type noop struct{}

// NewNoop returns the disabled notifier.
func NewNoop() Notifier {
	return noop{}
}

func (noop) Deliver(ctx context.Context, delivery Delivery) error {
	return ErrDisabled
}

func (noop) Enabled() bool {
	return false
}

// Compile-time check that noop satisfies Notifier.
var _ Notifier = noop{}
