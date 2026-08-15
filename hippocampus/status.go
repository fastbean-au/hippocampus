package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
)

// GetConsolidationStatus reports what this instance's sleep cycle is doing: when the next timed
// cycle is due, whether one is running, and what the last one did.
//
// It is the fourth and last of the forgetting-transparency RPCs, and the only one that answers
// "when". PreviewConsolidation says what would go, ExplainConsolidation where a memory stands, and
// GetForgottenMemories what went - and without a schedule beside them a store that forgets every
// two minutes is indistinguishable from one that is doing nothing at all.
//
// Three things carry the design.
//
// (1) It reads only in-memory atomics - no database, no scan - so a client may poll it at whatever
// rate its display needs. That is deliberate and is what lets the console show a live countdown
// without touching the store; the expensive figures (capacity pressure, used bytes, memory count)
// stay behind ExplainConsolidation's cached snapshot, and this reports that cache's TTL so a
// polling client paces its calls to that RPC from the server rather than from a guess.
//
// (2) It does NOT refuse on a read/write replica, unlike Sleep, PreviewConsolidation and
// ExplainConsolidation. Reporting consolidation_enabled false IS the answer there: refusing would
// leave a client unable to distinguish a replica from an instance whose cycle had stopped, which is
// the one thing this RPC exists to make visible.
//
// (3) It is reader-tier and scopeNone (hippocampus/scope.go): it names no stored record and returns
// counts, not ids. Counts are not enumeration, which is precisely what separates it from the
// preview.
func (s *Server) GetConsolidationStatus(
	ctx context.Context,
	in *contract.EmptyRequest,
) (*contract.GetConsolidationStatusResponse, error) {
	log.Debug("GetConsolidationStatus()")

	var res contract.GetConsolidationStatusResponse

	res.ConsolidationEnabled = s.consolidationEnabled

	// A replica's store is consolidated by whichever instance holds the single-consolidator lock,
	// under THAT instance's configuration - so this one's schedule would describe a cycle it never
	// runs. Everything below stays zero rather than reporting a period nothing acts on.
	if !s.consolidationEnabled {
		return &res, nil
	}

	res.PeriodSeconds = int64(s.sleepPeriod.Seconds())
	res.NextSleepAt = s.nextSleep.Load()
	res.WalTriggerEnabled = s.consolidation.walTriggerBytes > 0
	res.SleepInProgress = s.sleepInProgress.Load()
	res.SnapshotTtlSeconds = int64(explainStateTTL.Seconds())

	if last := s.lastCycle.Load(); last != nil {
		res.LastCycle = cycleReportToProto(last)
	}

	return &res, nil
}

// cycleReportToProto projects the internal report onto the wire. Separate from the handler so a
// test can assert the mapping without standing up a server, and so the internal struct stays free
// to hold Go types.
func cycleReportToProto(in *cycleReport) *contract.CycleReport {
	return &contract.CycleReport{
		StartedAt:               in.startedAt.UnixNano(),
		DurationMs:              in.duration.Milliseconds(),
		MemoriesConsolidated:    int32(in.memoriesConsolidated),
		EventsConsolidated:      int32(in.eventsConsolidated),
		MemoriesEvicted:         int32(in.memoriesEvicted),
		EventsEvicted:           int32(in.eventsEvicted),
		BytesFreed:              in.bytesFreed,
		SummarisationCandidates: int32(in.summarisationCandidates),
		Success:                 in.success,
		Failure:                 in.failure,
		Trigger:                 in.trigger,
	}
}
