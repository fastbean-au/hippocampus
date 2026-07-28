package hippocampus

import (
	"context"
	"errors"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/summarize"
)

// summariser returns the configured embedded-LLM summariser, or the disabled no-op when none was
// injected (as in tests constructing a Server directly), so callers never need a nil check.
func (s *Server) summariser() summarize.Summarizer {
	if s.summarizer == nil {
		return summarize.NewNoop()
	}

	return s.summarizer
}

// SummariseMemories generates a summary of an event's memories with the embedded LLM
// (ollama.enabled) and replaces them with it in one transaction, exactly as
// ReplaceMemoriesWithSummary does but with the service authoring the summary the caller would
// otherwise supply. It fails with FAILED_PRECONDITION when no summariser is configured or the
// event has no text (non-binary) memories to summarise, NOT_FOUND when the event does not exist,
// and UNAVAILABLE when the summariser call fails.
func (s *Server) SummariseMemories(ctx context.Context, in *contract.SummariseMemoriesRequest) (*contract.SummariseMemoriesResponse, error) {
	log.Trace("func() SummariseMemories")

	var res contract.SummariseMemoriesResponse

	eventId := in.GetEventId()
	if eventId == "" {
		return &res, status.Error(codes.InvalidArgument, "event_id must be provided")
	}

	if !s.summariser().Enabled() {
		return &res, status.Error(codes.FailedPrecondition, summarize.ErrDisabled.Error())
	}

	id, replaced, summary, err := s.summariseEvent(ctx, eventId, in.GetSignificance(), in.GetPlacement())
	if err != nil {
		return &res, err
	}

	res.Id = id
	res.MemoriesReplaced = int32(replaced)
	res.Summary = summary

	return &res, nil
}

// summariseEvent reads an event's memories, generates a summary of their (non-binary) bodies with
// the embedded LLM, and replaces every memory of the event with it via insertSummary. It is the
// shared body of the SummariseMemories RPC and the sleep cycle's auto-summarisation. Callers must
// have checked that a summariser is configured. significance/placement come from the RPC (both
// zero/nil on the auto path); when neither is set the summary defaults to the highest significance
// among the replaced memories, so a condensed event is at least as significant as its most
// significant constituent. The returned error is an appropriate gRPC status, ready to return.
func (s *Server) summariseEvent(ctx context.Context, eventId string, significance int32, placement *contract.SignificancePlacement) (string, int, string, error) {
	event, err := s.db.GetEvent(ctx, eventId)
	if err != nil {
		if errors.Is(err, db.ErrEventNotFound) {

			return "", 0, "", status.Errorf(codes.NotFound, "event '%s' not found", eventId)
		}

		return "", 0, "", mapError(err)
	}

	memories, err := s.db.GetMemoriesByEventId(ctx, eventId)
	if err != nil {
		return "", 0, "", mapError(err)
	}

	// Collect the text bodies for the prompt and the highest significance among all the memories
	// being replaced (binary ones count toward significance but cannot be summarised).
	bodies := make([]string, 0, len(*memories))

	var maxSig int32

	for i := range *memories {
		m := (*memories)[i]

		if m.Significance > maxSig {
			maxSig = m.Significance
		}

		if m.IsBinary {
			continue
		}

		bodies = append(bodies, m.Body)
	}

	if len(bodies) == 0 {
		return "", 0, "", status.Errorf(codes.FailedPrecondition, "event '%s' has no text memories to summarise", eventId)
	}

	ctx, span := tel.tracer.Start(ctx, "summarise_event", trace.WithAttributes(
		attribute.Int("memories", len(*memories)),
		attribute.Int("bodies", len(bodies)),
	))
	defer span.End()

	summary, err := s.summariser().Summarize(ctx, summarize.Request{
		EventName: event.Name,
		Group:     event.Group,
		Bodies:    bodies,
	})
	if err != nil {
		tel.summarisations.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", false)))
		span.RecordError(err)
		span.SetStatus(otelcodes.Error, err.Error())
		log.Warnf("summarisation of event '%s' failed: %s", eventId, err.Error())

		return "", 0, "", status.Errorf(codes.Unavailable, "summarisation failed: %v", err)
	}

	tel.summarisations.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", true)))

	gen := &contract.Memory{
		Body:         summary,
		EventId:      eventId,
		Group:        event.Group,
		Significance: significance,
		Placement:    placement,
	}

	// With no explicit ranking, default the summary to the most significant memory it replaces.
	if significance == 0 && !hasPlacement(placement) {
		gen.Significance = maxSig
	}

	id, replaced, err := s.insertSummary(ctx, eventId, gen)
	if err != nil {
		return "", 0, "", err
	}

	return id, replaced, summary, nil
}

// autoSummarizeCandidates summarises the candidates the most recent scan identified, using the
// embedded LLM, when ollama.autoSummarize is on. It runs inside the sleep cycle after
// scanSummarizationCandidates, so it condenses exactly the events the scan surfaced. It is
// best-effort: a disabled summariser, a disabled auto flag, or an empty candidate list makes it a
// no-op, and a per-event failure (unreachable model, an event that changed since the scan) is
// logged and skipped without failing the sleep cycle - matching the best-effort treatment of the
// scan itself. Each summarised event is removed from the cached candidate list so
// GetSummarizationCandidates does not keep offering an event already condensed.
func (s *Server) autoSummarizeCandidates(ctx context.Context) {
	if !s.consolidation.autoSummarize || !s.summariser().Enabled() {
		return
	}

	s.summarizationCandidatesMu.RLock()
	candidates := make([]db.SummarizationCandidate, len(s.summarizationCandidates))
	copy(candidates, s.summarizationCandidates)
	s.summarizationCandidatesMu.RUnlock()

	if len(candidates) == 0 {
		return
	}

	ctx, span := tel.tracer.Start(ctx, "auto_summarize_candidates", trace.WithAttributes(
		attribute.Int("candidates", len(candidates)),
	))
	defer span.End()

	summarised := 0

	summarisedIds := make(map[string]bool, len(candidates))

	for i := range candidates {
		eventId := candidates[i].EventId

		if _, replaced, _, err := s.summariseEvent(ctx, eventId, 0, nil); err != nil {
			log.Warnf("auto-summarisation of event '%s' skipped: %s", eventId, err.Error())

			continue
		} else {
			log.Infof("auto-summarised event '%s' (%d memories replaced)", eventId, replaced)
		}

		summarisedIds[eventId] = true

		summarised++
	}

	// Drop the events we condensed from the cached list so GetSummarizationCandidates stops
	// offering an event that no longer has unsummarised memories to condense.
	if summarised > 0 {
		s.summarizationCandidatesMu.Lock()

		kept := s.summarizationCandidates[:0]

		for i := range s.summarizationCandidates {
			if summarisedIds[s.summarizationCandidates[i].EventId] {
				continue
			}

			kept = append(kept, s.summarizationCandidates[i])
		}

		s.summarizationCandidates = kept
		s.summarizationCandidatesMu.Unlock()
	}

	span.AddEvent("events_auto_summarised", trace.WithAttributes(attribute.Int("summarised", summarised)))
	log.Infof("auto-summarised %d of %d candidate events", summarised, len(candidates))
}
