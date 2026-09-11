package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// hippoClient is the client seam: the three RPCs this integration is allowed to make, and no
// others. Dial hands back the whole generated client, so this declaration is the only thing
// standing between an object-storage agent and Purge - the same rule, and the same reasoning, as
// the broker bridges' own seven-method interface.
type hippoClient interface {
	RecallMemories(
		ctx context.Context,
		in *contract.RecallMemoriesRequest,
		opts ...grpc.CallOption,
	) (*contract.GetMemoriesResponse, error)

	ExplainConsolidation(
		ctx context.Context,
		in *contract.ExplainConsolidationRequest,
		opts ...grpc.CallOption,
	) (*contract.ExplainConsolidationResponse, error)

	GetForgottenMemories(
		ctx context.Context,
		in *contract.GetForgottenMemoriesRequest,
		opts ...grpc.CallOption,
	) (*contract.GetForgottenMemoriesResponse, error)
}

// explainChunkSize is the service's own cap on one ExplainConsolidation call. Held identifies ids
// in chunks of it.
const explainChunkSize = 200

// forgottenPageSize is the page size used to walk the forgotten log. The service caps it at 1000.
const forgottenPageSize = 500

// ErrConsolidationDisabled reports that the instance being asked is a replica, so it cannot answer
// which memories it still holds. It is separated out because the fix is a configuration one -
// point the reaper at the consolidating instance - rather than anything the process can retry.
var ErrConsolidationDisabled = errors.New("the instance has consolidation disabled, so it cannot be asked which memories it still holds")

// Memories wraps the RPCs behind names that say what this integration wants from them.
type Memories struct {
	client      hippoClient
	callTimeout time.Duration
}

// NewMemories wraps a dialled client. A non-positive timeout leaves each call bounded only by the
// caller's context.
func NewMemories(client hippoClient, callTimeout time.Duration) *Memories {
	return &Memories{
		client:      client,
		callTimeout: callTimeout,
	}
}

// Recall reinforces the named memories, returning how many of them the store actually held.
//
// An id the store does not hold is a SILENT NO-OP - recall is an UPDATE ... WHERE id IN (...) that
// matches nothing - which is the property the whole tap rests on: it can fire a speculative recall
// for every object anybody reads, without a lookup, without a cache and without any record of what
// it has seen before. The count is therefore not an error signal but a HIT RATE, and the tap reads
// it as one.
//
// The token must be UNSCOPED and WRITER tier. A group-scoped token turns an id the store does not
// hold into NotFound for the whole batch, since the service scope-checks ids before recalling them
// and deliberately does not distinguish "no such memory" from "not yours"; a reader-tier token gets
// a plain non-reinforcing read unless the deployment sets auth.readerRecallReinforces. NotFound is
// absorbed rather than returned, so a misconfigured token degrades to "reinforcement stops working"
// instead of "the gateway stops serving objects".
func (m *Memories) Recall(ctx context.Context, ids []string) (int, error) {
	log.Trace("func() client.Memories.Recall")

	if len(ids) == 0 {
		return 0, nil
	}

	ctx, cancel := m.callContext(ctx)
	defer cancel()

	// include_linked stays false: it would pull back one-hop neighbours that are NOT reinforced,
	// inflating the response and the hit count with memories nothing was read.
	resp, err := m.client.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: ids})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			log.WithField("ids", len(ids)).
				Debug("recall reported no such memory; the token is group-scoped")

			return 0, nil
		}

		return 0, fmt.Errorf("recalling %d memories: %w", len(ids), err)
	}

	return len(resp.GetMemories()), nil
}

// Held reports which of the given ids the store still holds.
//
// It asks ExplainConsolidation, which answers only about the ids it is given and simply omits the
// ones that are not there - an existence oracle that enumerates nothing, needs only reader tier,
// and reads a per-id primary key lookup rather than a scan. That "omits" is the entire contract
// being relied on here, so the absence of an id is read as absence rather than as an error.
//
// The one thing it cannot do is answer on a replica, which refuses the RPC outright because the
// decay policy it would describe is not the one being carried out. That surfaces as
// ErrConsolidationDisabled rather than as a transport failure, because the sweep must stop rather
// than treat "cannot ask" as "the store does not hold it".
func (m *Memories) Held(ctx context.Context, ids []string) (map[string]bool, error) {
	log.Trace("func() client.Memories.Held")

	held := make(map[string]bool, len(ids))

	for start := 0; start < len(ids); start += explainChunkSize {
		end := min(start+explainChunkSize, len(ids))

		if err := m.heldChunk(ctx, ids[start:end], held); err != nil {
			return nil, err
		}
	}

	return held, nil
}

func (m *Memories) heldChunk(ctx context.Context, ids []string, held map[string]bool) error {
	ctx, cancel := m.callContext(ctx)
	defer cancel()

	resp, err := m.client.ExplainConsolidation(ctx, &contract.ExplainConsolidationRequest{MemoryIds: ids})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return ErrConsolidationDisabled
		}

		return fmt.Errorf("asking about %d memories: %w", len(ids), err)
	}

	for _, v := range resp.GetValuations() {
		held[v.GetId()] = true
	}

	return nil
}

// Forgotten walks the forgotten log from newest to oldest, stopping once it reaches records older
// than since, and calls fn with each page. It reports whether the log is being recorded at all,
// which is what separates "nothing has been forgotten" from "nothing is being written down" - an
// ambiguity that would otherwise leave a catch-up pass reporting a clean bill of health for a
// deployment where the catch-up path does not exist.
func (m *Memories) Forgotten(
	ctx context.Context,
	since time.Time,
	fn func([]*contract.ForgottenMemory) error,
) (bool, error) {
	log.Trace("func() client.Memories.Forgotten")

	request := &contract.GetForgottenMemoriesRequest{
		Since: since.UnixNano(),
		Limit: forgottenPageSize,
	}

	enabled := false

	for {
		page, err := m.forgottenPage(ctx, request)
		if err != nil {
			return enabled, err
		}

		enabled = page.GetEnabled()

		if len(page.GetMemories()) > 0 {
			if err := fn(page.GetMemories()); err != nil {
				return enabled, err
			}
		}

		// next_seq is 0 on the last page, which is also what an empty log answers - so this is the
		// one termination condition and it covers both.
		if page.GetNextSeq() == 0 {
			return enabled, nil
		}

		request.AfterSeq = page.GetNextSeq()
	}
}

func (m *Memories) forgottenPage(
	ctx context.Context,
	request *contract.GetForgottenMemoriesRequest,
) (*contract.GetForgottenMemoriesResponse, error) {
	ctx, cancel := m.callContext(ctx)
	defer cancel()

	page, err := m.client.GetForgottenMemories(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("reading the forgotten log: %w", err)
	}

	return page, nil
}

// callContext applies the per-call timeout. The returned cancel is always non-nil, so a caller can
// defer it unconditionally.
func (m *Memories) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.callTimeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, m.callTimeout)
}
