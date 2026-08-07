package db

import (
	"context"
	"database/sql"

	log "github.com/sirupsen/logrus"
)

// IdentifiedMemoryCandidate pairs a memory's identity with the consolidation decision inputs for
// it. It is what a caller that already knows which memories it cares about needs in order to
// evaluate them through the same Server rules the sleep cycle applies, without scanning the store.
type IdentifiedMemoryCandidate struct {
	Id        string
	EventId   string
	Candidate MemoryConsolidationCandidate
}

// GetMemoryConsolidationCandidates returns the consolidation decision inputs for the given memory
// ids. Ids that do not exist are simply absent from the result, and the order is the storage
// layer's rather than the request's - the caller holds the ids, so it can order the answers itself.
//
// The projection mirrors PreviewConsolidation's, minus the reporting columns (group_name and the
// body length) that a per-id lookup has no use for: the same LEFT JOIN and the same COALESCE
// defaults, so a memory whose event no longer exists is scored as event-less rather than dropped by
// an INNER JOIN, and e.id distinguishes a real event from such a dangling reference.
func (d *DB) GetMemoryConsolidationCandidates(ctx context.Context, ids []string) ([]IdentifiedMemoryCandidate, error) {
	log.Trace("func() db.GetMemoryConsolidationCandidates")

	if len(ids) == 0 {
		return nil, nil
	}

	ranks, err := d.loadSignificanceRanks(ctx)
	if err != nil {
		log.Errorf("failed to load significance registry: %s", err.Error())

		return nil, err
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	candidates := make([]IdentifiedMemoryCandidate, 0, len(ids))

	// Chunked like GetMemoriesByIds to stay well inside bound-parameter limits, even though the RPC
	// layer caps a request far below them.
	for start := 0; start < len(ids); start += deleteChunkSize {
		end := min(start+deleteChunkSize, len(ids))

		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, v := range chunk {
			args[i] = v
		}

		chunkCandidates, err := d.memoryCandidateChunk(ctx, ranks, args, len(chunk))
		if err != nil {
			return nil, err
		}

		candidates = append(candidates, chunkCandidates...)
	}

	return candidates, nil
}

// memoryCandidateChunk reads one chunk's worth of candidates. It is split out so the rows are
// closed before the next chunk's query is issued: the SQLite pool holds a single connection, so a
// query opened while another's rows are still being read would deadlock.
func (d *DB) memoryCandidateChunk(
	ctx context.Context,
	ranks map[int64]int32,
	args []any,
	count int,
) ([]IdentifiedMemoryCandidate, error) {
	rows, err := d.query(
		ctx,
		`SELECT m.id, m.timestamp, m.significance_level_id, m.time_recalled, m.recall_count, m.event_id,
			e.significance_level_id, COALESCE(e.link_significance, 0), m.link_significance, e.id
		FROM memories m LEFT JOIN events e ON e.id = m.event_id
		WHERE m.id IN (`+placeholders(count)+`)`,
		args...,
	)
	if err != nil {
		log.Errorf("failed to read memory consolidation candidates: %s", err.Error())

		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var candidates []IdentifiedMemoryCandidate

	for rows.Next() {
		var candidate IdentifiedMemoryCandidate
		var joinedEventId sql.NullString
		var memoryLevelID, eventLevelID sql.NullInt64

		if err := rows.Scan(
			&candidate.Id,
			&candidate.Candidate.Timestamp,
			&memoryLevelID,
			&candidate.Candidate.TimeRecalled,
			&candidate.Candidate.RecallCount,
			&candidate.EventId,
			&eventLevelID,
			&candidate.Candidate.EventLinkSignificance,
			&candidate.Candidate.MemoryLinkSignificance,
			&joinedEventId,
		); err != nil {
			log.Errorf("failed to scan memory consolidation candidate: %s", err.Error())

			return nil, err
		}

		candidate.Candidate.MemorySignificance = rankOf(ranks, memoryLevelID)
		candidate.Candidate.EventSignificance = rankOf(ranks, eventLevelID)

		// A dangling event reference has no event to attribute the memory to, and the decay already
		// scores it as event-less; reporting the id would suggest an event a client could open.
		if !joinedEventId.Valid {
			candidate.EventId = ""
		}

		candidates = append(candidates, candidate)
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to read memory consolidation candidates: %s", err.Error())

		return nil, err
	}

	return candidates, nil
}
