package bridge

import (
	"context"
	"fmt"
	"time"

	"github.com/fastbean-au/hippocampus/observability"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// hippocampusClient is the narrow slice of the generated Hippocampus client the Store needs, so
// tests can substitute a fake. contract.HippocampusClient satisfies it.
//
// It names six RPCs and no more, deliberately: this interface IS the module's statement of what a
// bridge is permitted to do to a store. A bridge writes memories, opens the events they belong to,
// relates them to each other, reinforces what an upstream stream tells it was engaged with, honours
// an upstream deletion, and seeds a store from an upstream that already has history. It does not
// list, search, export, summarise, or clear. Dial hands back the whole generated client, so this declaration is the only
// thing standing between an adapter and Purge - widening it wants a reason in the commit message.
//
// ImportBatch is the one that deserves its reason here. It is a full-state upsert that carries
// recall history, which StoreMemory deliberately refuses to (a fresh memory is never pre-reinforced,
// or a create could arrive already unforgettable). A bridge reading a source that reports engagement
// it did not witness - a ranked feed that hands back a post's like count, say - has no other way to
// express "this was returned to N times", and the alternative encodings all lie: significance says
// the author mattered more, and issuing N recalls both resets the decay clock to now and hammers the
// service. It is TierWriter, the same tier StoreMemory already needs, so it grants no new privilege.
type hippocampusClient interface {
	StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error)
	StoreEvent(ctx context.Context, in *contract.Event, opts ...grpc.CallOption) (*contract.StoreEventResponse, error)
	RecallMemories(ctx context.Context, in *contract.RecallMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	ImportBatch(ctx context.Context, in *contract.ImportBatchRequest, opts ...grpc.CallOption) (*contract.ImportBatchResponse, error)
	LinkMemories(ctx context.Context, in *contract.LinkMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
}

// Store turns delivered broker messages into Hippocampus memories and writes them over gRPC. It is
// the piece every broker adapter shares: an adapter's consume loop normalises each native delivery
// into a Message and calls Handle, using Handle's error return to decide whether to ack or
// nack/redeliver the delivery.
type Store struct {
	client      hippocampusClient
	transformer Transformer
	callTimeout time.Duration

	// broker names which adapter this Store serves, and is the one attribute that distinguishes the
	// four bridges' metrics when several ship to the same collector.
	broker string
}

// NewStore builds a Store from a Hippocampus client, a Transformer, and a per-message call timeout
// (<= 0 means no per-call bound beyond the caller's context). The broker name is stamped on every
// metric this Store records; an empty one reports "unknown" rather than an empty label, which is
// harder to spot in a query.
func NewStore(client hippocampusClient, transformer Transformer, callTimeout time.Duration, broker string) *Store {
	if broker == "" {
		broker = "unknown"
	}

	return &Store{
		client:      client,
		transformer: transformer,
		callTimeout: callTimeout,
		broker:      broker,
	}
}

// Transformer returns the Transformer this Store was built with.
//
// It exists for an adapter with a SECOND source of messages - one that does not arrive over the
// broker - so it can map them through the very same instance rather than a separately-configured
// one that is merely meant to match. The Bluesky adapter's curated-feed mode is the case: a post
// read over HTTP must be filtered, bounded and clamped exactly as one read off the firehose, and
// sharing the instance is what makes that true by construction instead of by review.
func (s *Store) Transformer() Transformer {
	return s.transformer
}

// Handle transforms one message and stores every memory it yields. It returns an error only when a
// delivery could not be durably handled - a Transformer failure or a store transport error - so the
// caller can nack/redeliver; a memory dropped for significance below the service's threshold
// (StoreMemoryResponse.Rejected) is a successful outcome, logged at debug and not an error. A
// Transformer that yields no memories is a successful no-op (the message is intentionally dropped).
func (s *Store) Handle(ctx context.Context, msg Message) error {
	log.Trace("Store.Handle()")

	start := time.Now()

	mems, err := s.transformer.Transform(msg)
	if err != nil {
		s.record(ctx, OutcomeFailed, start)

		return fmt.Errorf("transforming message on subject %q: %w", msg.Subject, err)
	}

	// A Transformer yielding nothing filtered the message deliberately, which is a different thing
	// from one that failed and a different thing again from one the service declined - hence three
	// non-failure outcomes rather than a success bool. See the outcome constants.
	if len(mems) == 0 {
		s.record(ctx, OutcomeFiltered, start)

		return nil
	}

	// The message's outcome is the WORST of its memories': a message yielding three memories of
	// which one failed has not been durably handled, and the adapter is about to redeliver it.
	outcome := OutcomeStored

	for i, v := range mems {
		if v == nil {
			continue
		}

		stored, err := s.store(ctx, v)
		if err != nil {
			s.record(ctx, OutcomeFailed, start)

			return fmt.Errorf("storing memory %d of %d from subject %q: %w", i+1, len(mems), msg.Subject, err)
		}

		if !stored {
			outcome = OutcomeRejected
		}
	}

	s.record(ctx, outcome, start)

	return nil
}

// record reports one message's outcome and how long handling it took.
func (s *Store) record(ctx context.Context, outcome string, start time.Time) {
	attrs := observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, outcome),
	)

	tel.messages.Add(ctx, 1, attrs)
	tel.storeDuration.Record(ctx, time.Since(start).Seconds(), attrs)
}

// store issues one StoreMemory RPC, bounding it by callTimeout when configured. The bool reports
// whether the memory was RETAINED: false means the service declined it for insignificance, which is
// a success for the caller (nothing to redeliver) but a different outcome to report.
func (s *Store) store(ctx context.Context, mem *contract.Memory) (bool, error) {
	if s.callTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, s.callTimeout)
		defer cancel()
	}

	resp, err := s.client.StoreMemory(ctx, mem)
	if err != nil {
		return false, err
	}

	attrs := observability.WithGroup(attribute.String(attrBroker, s.broker))

	if resp.GetRejected() {
		log.WithFields(log.Fields{
			"significance": mem.GetSignificance(),
			"group":        mem.GetGroup(),
		}).
			Debug("memory dropped below minimum significance")

		tel.memories.Add(ctx, 1, observability.WithGroup(
			attribute.String(attrBroker, s.broker),
			attribute.String(attrOutcome, OutcomeRejected),
		))

		return false, nil
	}

	log.WithFields(log.Fields{
		"id":    resp.GetId(),
		"group": mem.GetGroup(),
	}).
		Trace("stored memory")

	tel.memories.Add(ctx, 1, observability.WithGroup(
		attribute.String(attrBroker, s.broker),
		attribute.String(attrOutcome, OutcomeStored),
	))

	// Mirrors the service's own memory.body_bytes, so a bridge's write-size distribution can be
	// compared with what the store ended up holding.
	tel.bodyBytes.Record(ctx, int64(len(mem.GetBody())), attrs)

	return true, nil
}
