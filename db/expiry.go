package db

import (
	"context"

	log "github.com/sirupsen/logrus"
)

// ExpiryResult is what one expiry pass deleted.
type ExpiryResult struct {
	Memories int
	Events   int
}

// ExpireMemories deletes every memory stored before createdBefore (UnixNano), whatever its value and
// however recently it was recalled. It is consolidation.maximumRetentionInDays: a CEILING, where
// every other rule in the store is a judgement (TODO-3 item 157).
//
// The decay rules cannot express a ceiling on their own. Recall resets the decay clock, so a memory
// read often enough is never forgotten - which is the design, and is also why a storage-limitation
// policy ("never keep this longer than 90 days") had no way to be stated. The age here is measured
// from creation, not from the last recall, for exactly that reason.
//
// It selects by creation time alone and deletes through the same chokepoint as the other passes, so
// links, the search outbox, callbacks and the forgotten log all follow without a line of their own
// here. The recall-race guard in that chokepoint still applies: a memory recalled between the scan
// and the delete is spared this cycle and taken on the next, which is the right side to err on for
// something that is past its ceiling either way.
//
// It deliberately ignores consolidation.minimumRetentionInDays. A ceiling that a floor can override
// is not a ceiling; startup refuses a maximum that is not above the minimum, so the two only meet for
// a memory recalled recently after a long life, and there the ceiling wins.
func (d *DB) ExpireMemories(ctx context.Context, createdBefore int64) (ExpiryResult, error) {
	log.Trace("func() db.ExpireMemories")

	var result ExpiryResult

	// The query timeout bounds the scan only; the deletes below take their own (see item 151).
	scanCtx, cancel := d.opContext(ctx)
	defer cancel()

	rows, err := d.query(
		scanCtx,
		`SELECT id, time_recalled, recall_count, event_id FROM memories WHERE timestamp < ?`,
		createdBefore,
	)
	if err != nil {
		log.Errorf("failed to scan for expired memories: %s", err.Error())

		return result, err
	}
	defer func() { _ = rows.Close() }()

	var deletions []memoryRecallSnapshot

	eventOf := make(map[string]string)

	for rows.Next() {
		var snapshot memoryRecallSnapshot
		var eventId string

		if err := rows.Scan(&snapshot.id, &snapshot.timeRecalled, &snapshot.recallCount, &eventId); err != nil {
			log.Errorf("failed to scan an expired memory: %s", err.Error())

			return result, err
		}

		deletions = append(deletions, snapshot)

		if eventId != "" {
			eventOf[snapshot.id] = eventId
		}
	}

	if err := rows.Err(); err != nil {
		log.Errorf("failed to scan for expired memories: %s", err.Error())

		return result, err
	}

	_ = rows.Close()

	if len(deletions) == 0 {
		return result, nil
	}

	// No value and no threshold: neither was the reason. The forgotten log records the rule, which
	// says what happened, and zeroes for the two figures that only describe a decay decision.
	reason := forgetReason{cause: CauseExpiry}

	if d.tombstones.Enabled && d.tombstoneTable {
		reason.rule = ForgetRuleExpiry
	}

	deletedIds, err := d.deleteMemoriesIfUnrecalled(ctx, deletions, reason)
	result.Memories = len(deletedIds)

	if err != nil {
		log.Errorf("failed to delete expired memories: %s", err.Error())

		return result, err
	}

	// An event that lost a memory is either empty now - deleted, as eviction deletes one - or
	// flagged, which is what memories_consolidated means (item 140).
	touched := make(map[string]bool)

	for _, id := range deletedIds {
		if eventId, ok := eventOf[id]; ok {
			touched[eventId] = true
		}
	}

	var retErr error

	for eventId := range touched {
		deleted, err := d.DeleteEventIfEmpty(ctx, eventId, CauseExpiry)
		if err != nil {
			log.Errorf("failed to delete event '%s' after expiry: %s", eventId, err.Error())

			if retErr == nil {
				retErr = err
			}
		}

		if deleted {
			result.Events++

			continue
		}

		if err := d.setEventConsolidated(ctx, eventId); err != nil {
			log.Errorf("failed to set MemoriesConsolidated for event '%s' after expiry: %s", eventId, err.Error())

			if retErr == nil {
				retErr = err
			}
		}
	}

	return result, retErr
}
