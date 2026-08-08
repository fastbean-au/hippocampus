package db

import (
	"context"
	"database/sql"
	"sort"

	log "github.com/sirupsen/logrus"
)

// previewDefaultLimit and previewMaxLimit bound the sample of individual memories a preview
// returns. Every count in ConsolidationPreview is complete regardless of the limit, so truncating
// the sample never understates how much would be forgotten - it only stops a preview of a large
// store from returning the store.
const (
	previewDefaultLimit = 100
	previewMaxLimit     = 1000
)

// PreviewLimit normalises a requested sample size: a non-positive value selects the default, and
// anything above the cap is clamped down to it.
//
// It is exported because callers need to agree with PreviewConsolidation on what a given request
// resolves to - the RPC layer keys its singleflight on the normalised value, so a request for the
// default and an explicit request for the same number share one scan instead of running two that
// would return the same thing.
func PreviewLimit(requested int) int {
	if requested <= 0 {
		return previewDefaultLimit
	}

	if requested > previewMaxLimit {
		return previewMaxLimit
	}

	return requested
}

// ForgetRule names which of the two independent deletion paths selected a memory.
type ForgetRule int

const (
	// ForgetRuleConsolidation is the value-based path: the memory's decayed value fell below the
	// capacity-pressure-scaled deletion threshold.
	ForgetRuleConsolidation ForgetRule = iota + 1

	// ForgetRuleEviction is the capacity path: the memory was still above the threshold and went
	// only to bring the store back under its byte capacity.
	ForgetRuleEviction
)

// ForgetCandidate is one memory a consolidation cycle would delete, with the numbers behind the
// decision. Body content is deliberately absent: a preview reports what would be lost, and must
// not become a way to read the store.
type ForgetCandidate struct {
	Id           string
	EventId      string
	Group        string
	Significance int32
	Value        float64
	Bytes        int64
	Rule         ForgetRule
	TimeStamp    int64
	TimeRecalled int64
	RecallCount  int32
}

// ConsolidationPreview is what a cycle would do to the store. The counts and byte figures are
// complete; Candidates is a bounded sample ordered by Value ascending.
type ConsolidationPreview struct {
	MemoriesConsolidated int
	MemoriesEvicted      int
	EventsDeleted        int
	BytesFreed           int64
	MemoriesRetained     int
	RetainedBytes        int64
	Candidates           []ForgetCandidate
	Truncated            bool
}

// PreviewOptions carries the decision inputs the server owns, so the preview evaluates against
// exactly the configuration and capacity reading the next real cycle would use rather than
// re-deriving them here.
//
// UsedBytes is the store's current usage; eviction is previewed only when what remains after
// consolidation still exceeds CapacityBytes, and then reclaims down to EvictionFloor - mirroring
// Server.evict. A non-positive CapacityBytes disables the eviction half entirely.
type PreviewOptions struct {
	Limit         int
	UsedBytes     int64
	CapacityBytes int64
	EvictionFloor int64
}

// previewRow is one scanned memory, carrying both the decision inputs and the reporting detail.
type previewRow struct {
	candidate MemoryConsolidationCandidate
	id        string
	eventId   string
	group     string
	bytes     int64
	value     float64
}

// PreviewConsolidation reports what a consolidation cycle would forget if one ran now, without
// deleting anything.
//
// It scans once and makes every decision through the same Server methods the real passes use
// (ShouldConsolidateMemory, MemoryRetained, MemoryValue, ShouldConsolidateEvent), so the preview
// and the cycle cannot disagree about an individual memory. What it reimplements rather than
// shares is the per-event bookkeeping, which the real passes interleave with their deletes;
// TestPreviewMatchesASleepCycle pins the two together against a real store.
//
// Two departures from the real scans, both deliberate:
//
//   - It reads group_name and length(body), so unlike the consolidation passes it does not stay on
//     the covering index. A preview is operator-initiated and occasional, and the detail is the
//     point of it; the real cycle's index-only scan is untouched.
//   - It applies the cycle's own ordering. Consolidation runs first, so memories it would delete
//     are excluded from the eviction pool and their bytes are treated as already reclaimed before
//     eviction is considered. Previewing the two independently would double-count a memory both
//     paths could claim and overstate what eviction has left to do.
func (d *DB) PreviewConsolidation(ctx context.Context, s Server, opts PreviewOptions) (ConsolidationPreview, error) {
	log.Trace("func() db.PreviewConsolidation")

	var preview ConsolidationPreview

	limit := PreviewLimit(opts.Limit)

	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		log.Errorf("failed to load significance registry: %s", err.Error())

		return preview, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// The projection mirrors EvictMemories' - the widest of the real scans - plus group_name, and
	// with the same COALESCE defaults so a memory whose event no longer exists is scored as
	// event-less rather than being silently dropped by an INNER JOIN. e.id distinguishes a real
	// event from such a dangling reference.
	rows, err := d.query(
		ctx,
		`SELECT m.id, m.timestamp, m.significance_level_id, m.time_recalled, m.recall_count, m.event_id,
			m.group_name, e.significance_level_id, COALESCE(e.link_significance, 0),
			m.link_significance, e.id,
			length(m.body) + `+d.metadataBytesExpr("m.")+`
		FROM memories m LEFT JOIN events e ON e.id = m.event_id`,
	)
	if err != nil {
		log.Errorf("failed to preview consolidation: %s", err.Error())

		return preview, err
	}
	defer func() { _ = rows.Close() }()

	var consolidating []previewRow
	var evictable []previewRow

	// memoriesPerEvent counts every memory an event holds, including retained ones: an event is
	// deleted only when it loses all of them, so a memory that survives must keep its event alive.
	memoriesPerEvent := make(map[string]int)
	deletionsPerEvent := make(map[string]int)

	for rows.Next() {
		var row previewRow
		var joinedEventId sql.NullString
		var memoryLevelID, eventLevelID sql.NullInt64
		var bodyBytes sql.NullInt64

		if err := rows.Scan(
			&row.id,
			&row.candidate.Timestamp,
			&memoryLevelID,
			&row.candidate.TimeRecalled,
			&row.candidate.RecallCount,
			&row.eventId,
			&row.group,
			&eventLevelID,
			&row.candidate.EventLinkSignificance,
			&row.candidate.MemoryLinkSignificance,
			&joinedEventId,
			&bodyBytes,
		); err != nil {
			log.Errorf("failed to scan memory for preview: %s", err.Error())

			return preview, err
		}

		row.candidate.MemorySignificance = rankOf(ranks, memoryLevelID)
		row.candidate.EventSignificance = rankOf(ranks, eventLevelID)
		row.bytes = bodyBytes.Int64 + evictionRowOverheadBytes
		row.value = s.MemoryValue(row.candidate)

		// A dangling event reference has no event row to delete, so it stays out of the event
		// bookkeeping entirely - exactly as ConsolidateEventMemories keeps it out.
		if joinedEventId.Valid {
			memoriesPerEvent[row.eventId]++
		} else {
			row.eventId = ""
		}

		if s.ShouldConsolidateMemory(row.candidate) {
			consolidating = append(consolidating, row)

			if joinedEventId.Valid {
				deletionsPerEvent[row.eventId]++
			}

			continue
		}

		// Retention exempts a memory from both paths, so it is neither consolidated above nor
		// eligible for eviction below. Counted rather than listed: on a healthy store almost
		// everything is retained.
		if s.MemoryRetained(row.candidate) {
			preview.MemoriesRetained++
			preview.RetainedBytes += row.bytes

			continue
		}

		evictable = append(evictable, row)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to preview consolidation: %s", err.Error())

		return preview, err
	}

	_ = rows.Close()

	preview.MemoriesConsolidated = len(consolidating)

	var consolidatedBytes int64
	for _, row := range consolidating {
		consolidatedBytes += row.bytes
	}

	evicting := previewEvictions(evictable, consolidatedBytes, opts)

	for _, row := range evicting {
		if row.eventId != "" {
			deletionsPerEvent[row.eventId]++
		}
	}

	preview.MemoriesEvicted = len(evicting)
	preview.BytesFreed = consolidatedBytes

	for _, row := range evicting {
		preview.BytesFreed += row.bytes
	}

	// An event goes when every memory it holds goes with it.
	for id, count := range deletionsPerEvent {
		if count >= memoriesPerEvent[id] {
			preview.EventsDeleted++
		}
	}

	// Plus the events that already hold no memories and have themselves decayed past the
	// threshold - the third consolidation pass.
	empty, err := d.previewEmptyEventDeletions(ctx, s)
	if err != nil {
		return preview, err
	}

	preview.EventsDeleted += empty

	preview.Candidates, preview.Truncated = previewSample(consolidating, evicting, limit)

	return preview, nil
}

// previewEvictions returns the memories capacity eviction would delete after consolidation has
// taken its share. It mirrors EvictMemories' selection: nothing at all unless a byte capacity is
// configured and still exceeded, then least-valuable-first until the excess is reclaimed.
func previewEvictions(evictable []previewRow, consolidatedBytes int64, opts PreviewOptions) []previewRow {
	if opts.CapacityBytes <= 0 {
		return nil
	}

	// What the cycle would find when it reaches eviction, consolidation having already run.
	remaining := opts.UsedBytes - consolidatedBytes

	if remaining <= opts.CapacityBytes {
		return nil
	}

	floor := opts.EvictionFloor
	if floor <= 0 || floor > opts.CapacityBytes {
		floor = opts.CapacityBytes
	}

	excess := remaining - floor

	sort.Slice(evictable, func(i int, j int) bool {
		return evictable[i].value < evictable[j].value
	})

	var selected int64
	var evicting []previewRow

	for _, row := range evictable {
		if selected >= excess {
			break
		}

		selected += row.bytes
		evicting = append(evicting, row)
	}

	return evicting
}

// previewSample merges the two sets into one list ordered by value ascending - least valuable
// first, so the memories furthest past the threshold lead - and truncates it to limit.
func previewSample(consolidating []previewRow, evicting []previewRow, limit int) ([]ForgetCandidate, bool) {
	candidates := make([]ForgetCandidate, 0, len(consolidating)+len(evicting))

	for _, row := range consolidating {
		candidates = append(candidates, row.forgetCandidate(ForgetRuleConsolidation))
	}

	for _, row := range evicting {
		candidates = append(candidates, row.forgetCandidate(ForgetRuleEviction))
	}

	sort.Slice(candidates, func(i int, j int) bool {
		return candidates[i].Value < candidates[j].Value
	})

	if len(candidates) > limit {
		return candidates[:limit], true
	}

	return candidates, false
}

// forgetCandidate projects a scanned row onto the reported shape. Bytes is reported net of the
// row overhead the estimate adds, so it reads as the body's stored size rather than as an
// accounting figure.
func (r previewRow) forgetCandidate(rule ForgetRule) ForgetCandidate {
	return ForgetCandidate{
		Id:           r.id,
		EventId:      r.eventId,
		Group:        r.group,
		Significance: r.candidate.MemorySignificance,
		Value:        r.value,
		Bytes:        r.bytes - evictionRowOverheadBytes,
		Rule:         rule,
		TimeStamp:    r.candidate.Timestamp,
		TimeRecalled: r.candidate.TimeRecalled,
		RecallCount:  r.candidate.RecallCount,
	}
}

// RetainedStats returns how many memories are inside the minimum retention window, and their
// stored size. A memory is retained when its decay timestamp - the later of its creation and its
// most recent recall, the same clock consolidation measures age from - is at or after cutoff.
//
// It exists as one aggregate query rather than as a by-product of the consolidation scans because
// those deliberately stay on the covering index and never read body lengths; this is the cost of
// the byte figure, which is the half that matters. Retained bytes approaching the byte capacity is
// what tells an operator the capacity target has become unreachable, since retention overrides it
// - so the caller (Server.evict) only asks when both a retention floor and a byte capacity are
// configured, and nothing pays for it otherwise.
func (d *DB) RetainedStats(ctx context.Context, cutoff int64) (int, int64, error) {
	log.Trace("func() db.RetainedStats")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	// SQLite's two-argument MAX is a scalar function; Postgres and MySQL spell it GREATEST (their
	// MAX is aggregate-only) - the same branch FindSummarisationCandidates makes.
	greatest := `MAX(timestamp, time_recalled)`
	if d.driver != driverSQLite {
		greatest = `GREATEST(timestamp, time_recalled)`
	}

	var count int
	var bytes sql.NullInt64

	// COALESCE because SUM over no rows is NULL, which is the empty-store case rather than an error.
	err := d.queryRow(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(length(body) + `+d.metadataBytesExpr("")+`), 0) FROM memories WHERE `+greatest+` >= ?`,
		cutoff,
	).Scan(&count, &bytes)
	if err != nil {
		log.Errorf("failed to read retained stats: %s", err.Error())

		return 0, 0, err
	}

	// The same per-row allowance EvictMemories and the preview add, so the figure is comparable
	// with used bytes and the capacity target rather than being a bare sum of body lengths.
	return count, bytes.Int64 + int64(count)*evictionRowOverheadBytes, nil
}

// previewEmptyEventDeletions counts the events that hold no memories and have decayed past the
// deletion threshold, mirroring ConsolidateEvents' scan. Events emptied by this same cycle are
// not included here - they are counted from the memory bookkeeping instead, because this query
// sees the store as it stands now, before any of it is deleted.
func (d *DB) previewEmptyEventDeletions(ctx context.Context, s Server) (int, error) {
	log.Trace("func() db.previewEmptyEventDeletions")

	rows, err := d.query(
		ctx,
		`SELECT e.id, e.time_start, e.time_end, COALESCE(l.level_rank, 0), e.link_significance
		FROM events e LEFT JOIN significance_levels l ON l.id = e.significance_level_id
		WHERE e.id NOT IN (SELECT DISTINCT event_id FROM memories WHERE event_id != '')`,
	)
	if err != nil {
		log.Errorf("failed to preview event consolidation: %s", err.Error())

		return 0, err
	}
	defer func() { _ = rows.Close() }()

	count := 0

	for rows.Next() {
		var id string
		var candidate EventConsolidationCandidate

		if err := rows.Scan(&id, &candidate.TimeStart, &candidate.TimeEnd, &candidate.Significance, &candidate.LinkSignificance); err != nil {
			log.Errorf("failed to scan event for preview: %s", err.Error())

			return 0, err
		}

		if s.ShouldConsolidateEvent(candidate) {
			count++
		}
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to preview event consolidation: %s", err.Error())

		return 0, err
	}

	return count, nil
}
