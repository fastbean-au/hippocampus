package hippocampus

import (
	"context"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/types"
)

// Group scoping, RPC side.
//
// A token may name the group labels it is allowed to see (auth.Claims.Groups). This file holds the
// declaration of how each RPC honours that, and the four helpers the handlers use to do it.
//
// Why a declaration table rather than a single chokepoint: the scope cannot be enforced in one
// place, because the RPCs do not all reach the store the same way. A listing RPC carries the scope
// as a query predicate; an RPC addressing rows by id has no predicate to carry it and must instead
// check what those ids are; a walk over the whole store needs it threaded into the pagination; and
// three RPCs cannot be scoped at all and are refused to a scoped caller. Four mechanisms means four
// places to forget one, so the table below records which mechanism each RPC uses and a drift guard
// requires every RPC in the service descriptor to appear. That makes a NEW RPC fail the build until
// somebody decides how it is scoped - which is the failure this design is really guarding against,
// since a missing predicate is silent and looks exactly like a working feature.
//
// The table is documentation and a checklist, NOT the enforcement: it is not consulted at request
// time. What verifies the handlers actually do what they declare is TestGroupScopeIsolation, the
// two-token test that drives every RPC as a caller bound to one group and asserts another group's
// records are never returned, mutated, or confirmed to exist.
//
// This is a SOFT partition. The decay dynamics stay store-global and shared, so a busy group still
// affects what another group forgets, and one number deliberately crosses the boundary:
// link_significance is a denormalised aggregate in the covering index, so a link to a memory in
// another group raises this one's effective significance even though its other end can never be
// read. Recomputing that per-scope would mean joining the link tables in the consolidation scans,
// which is precisely what denormalising it exists to avoid. See the decision record at TODO 60.1.

// scopeMode is how one RPC honours the caller's group scope.
type scopeMode int

const (
	// scopeFilter passes the scope to the store as a query predicate (MemoryFilter.Groups,
	// EventFilter.Groups, ContentQuery.Groups, or the paged walk's groups argument).
	scopeFilter scopeMode = iota

	// scopeIds addresses rows the caller named, so it checks those ids against the scope before
	// acting on them and reports NotFound for any that fall outside it.
	scopeIds

	// scopeWrite creates or replaces rows, so it stamps the group rather than filtering by it (see
	// writeGroup). Where such an RPC also references an existing row - StoreMemory's event_id - that
	// reference is additionally id-checked.
	scopeWrite

	// scopeUnbound cannot be answered within a partition and is refused to a scoped caller.
	scopeUnbound

	// scopeNone exposes no stored record at all, so there is nothing to scope.
	scopeNone
)

// scopes declares how every RPC honours the caller's group scope. Keyed by bare gRPC method name,
// as it appears in contract.Hippocampus_ServiceDesc, matching auth's policies table.
var scopes = map[string]scopeMode{
	// Listing and searching: the scope is a predicate, conjoined with whatever the caller filtered
	// by, so asking for a group outside the scope returns an empty page rather than an error.
	"GetEvents":      scopeFilter,
	"GetMemories":    scopeFilter,
	"SearchMemories": scopeFilter,

	// The forgotten log. A tombstone carries the group its memory had, so both RPCs take the scope
	// as a predicate like any other listing - which is also why neither is scopeUnbound: unlike the
	// preview, they speak only about records that were already the caller's. That is what lets the
	// read sit at reader tier (auth/authz.go) rather than beside the preview, and it is what makes a
	// scoped clear empty that caller's partition of the log and no more.
	"GetForgottenMemories":    scopeFilter,
	"DeleteForgottenMemories": scopeFilter,

	// GetSummarisationCandidates serves a cache the sleep cycle populated store-wide, so it filters
	// that cache by the caller's scope on the way out rather than at the scan - the scan is
	// server-owned and must keep seeing every group, or the groups it skipped would never be
	// summarised for anyone.
	"GetSummarisationCandidates": scopeFilter,

	// The data-movement RPCs walk the store through GetMemoriesPage/GetEventsPage, which take the
	// scope. Export and Transfer therefore move exactly the caller's partition - which is also the
	// useful per-group export - and Clear deletes only what such a walk captured in its manifest.
	"Export":   scopeFilter,
	"Transfer": scopeFilter,
	"Clear":    scopeFilter,

	// Addressed by id: checked against the scope before use, NotFound if outside it.
	"ExplainConsolidation":       scopeIds,
	"EndEvent":                   scopeIds,
	"UpdateEventSignificance":    scopeIds,
	"MergeEvents":                scopeIds,
	"DeleteEvent":                scopeIds,
	"GetEventById":               scopeIds,
	"UpdateMemory":               scopeIds,
	"DeleteMemories":             scopeIds,
	"RecallMemories":             scopeIds,
	"LinkMemories":               scopeIds,
	"UnlinkMemories":             scopeIds,
	"GetMemoryLinks":             scopeIds,
	"LinkEvents":                 scopeIds,
	"UnlinkEvents":               scopeIds,
	"GetEventLinks":              scopeIds,
	"ReplaceMemoriesWithSummary": scopeIds,
	"SummariseMemories":          scopeIds,

	// Writes stamp the group. Import and ImportBatch are writes too, not filtered reads: they upsert
	// rows carrying a group of their own, which must be one the caller holds.
	"StoreEvent":  scopeWrite,
	"StoreMemory": scopeWrite,
	"Import":      scopeWrite,
	"ImportBatch": scopeWrite,

	// Refused to a scoped caller.
	//
	// Purge empties the store; a group-scoped purge would report success for an operation whose
	// name promises something far broader, and it would also have to make the purge gate
	// (InterceptorBlockWhenPurgeInProgress) group-aware or spend the purge rejecting every other
	// group's traffic. Sleep runs the store-global decay cycle, which has no per-group meaning at
	// all under a shared capacity target. PreviewConsolidation enumerates ids, groups and
	// significances from across the whole store by design - that breadth is the feature.
	//
	// None of the three is a partition-holder's operation; all three belong to whoever runs the
	// store, and an unscoped token is what that operator holds.
	// GetTopology joins them for a different reason, and one worth stating because its required
	// TIER is configurable (topology.minimumTier) and this is not. The tier answers "how sensitive
	// is our infrastructure's shape", which varies by deployment and is the operator's to set. This
	// answers "can the question be asked within a partition", and it cannot: there is no per-group
	// topology, so a scoped caller could only ever be shown a store, an index and a set of peers
	// that are not theirs. Lowering the tier to reader therefore widens who may ask among the
	// operator's own people; it never reaches a tenant.
	"Purge":                scopeUnbound,
	"Sleep":                scopeUnbound,
	"PreviewConsolidation": scopeUnbound,
	"GetTopology":          scopeUnbound,

	// WhoAmI describes the caller to itself and reads no stored record.
	"WhoAmI": scopeNone,

	// GetConsolidationStatus reports the sleep cycle's schedule and the aggregate counts of the last
	// one. It names no record - no ids, no groups, no bodies - so there is nothing to scope, which
	// is also why it is not scopeUnbound despite describing a store-wide operation: unlike the
	// preview it enumerates nothing, it only says that a cycle ran and how much it took.
	//
	// Those figures are store-global rather than per-partition, and are shown to a scoped caller for
	// the same reason ExplainConsolidation's capacity pressure and threshold are: the decay dynamics
	// are store-global by design (see the soft-partition note in CLAUDE.md), so they are what
	// actually decides this caller's own memories' fate. A per-partition figure would be the
	// misleading one.
	"GetConsolidationStatus": scopeNone,
}

// scopedGroups returns the caller's group scope and whether they are bound to one at all. Handlers
// must branch on the bool, never on the slice's length: an empty slice means "unscoped, show
// everything", and treating it as "scoped to nothing" would make an unauthenticated instance return
// an empty store for every read.
func (s *Server) scopedGroups(ctx context.Context) ([]string, bool) {
	return auth.GroupsFromContext(ctx)
}

// requireUnbound refuses a scoped caller an RPC that cannot be answered within a partition (see the
// scopeUnbound entries above). It names the reason rather than returning a bare code, because the
// caller is not doing anything wrong - they are holding the wrong kind of token for the job, and
// PermissionDenied alone would read as "your role is too low" and send them to change the wrong
// thing.
func (s *Server) requireUnbound(ctx context.Context, rpc string) error {
	groups, bound := s.scopedGroups(ctx)
	if !bound {
		return nil
	}

	log.Debugf("%s refused: caller is scoped to groups %s", rpc, strings.Join(groups, ","))

	return status.Errorf(
		codes.PermissionDenied,
		"%s operates on the whole store and is not available to a group-scoped token (this token is scoped to: %s)",
		rpc,
		strings.Join(groups, ", "),
	)
}

// writeGroup resolves the group a write should be stamped with, given what the caller asked for.
//
// Three cases, and the middle one is the reason this exists rather than a plain scope check:
//
//   - Unscoped caller: whatever they asked for, including nothing. Unchanged behaviour.
//   - Scoped caller naming no group: the scope's sole group is stamped. Without this a bound writer
//     would create records in the unnamed group "" and then be unable to read back what it had just
//     written, which is a worse failure than a refusal because it looks like data loss.
//   - Scoped caller naming a group: allowed if it is in scope, refused otherwise.
//
// A caller holding several groups and naming none is refused, since there is no basis to choose
// between them and picking the first would be an arbitrary rule that quietly files records
// somewhere the caller did not intend.
func (s *Server) writeGroup(ctx context.Context, requested string) (string, error) {
	groups, bound := s.scopedGroups(ctx)

	if !bound {
		return requested, nil
	}

	if requested == "" {
		if len(groups) == 1 {
			return groups[0], nil
		}

		return "", status.Errorf(
			codes.InvalidArgument,
			"this token is scoped to several groups (%s), so a write must name which one it belongs to",
			strings.Join(groups, ", "),
		)
	}

	if !auth.GroupInScope(groups, requested) {
		return "", status.Errorf(codes.PermissionDenied, "group %q is outside this token's scope", requested)
	}

	return requested, nil
}

// scopeMemoryIds and scopeEventIds refuse a request naming any record outside the caller's scope.
//
// The refusal is NotFound, deliberately, and NOT PermissionDenied: a caller must not be able to
// learn that a record exists by watching which of the two errors comes back, and an out-of-scope
// record should be as invisible as one that was never stored. That also makes the error identical
// to the one these RPCs already return for an id that genuinely does not exist, so a handler needs
// no second branch and a client needs no new failure mode.
//
// For the same reason the message names the ids the caller supplied - it tells them nothing they
// did not already know - and never says whether the id existed.
func (s *Server) scopeMemoryIds(ctx context.Context, ids []string) error {
	groups, bound := s.scopedGroups(ctx)

	if !bound || len(ids) == 0 {
		return nil
	}

	outside, err := s.db.MemoryIdsOutsideGroups(ctx, ids, groups)
	if err != nil {
		return mapError(err)
	}

	if len(outside) > 0 {
		return status.Errorf(codes.NotFound, "no such memory: %s", strings.Join(outside, ", "))
	}

	return nil
}

// memoryIdsOutsideScope and eventIdsOutsideScope report which of the named ids the caller cannot
// see, without turning that into an error. They back the link paths that must drop what they found
// rather than refuse: an id the caller never named (the far end of a link, an unlink target that
// may or may not exist) has to behave exactly as if it were absent, because any other treatment
// would let the caller detect a record on the other side of the boundary.
//
// An unbound caller gets an empty result, so callers can use these unconditionally.
func (s *Server) memoryIdsOutsideScope(ctx context.Context, ids []string) ([]string, error) {
	groups, bound := s.scopedGroups(ctx)

	if !bound || len(ids) == 0 {
		return nil, nil
	}

	return s.db.MemoryIdsOutsideGroups(ctx, ids, groups)
}

func (s *Server) eventIdsOutsideScope(ctx context.Context, ids []string) ([]string, error) {
	groups, bound := s.scopedGroups(ctx)

	if !bound || len(ids) == 0 {
		return nil, nil
	}

	return s.db.EventIdsOutsideGroups(ctx, ids, groups)
}

// filterMemoriesToScope drops memories outside the caller's scope from a result set.
//
// It is the one place scoping is applied to rows rather than to a query, and it is used only where
// the ids were not the caller's to begin with: link traversal (RecallMemories/SearchMemories
// include_linked, the GetMemories linked_to filter) reaches memories the caller never named, so
// there is nothing to refuse - the correct answer is that those neighbours are simply not there.
// Refusing the whole request instead would let a caller detect a cross-group link by watching a
// recall of their own memories start failing.
//
// The rows are already in hand, so this costs no query. It must NOT be used in place of a query
// predicate anywhere a limit applies, since dropping rows after the fact would silently shorten a
// page and make the shortfall report how much the caller cannot see.
func (s *Server) filterMemoriesToScope(ctx context.Context, memories []types.Memory) []types.Memory {
	groups, bound := s.scopedGroups(ctx)

	if !bound || len(memories) == 0 {
		return memories
	}

	filtered := make([]types.Memory, 0, len(memories))

	for _, memory := range memories {
		if auth.GroupInScope(groups, memory.Group) {
			filtered = append(filtered, memory)
		}
	}

	return filtered
}

func (s *Server) scopeEventIds(ctx context.Context, ids []string) error {
	groups, bound := s.scopedGroups(ctx)

	if !bound || len(ids) == 0 {
		return nil
	}

	outside, err := s.db.EventIdsOutsideGroups(ctx, ids, groups)
	if err != nil {
		return mapError(err)
	}

	if len(outside) > 0 {
		return status.Errorf(codes.NotFound, "no such event: %s", strings.Join(outside, ", "))
	}

	return nil
}
