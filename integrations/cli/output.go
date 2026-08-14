package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
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

		// Rendered from group_scoped, not from the list being empty: unscoped and scoped-to-nothing
		// are opposites and the list alone cannot tell them apart. Spelling out "unscoped" matters
		// here more than in the console - an operator reading an unexpectedly short list wants to
		// know whether their token is the reason.
		if m.GetGroupScoped() {
			r.line("groups:       %s", strings.Join(m.GetGroups(), ", "))
		} else {
			r.line("groups:       unscoped (whole store)")
		}

		// Everything above describes the caller; everything below describes the instance answering,
		// and is the same for every caller of it. Worth a line of its own rather than JSON-only:
		// which of two addresses is the consolidator, and whether this one records what it forgets,
		// are questions an operator asks of a running deployment, and asking them by watching an RPC
		// be refused is the thing whoami exists to avoid.
		r.line("consolidating: %t", m.GetConsolidationEnabled())
		r.line("forgotten log: %t", m.GetTombstonesEnabled())
		r.line("summariser:    %t", m.GetSummariserEnabled())
		r.line("search modes:  %s", orNone(strings.Join(searchModeNames(m.GetSearchModes()), ", ")))

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

	case *contract.GetLinksResponse:
		r.line("%d link(s), %d total link significance", len(m.GetLinks()), m.GetLinkSignificance())

		for _, link := range m.GetLinks() {
			r.line("  %s  %-8s significance %d  created %s",
				link.GetId(),
				linkDirectionLabel(link.GetDirection()),
				link.GetSignificance(),
				formatNanos(link.GetCreated()),
			)
		}

	case *contract.ReplaceMemoriesWithSummaryResponse:
		r.line("summary memory %s replaced %d memory/memories", m.GetId(), m.GetMemoriesReplaced())

	case *contract.SummariseMemoriesResponse:
		r.line("summary memory %s replaced %d memory/memories", m.GetId(), m.GetMemoriesReplaced())
		r.line("summary: %s", m.GetSummary())

	case *contract.GetSummarisationCandidatesResponse:
		r.line("%d candidate(s)", len(m.GetCandidates()))

		// An empty list means one of two opposite things, so say which. Only worth a line when the
		// list is empty: with candidates in hand the scan is evidently running.
		if len(m.GetCandidates()) == 0 && !m.GetScanEnabled() {
			r.line("  (the candidate scan is not running on this instance — it needs")
			r.line("   consolidation.summarisationMinMemories > 0 and consolidation.enabled)")
		}

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

	case *contract.PreviewConsolidationResponse:
		r.renderPreview(m)

	case *contract.ExplainConsolidationResponse:
		r.renderExplanation(m)

	case *contract.GetForgottenMemoriesResponse:
		r.renderForgotten(m)

	case *contract.DeleteForgottenMemoriesResponse:
		r.line("deleted: %d", m.GetDeleted())

	default:
		// No bespoke text form: fall back to protojson so nothing is ever silently dropped.
		return r.writeJSON(msg)
	}

	return nil
}

// renderPreview writes a dry run in the order an operator reads it: what would go, what is holding
// data back, then which memories - because the counts are the answer and the sample is the
// evidence.
func (r *renderer) renderPreview(preview *contract.PreviewConsolidationResponse) {
	r.line("would forget %d memory/memories and %d event(s), freeing ~%d bytes",
		preview.GetMemoriesConsolidated()+preview.GetMemoriesEvicted(),
		preview.GetEventsDeleted(),
		preview.GetBytesFreed(),
	)

	r.line("  consolidated (decayed below the threshold): %d", preview.GetMemoriesConsolidated())
	r.line("  evicted (over the byte capacity):           %d", preview.GetMemoriesEvicted())

	// Flagged rather than merely reported: retention overrides the capacity target, so a retained
	// set approaching the capacity is why a store can sit above its target indefinitely.
	if preview.GetMemoriesRetained() > 0 {
		r.line("  retained by the minimum retention floor:    %d (%d bytes)",
			preview.GetMemoriesRetained(),
			preview.GetRetainedBytes(),
		)
	}

	r.line("")
	r.line("capacity pressure:  %.3f", preview.GetCapacityPressure())
	// Six significant figures, not %g: the threshold is a product of two floats, so the full
	// precision shows arithmetic noise (10.000000000000004) that reads as a real number.
	r.line("deletion threshold: %.6g (scaled by the pressure above)", preview.GetDeletionThreshold())

	if preview.GetCapacityBytes() > 0 {
		r.line("used / capacity:    %d / %d bytes", preview.GetUsedBytes(), preview.GetCapacityBytes())
	} else {
		r.line("used:               %d bytes (no byte capacity configured, so nothing is evicted)", preview.GetUsedBytes())
	}

	if len(preview.GetCandidates()) == 0 {
		return
	}

	r.line("")
	r.line("%d shown, least valuable first%s:", len(preview.GetCandidates()), truncatedNote(preview.GetTruncated()))

	for _, candidate := range preview.GetCandidates() {
		r.line("  %-24s  value %-11.4g  significance %-5d  %-8s  %s",
			candidate.GetId(),
			candidate.GetValue(),
			candidate.GetSignificance(),
			forgetRuleLabel(candidate.GetRule()),
			orNone(candidate.GetGroup()),
		)
	}
}

// renderForgotten writes the forgotten log, newest first.
//
// The empty case is spelled out rather than left blank, because an empty log is ambiguous: nothing
// has been forgotten, or nothing is being written down. The response's enabled flag is the only
// thing that tells the two apart, and an operator reading a blank table would assume the first.
func (r *renderer) renderForgotten(forgotten *contract.GetForgottenMemoriesResponse) {
	if !forgotten.GetEnabled() {
		r.line("the forgotten log is not enabled (consolidation.tombstones.enabled)")

		if len(forgotten.GetMemories()) == 0 {
			return
		}

		r.line("showing %d record(s) written while it was", len(forgotten.GetMemories()))
	}

	if len(forgotten.GetMemories()) == 0 {
		r.line("nothing has been forgotten")

		return
	}

	r.line("%d of %d record(s), most recent first:", len(forgotten.GetMemories()), forgotten.GetTotal())

	for _, memory := range forgotten.GetMemories() {
		r.line("  %-24s  %-20s  value %-11.4g  significance %-5d  %-8s  %s",
			memory.GetId(),
			time.Unix(0, memory.GetForgottenAt()).Format(time.RFC3339),
			memory.GetValue(),
			memory.GetSignificance(),
			forgetRuleLabel(memory.GetRule()),
			orNone(memory.GetGroup()),
		)
	}

	if forgotten.GetNextSeq() > 0 {
		r.line("")
		r.line("more records: --after-seq %d", forgotten.GetNextSeq())
	}
}

// renderExplanation writes where each memory stands, then the configuration deciding it - the
// per-memory lines first because they are what was asked about, with the inputs underneath as the
// context for reading them.
func (r *renderer) renderExplanation(explanation *contract.ExplainConsolidationResponse) {
	for _, valuation := range explanation.GetValuations() {
		r.line("memory %s", valuation.GetId())
		r.line("  value / threshold:  %.6g / %.6g%s",
			valuation.GetValue(),
			valuation.GetThreshold(),
			valuationNote(valuation),
		)
		r.line("  significance:       %d stored, %.6g effective", valuation.GetSignificance(), valuation.GetEffectiveSignificance())
		r.line("  age:                %.2f days, %d recall(s)", valuation.GetAgeDays(), valuation.GetRecallCount())
		r.line("  forgotten in:       %s", forgottenIn(valuation.GetDaysUntilForgotten()))
	}

	if len(explanation.GetValuations()) > 0 {
		r.line("")
	}

	r.line("method %d, aggressiveness %.6g, %.6g day(s) per age unit",
		explanation.GetMethod(),
		explanation.GetAggressiveness(),
		explanation.GetUnitsOfAgeInDays(),
	)

	r.line("capacity pressure:  %.3f", explanation.GetCapacityPressure())
	r.line("deletion threshold: %.6g (scaled by the pressure above)", explanation.GetDeletionThreshold())

	if explanation.GetCapacityBytes() > 0 {
		r.line("used / capacity:    %d / %d bytes", explanation.GetUsedBytes(), explanation.GetCapacityBytes())
	} else {
		r.line("used:               %d bytes (no byte capacity configured)", explanation.GetUsedBytes())
	}

	curve := explanation.GetCurve()
	if curve == nil {
		return
	}

	r.line("")
	r.line("decay of significance %.6g over %.6g day(s), %d point(s):",
		curve.GetSignificance(),
		curve.GetMaxAgeDays(),
		len(curve.GetPoints()),
	)

	if crossing := curve.GetCrossingAgeDays(); crossing >= 0 {
		r.line("  crosses the threshold at %.2f day(s)", crossing)
	} else {
		r.line("  does not cross the threshold within the projected span")
	}

	for _, point := range curve.GetPoints() {
		r.line("  %10.3f  %.6g", point.GetAgeDays(), point.GetValue())
	}
}

// valuationNote names whichever rule is overriding the value comparison, since a memory well below
// the threshold that is nonetheless safe (retained, or not yet old enough) otherwise reads as a
// contradiction.
func valuationNote(valuation *contract.MemoryValuation) string {
	switch {

	case valuation.GetRetained():
		return "  (retained: inside the minimum retention window)"

	case valuation.GetBelowMinimumAge():
		return "  (below the minimum age, so consolidation is deferred)"

	case valuation.GetWouldConsolidate():
		return "  (a cycle running now would forget it)"

	default:
		return ""

	}
}

// forgottenIn renders the projection, keeping the two sentinels readable: 0 means already due, and
// -1 means no crossing within any span worth reporting.
func forgottenIn(days float64) string {
	switch {

	case days < 0:
		return "not within any projected timeframe"

	case days == 0:
		return "due now"

	default:
		return fmt.Sprintf("~%.2f day(s)", days)

	}
}

// truncatedNote explains a short sample, so a truncated list is never mistaken for the whole of
// what would be forgotten.
func truncatedNote(truncated bool) string {
	if truncated {
		return " (truncated - raise --limit to see more)"
	}

	return ""
}

// forgetRuleLabel renders the rule compactly for the per-memory lines.
func forgetRuleLabel(rule contract.ForgetRule) string {
	switch rule {

	case contract.ForgetRule_FORGET_RULE_CONSOLIDATION:
		return "decayed"

	case contract.ForgetRule_FORGET_RULE_EVICTION:
		return "capacity"

	default:
		return "unknown"
	}
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

	// Only printed when the server actually counted: the field is 0 both for an event holding
	// nothing and for a request that never asked, and printing "0" for the latter would be a claim
	// the response did not make.
	if count := event.GetMemoryCount(); count > 0 {
		r.line("  memories:     %d", count)
	}

	r.renderMetadata(event.GetMetadata())

	r.renderLinks(event.GetLinks(), event.GetLinkSignificance())

	for _, memory := range event.GetMemories() {
		r.line("")
		r.renderMemory(memory)
	}
}

// renderMetadata prints a memory's or event's labels, one per line, with the keys SORTED.
//
// The sort is not cosmetic: Go randomises map iteration, so without it the same memory would render
// differently between runs - unreadable to diff, and untestable against a fixture.
func (r *renderer) renderMetadata(metadata map[string]string) {
	if len(metadata) == 0 {
		return
	}

	keys := make([]string, 0, len(metadata))

	for k := range metadata {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		r.line("  metadata:     %s=%s", k, metadata[k])
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

	r.renderMetadata(memory.GetMetadata())

	if memory.GetRecallCount() > 0 {
		r.line("  recall_count: %d (last %s)", memory.GetRecallCount(), formatNanos(memory.GetTimeRecalled()))
	}

	r.renderLinks(memory.GetLinks(), memory.GetLinkSignificance())

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

// searchModeNames renders the modes whoami reported in the vocabulary --mode accepts, rather than
// as the proto enum names. Inverted from the searchModes flag map for exactly that reason: what
// this prints is what can be typed straight back into `hippo memory search --mode`.
func searchModeNames(modes []contract.SearchMode) []string {
	out := make([]string, 0, len(modes))

	for _, mode := range modes {
		for name, v := range searchModes {
			if name == "" || v != mode {
				continue
			}

			out = append(out, name)

			break
		}
	}

	sort.Strings(out)

	return out
}

func orNone(value string) string {
	if value == "" {
		return "(none)"
	}

	return value
}

// renderLinks prints an item's links, shared by the memory and event renderers. Both the summed
// significance and the individual links are shown only when there is something to show, so an
// unlinked item reads exactly as it did before links existed.
func (r *renderer) renderLinks(links []*contract.Link, significance int64) {
	if significance > 0 {
		r.line("  link_sig:     %d", significance)
	}

	for _, link := range links {
		r.line("  link:         %s (significance %d)", link.GetId(), link.GetSignificance())
	}
}

// linkDirectionLabel names a link's direction relative to the item that was asked about.
func linkDirectionLabel(d contract.LinkDirection) string {
	switch d {

	case contract.LinkDirection_LINK_DIRECTION_OUTBOUND:
		return "outbound"

	case contract.LinkDirection_LINK_DIRECTION_INBOUND:
		return "inbound"

	default:
		return "both"

	}
}
