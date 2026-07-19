package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/montanaflynn/stats"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/types"
)

// ErrEventNotFound is returned (wrapped) by GetEvent when no event has the requested id, so
// callers can map it to a gRPC NotFound with errors.Is rather than an opaque Unknown.
var ErrEventNotFound = errors.New("event not found")

// eventColumns is the read projection. significance is the level's rank, exposed by eventsFrom's
// join to the registry; scanEvent reads it into types.Event.Significance. Use it with eventsFrom as
// the FROM source, never the bare events table (which has no significance column).
const eventColumns = `id, time_start, time_end, significance, name, description, memories_consolidated, relationship_significance, relationships, group_name`

// eventStoredColumns is the physical column list of the events table (significance_level_id, not the
// removed significance): used for INSERT (transfer import).
const eventStoredColumns = `id, time_start, time_end, significance_level_id, name, description, memories_consolidated, relationship_significance, relationships, group_name`

// eventsFrom is the read source for eventColumns: events LEFT JOINed to the significance registry
// and aliased back to "events", so WHERE/ORDER clauses naming bare columns (id, significance, ...)
// need no change. An unranked (NULL) level reads as significance 0.
const eventsFrom = `(SELECT e.id, e.time_start, e.time_end, COALESCE(l.level_rank, 0) AS significance,
	e.name, e.description, e.memories_consolidated, e.relationship_significance, e.relationships,
	e.group_name FROM events e LEFT JOIN significance_levels l ON l.id = e.significance_level_id) AS events`

func scanEvent(scan func(dest ...any) error) (types.Event, error) {
	var e types.Event
	var relationships string

	if err := scan(
		&e.Id,
		&e.TimeStart,
		&e.TimeEnd,
		&e.Significance,
		&e.Name,
		&e.Description,
		&e.MemoriesConsolidated,
		&e.RelationshipSignificance,
		&relationships,
		&e.Group,
	); err != nil {
		return e, err
	}

	r := make([]types.Relationship, 0)

	if err := json.Unmarshal([]byte(relationships), &r); err != nil {
		return e, err
	}

	e.Relationships = r

	return e, nil
}

// CreateEvent creates an event record, returning the id and an error
func (d *DB) CreateEvent(ctx context.Context, event types.Event) (string, error) {
	log.Trace("func() db.CreateEvent")

	// Defaults first, then validate: SetDefaults fills a zero time_start with the current time, so
	// validating afterwards makes time_start optional on create (matching how Memory's time_stamp
	// already works) rather than the default being unreachable dead code.
	event.SetDefaults()

	if err := event.Validate(false); err != nil {
		return "", err
	}

	event.RelationshipSignificance = event.CalculateRelationshipSignificance()

	relationships, err := json.Marshal(event.Relationships)
	if err != nil {
		return "", err
	}

	levelID, err := d.ensureSignificanceLevel(ctx, event.Significance, event.SignificanceLevelID)
	if err != nil {
		return "", err
	}

	event.SignificanceLevelID = levelID

	_, err = d.exec(ctx,
		`INSERT INTO events (id, time_start, time_end, significance_level_id, name, description, relationship_significance, relationships, group_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.Id,
		event.TimeStart,
		event.TimeEnd,
		levelIDArg(event.SignificanceLevelID),
		event.Name,
		event.Description,
		event.RelationshipSignificance,
		string(relationships),
		event.Group,
	)

	return event.Id, err
}

// UpdateEvent applies a partial update to an existing event: only fields carrying a non-zero
// value overwrite the stored row. It does not create the event when the id is unknown - it
// returns (false, nil) so callers can surface NotFound rather than silently inserting a phantom
// row (an empty-id or unknown-id upsert used to poison eviction's LEFT JOIN and the event scans).
// Returns whether a matching event existed.
func (d *DB) UpdateEvent(ctx context.Context, event types.Event) (bool, error) {
	log.Trace("func() db.UpdateEvent")

	// Build the SET list from only the fields carrying a value, mirroring the previous
	// conditional-overwrite semantics without the upsert. Portable across all three dialects - no
	// excluded/new pseudo-row, so no per-driver branch.
	var (
		sets []string
		args []any
	)

	if event.TimeStart > 0 {
		sets = append(sets, `time_start = ?`)
		args = append(args, event.TimeStart)
	}

	if event.TimeEnd > 0 {
		sets = append(sets, `time_end = ?`)
		args = append(args, event.TimeEnd)
	}

	// Significance is changed when the caller supplied a placement-resolved level id, or an absolute
	// value > 0 (resolved here). A nil level id with a non-positive value leaves significance
	// untouched.
	levelID := event.SignificanceLevelID

	if levelID == nil && event.Significance > 0 {
		resolved, err := d.ensureSignificanceLevel(ctx, event.Significance, nil)
		if err != nil {
			return false, err
		}

		levelID = resolved
	}

	if levelID != nil {
		sets = append(sets, `significance_level_id = ?`)
		args = append(args, *levelID)
	}

	if event.Name != "" {
		sets = append(sets, `name = ?`)
		args = append(args, event.Name)
	}

	if event.Description != "" {
		sets = append(sets, `description = ?`)
		args = append(args, event.Description)
	}

	if event.Group != "" {
		sets = append(sets, `group_name = ?`)
		args = append(args, event.Group)
	}

	if len(event.Relationships) > 0 {

		// TODO: relationships coming into here need to be the intended values
		relationships, err := json.Marshal(event.Relationships)
		if err != nil {
			return false, err
		}

		sets = append(sets, `relationship_significance = ?`, `relationships = ?`)
		args = append(args, event.CalculateRelationshipSignificance(), string(relationships))
	}

	// Nothing to change: there is no UPDATE to learn existence from, so probe for it directly.
	if len(sets) == 0 {
		return d.EventExists(ctx, event.Id)
	}

	args = append(args, event.Id)

	res, err := d.exec(ctx, `UPDATE events SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return false, err
	}

	// Existence from the UPDATE itself (RowsAffected), so a concurrent delete can't slip between a
	// separate probe and the UPDATE - see updatedRowExisted for the MySQL changed-vs-matched caveat.
	return d.updatedRowExisted(ctx, res, "events", event.Id)
}

// EventExists reports whether an event with the given id exists. It backs the write-path guards
// that reject a memory or a merge referencing a nonexistent event, so a dangling event_id is never
// created in the first place.
func (d *DB) EventExists(ctx context.Context, id string) (bool, error) {
	log.Trace("func() db.EventExists")

	var exists bool

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}

	return exists, nil
}

func (d *DB) setEventConsolidated(ctx context.Context, id string) error {
	log.Trace("func() db.setEventConsolidated")

	// The value is bound rather than a literal 1: the column is INTEGER on SQLite but BOOLEAN on
	// Postgres, and a bound true converts cleanly for both.
	_, err := d.exec(ctx, `UPDATE events SET memories_consolidated = ? WHERE id = ?`, true, id)

	return err
}

// DeleteEvent deletes the event with the given id, returning whether a row was actually deleted so
// the caller can surface NotFound for an unknown id rather than reporting success unconditionally.
// DELETE reports RowsAffected reliably on all three dialects (the MySQL "0 rows affected on a
// value-unchanged UPDATE" quirk does not apply to DELETE).
func (d *DB) DeleteEvent(ctx context.Context, id string) (bool, error) {
	log.Trace("func() db.DeleteEvent")

	// TODO: get relationships, and remove foreign components

	res, err := d.exec(ctx, `DELETE FROM events WHERE id = ?`, id)
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return n > 0, nil
}

// DeleteEventIfEmpty deletes the event only if it currently has no memories. Consolidation and
// eviction scans decide an event is empty from a snapshot taken before the delete runs; a
// concurrent write can attach a fresh memory to that event in the gap between the two. Checking
// emptiness in the same statement as the delete closes that window, so a memory written mid-scan
// keeps its parent event instead of being left with a dangling event_id. Returns whether the
// event was deleted.
func (d *DB) DeleteEventIfEmpty(ctx context.Context, id string) (bool, error) {
	log.Trace("func() db.DeleteEventIfEmpty")

	res, err := d.exec(ctx,
		`DELETE FROM events WHERE id = ? AND NOT EXISTS (SELECT 1 FROM memories WHERE event_id = ?)`,
		id,
		id,
	)
	if err != nil {
		return false, err
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return n > 0, nil
}

func (d *DB) GetEvent(ctx context.Context, id string) (*types.Event, error) {
	log.Trace("func() db.GetEvent")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, `SELECT `+eventColumns+` FROM `+eventsFrom+` WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("event '%s': %w", id, ErrEventNotFound)
	}

	event, err := scanEvent(rows.Scan)
	if err != nil {
		return nil, err
	}

	return &event, nil
}

// eventOrderClauses maps the API order_by values to fixed, injection-safe ORDER BY clauses. The
// order_by string is never interpolated into SQL directly — only these constant clauses are. A
// stable id tiebreaker keeps offset pagination deterministic across pages.
var eventOrderClauses = map[string]string{
	"significance": `significance DESC, time_start DESC, id ASC`,
	"timestamp":    `time_start DESC, id ASC`,
}

const defaultEventOrderBy = "significance"

// eventFilterConditions builds the shared WHERE clause and its args for the events filter, so
// GetEvents and CountEventsFiltered stay in lock-step over the exact same predicate.
//
// SignificanceExtremum, when set, replaces the SignificanceMin/SignificanceMax range check with an
// equality match against the highest (or lowest) significance value among events matching the
// other filters - computed via a subquery built from this same function (with the extremum and
// range fields cleared), so the "other filters" stay identical between the two.
func eventFilterConditions(filter EventFilter) (string, []any) {
	query := ` WHERE 1=1`
	var args []any

	if filter.TimeStartMin > 0 {
		query += ` AND time_start >= ?`
		args = append(args, filter.TimeStartMin)
	}

	if filter.TimeStartMax > 0 {
		query += ` AND time_start <= ?`
		args = append(args, filter.TimeStartMax)
	}

	if filter.TimeEndMin > 0 {
		query += ` AND time_end >= ?`
		args = append(args, filter.TimeEndMin)
	}

	if filter.TimeEndMax > 0 {
		query += ` AND time_end <= ?`
		args = append(args, filter.TimeEndMax)
	}

	if filter.Group != "" {
		query += ` AND group_name = ?`
		args = append(args, filter.Group)
	}

	if filter.SignificanceExtremum != SignificanceExtremumNone {
		aggregate := "MAX"
		if filter.SignificanceExtremum == SignificanceExtremumLowest {
			aggregate = "MIN"
		}

		subFilter := filter
		subFilter.SignificanceExtremum = SignificanceExtremumNone
		subFilter.SignificanceMin = 0
		subFilter.SignificanceMax = 0
		subWhere, subArgs := eventFilterConditions(subFilter)

		query += ` AND significance = (SELECT ` + aggregate + `(significance) FROM ` + eventsFrom + subWhere + `)`
		args = append(args, subArgs...)

		return query, args
	}

	if filter.SignificanceMin > 0 {
		query += ` AND significance >= ?`
		args = append(args, filter.SignificanceMin)
	}

	if filter.SignificanceMax > 0 {
		query += ` AND significance <= ?`
		args = append(args, filter.SignificanceMax)
	}

	return query, args
}

// CountEventsFiltered returns the number of events matching the filter, ignoring Limit/Offset so
// the caller can size pagination.
func (d *DB) CountEventsFiltered(ctx context.Context, filter EventFilter) (int, error) {
	log.Trace("func() db.CountEventsFiltered")

	where, args := eventFilterConditions(filter)

	var count int

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM `+eventsFrom+where, args...).Scan(&count); err != nil {
		return 0, err
	}

	return count, nil
}

func (d *DB) GetEvents(ctx context.Context, filter EventFilter) (*[]types.Event, error) {
	log.Trace("func() db.GetEvents")

	where, args := eventFilterConditions(filter)

	order, ok := eventOrderClauses[filter.OrderBy]
	if !ok {
		order = eventOrderClauses[defaultEventOrderBy]
	}

	query := `SELECT ` + eventColumns + ` FROM ` + eventsFrom + where + ` ORDER BY ` + order

	// OFFSET is only valid alongside LIMIT in SQLite/MySQL, so both are gated on a positive limit.
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)

		if filter.Offset > 0 {
			query += ` OFFSET ?`
			args = append(args, filter.Offset)
		}
	}

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var events []types.Event

	for rows.Next() {
		event, err := scanEvent(rows.Scan)
		if err != nil {
			return nil, err
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &events, nil
}

// ConsolidateEvents deletes events that have no associated memories and whose own value has
// decayed below the deletion threshold. Events with memories are handled by
// ConsolidateEventMemories, which deletes an event when its last memory is consolidated.
func (d *DB) ConsolidateEvents(ctx context.Context, s Server) (int, error) {
	log.Trace("func() db.ConsolidateEvents")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		ctx,
		`SELECT e.id, e.time_start, e.time_end, COALESCE(l.level_rank, 0), e.relationship_significance
		FROM events e LEFT JOIN significance_levels l ON l.id = e.significance_level_id
		WHERE e.id NOT IN (SELECT DISTINCT event_id FROM memories WHERE event_id != '')`,
	)
	if err != nil {
		log.Errorf("failed to consolidate events: %s", err.Error())

		return 0, err
	}
	defer func() { _ = rows.Close() }()

	var deletions []string

	for rows.Next() {
		var id string
		var candidate EventConsolidationCandidate

		if err := rows.Scan(&id, &candidate.TimeStart, &candidate.TimeEnd, &candidate.Significance, &candidate.RelationshipSignificance); err != nil {
			log.Errorf("failed to scan event for consolidation: %s", err.Error())

			return 0, err
		}

		if s.ShouldConsolidateEvent(candidate) {
			deletions = append(deletions, id)
		}
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to consolidate events: %s", err.Error())

		return 0, err
	}

	_ = rows.Close()

	// The per-event deletes are best-effort - retErr surfaces the first failure for the sleep
	// cycle's success metric without stopping the remaining events.
	count := 0
	var retErr error

	for _, id := range deletions {
		deleted, err := d.DeleteEventIfEmpty(ctx, id)
		if err != nil {
			log.Errorf("failed to delete event '%s' for event consolidation: %s", id, err.Error())

			if retErr == nil {
				retErr = err
			}

			continue
		}

		if deleted {
			count++
		}
	}

	return count, retErr
}

func (d *DB) CountEvents(ctx context.Context) int {
	log.Trace("func() db.Count")

	var count int

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(ctx, `SELECT COUNT(*) FROM events`).Scan(&count); err != nil {
		log.Errorf("failed to count events: %s", err.Error())

		return -1
	}

	return count
}

func (d *DB) CalculateSignificancePercentile(ctx context.Context, percent float64) (float64, error) {
	log.Trace("func() db.CalculateSignificancePercentile")

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(ctx, `SELECT COALESCE(l.level_rank, 0) FROM events e
		LEFT JOIN significance_levels l ON l.id = e.significance_level_id`)
	if err != nil {
		return 0.0, err
	}
	defer func() { _ = rows.Close() }()

	var sigs stats.Float64Data

	for rows.Next() {
		var sig int32

		if err := rows.Scan(&sig); err != nil {
			return 0.0, err
		}

		sigs = append(sigs, float64(sig))
	}

	if err := rows.Err(); err != nil {
		return 0.0, err
	}

	return stats.PercentileNearestRank(sigs, percent)
}
