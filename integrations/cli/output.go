package main

import (
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// renderer writes RPC responses to the CLI's output stream in either a human-readable text form or
// as protojson. The json form is the stable, machine-parseable output for scripting; text is the
// default for interactive use.
type renderer struct {
	out  io.Writer
	json bool
}

// render writes msg to the output stream. In json mode it emits indented protojson (the wire field
// names), which is what a script should parse; otherwise it dispatches to a compact text rendering
// per response type, falling back to protojson for any type without a bespoke renderer.
func (r *renderer) render(msg proto.Message) error {
	if r.json {
		return r.writeJSON(msg)
	}

	return r.renderText(msg)
}

// writeJSON emits msg as indented protojson followed by a newline. It is the single JSON-encoding
// site, shared by the --output json path and renderText's fallback for types without a bespoke text
// form.
func (r *renderer) writeJSON(msg proto.Message) error {
	data, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to encode response: %w", err)
	}

	_, err = fmt.Fprintln(r.out, string(data))

	return err
}

// renderText writes a compact human-readable form of the common response types.
func (r *renderer) renderText(msg proto.Message) error {
	switch m := msg.(type) {

	case *contract.GeneralResponse:
		r.line("ok: %t", m.GetOk())

	case *contract.WhoAmIResponse:
		r.line("client_id:    %s", orNone(m.GetClientId()))
		r.line("role:         %s", m.GetRole())
		r.line("auth_enabled: %t", m.GetAuthEnabled())

	case *contract.StoreMemoryResponse:
		if m.GetRejected() {
			r.line("rejected (significance below the minimum)")

			break
		}

		r.line("stored memory %s", m.GetId())

	case *contract.StoreEventResponse:
		if m.GetRejected() {
			r.line("rejected (significance below the minimum)")

			break
		}

		r.line("stored event %s (%d nested memories retained)", m.GetId(), m.GetMemoryCount())

	case *contract.GetEventResponse:
		r.renderEvent(m.GetEvent())

	case *contract.GetEventsResponse:
		r.line("%d event(s) (of %d matching)", len(m.GetEvents()), m.GetTotalCount())

		for _, event := range m.GetEvents() {
			r.line("")
			r.renderEvent(event)
		}

	case *contract.GetMemoriesResponse:
		r.line("%d memory/memories (of %d matching)", len(m.GetMemories()), m.GetTotalCount())

		for _, memory := range m.GetMemories() {
			r.line("")
			r.renderMemory(memory)
		}

	case *contract.ReplaceMemoriesWithSummaryResponse:
		r.line("summary memory %s replaced %d memory/memories", m.GetId(), m.GetMemoriesReplaced())

	case *contract.SummariseMemoriesResponse:
		r.line("summary memory %s replaced %d memory/memories", m.GetId(), m.GetMemoriesReplaced())
		r.line("summary: %s", m.GetSummary())

	case *contract.GetSummarisationCandidatesResponse:
		r.line("%d candidate(s)", len(m.GetCandidates()))

		for _, candidate := range m.GetCandidates() {
			r.line("  %s  %-30s  %d memories", candidate.GetEventId(), candidate.GetEventName(), candidate.GetMemoryCount())
		}

	case *contract.ExportResponse:
		r.line("manifest_id:       %s", m.GetManifestId())
		r.line("object_key:        %s", m.GetObjectKey())
		r.line("events_exported:   %d", m.GetEventsExported())
		r.line("memories_exported: %d", m.GetMemoriesExported())
		r.line("events_cleared:    %d", m.GetEventsCleared())
		r.line("memories_cleared:  %d", m.GetMemoriesCleared())

	case *contract.ImportResponse:
		r.line("events_imported:   %d", m.GetEventsImported())
		r.line("memories_imported: %d", m.GetMemoriesImported())

	case *contract.ImportBatchResponse:
		r.line("events_imported:   %d", m.GetEventsImported())
		r.line("memories_imported: %d", m.GetMemoriesImported())

	case *contract.TransferResponse:
		r.line("manifest_id:          %s", m.GetManifestId())
		r.line("events_transferred:   %d", m.GetEventsTransferred())
		r.line("memories_transferred: %d", m.GetMemoriesTransferred())
		r.line("events_cleared:       %d", m.GetEventsCleared())
		r.line("memories_cleared:     %d", m.GetMemoriesCleared())

	case *contract.ClearResponse:
		r.line("events_cleared:   %d", m.GetEventsCleared())
		r.line("memories_cleared: %d", m.GetMemoriesCleared())

	default:
		// No bespoke text form: fall back to protojson so nothing is ever silently dropped.
		return r.writeJSON(msg)
	}

	return nil
}

func (r *renderer) renderEvent(event *contract.Event) {
	if event == nil {
		r.line("(no event)")

		return
	}

	r.line("event %s", event.GetId())
	r.line("  name:         %s", event.GetName())

	if desc := event.GetDescription(); desc != "" {
		r.line("  description:  %s", desc)
	}

	r.line("  significance: %d", event.GetSignificance())
	r.line("  time_start:   %s", formatNanos(event.GetTimeStart()))

	if event.GetTimeEnd() != 0 {
		r.line("  time_end:     %s", formatNanos(event.GetTimeEnd()))
	}

	if group := event.GetGroup(); group != "" {
		r.line("  group:        %s", group)
	}

	for _, memory := range event.GetMemories() {
		r.line("")
		r.renderMemory(memory)
	}
}

func (r *renderer) renderMemory(memory *contract.Memory) {
	if memory == nil {
		r.line("(no memory)")

		return
	}

	r.line("memory %s", memory.GetId())
	r.line("  significance: %d", memory.GetSignificance())
	r.line("  time_stamp:   %s", formatNanos(memory.GetTimeStamp()))

	if eventID := memory.GetEventId(); eventID != "" {
		r.line("  event_id:     %s", eventID)
	}

	if group := memory.GetGroup(); group != "" {
		r.line("  group:        %s", group)
	}

	if memory.GetRecallCount() > 0 {
		r.line("  recall_count: %d (last %s)", memory.GetRecallCount(), formatNanos(memory.GetTimeRecalled()))
	}

	if memory.GetIsBinary() == contract.Bool_TRUE {
		r.line("  body:         <binary, %d bytes>", len(memory.GetBody()))

		return
	}

	r.line("  body:         %s", memory.GetBody())
}

func (r *renderer) line(format string, args ...any) {
	_, _ = fmt.Fprintf(r.out, format+"\n", args...)
}

// formatNanos renders a UnixNano timestamp as RFC3339; 0 (unset) becomes a dash.
func formatNanos(nanos int64) string {
	if nanos == 0 {
		return "-"
	}

	return time.Unix(0, nanos).Format(time.RFC3339)
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}
