package bridge

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// memoryStorer is the narrow slice of the generated Hippocampus client the Store needs, so tests can
// substitute a fake. contract.HippocampusClient satisfies it.
type memoryStorer interface {
	StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error)
}

// Store turns delivered broker messages into Hippocampus memories and writes them over gRPC. It is
// the piece every broker adapter shares: an adapter's consume loop normalises each native delivery
// into a Message and calls Handle, using Handle's error return to decide whether to ack or
// nack/redeliver the delivery.
type Store struct {
	client      memoryStorer
	transformer Transformer
	callTimeout time.Duration
}

// NewStore builds a Store from a Hippocampus client, a Transformer, and a per-message call timeout
// (<= 0 means no per-call bound beyond the caller's context).
func NewStore(client memoryStorer, transformer Transformer, callTimeout time.Duration) *Store {
	return &Store{
		client:      client,
		transformer: transformer,
		callTimeout: callTimeout,
	}
}

// Handle transforms one message and stores every memory it yields. It returns an error only when a
// delivery could not be durably handled - a Transformer failure or a store transport error - so the
// caller can nack/redeliver; a memory dropped for significance below the service's threshold
// (StoreMemoryResponse.Rejected) is a successful outcome, logged at debug and not an error. A
// Transformer that yields no memories is a successful no-op (the message is intentionally dropped).
func (s *Store) Handle(ctx context.Context, msg Message) error {
	log.Trace("Store.Handle()")

	mems, err := s.transformer.Transform(msg)
	if err != nil {
		return fmt.Errorf("transforming message on subject %q: %w", msg.Subject, err)
	}

	for i, v := range mems {
		if v == nil {
			continue
		}

		if err := s.store(ctx, v); err != nil {
			return fmt.Errorf("storing memory %d of %d from subject %q: %w", i+1, len(mems), msg.Subject, err)
		}
	}

	return nil
}

// store issues one StoreMemory RPC, bounding it by callTimeout when configured.
func (s *Store) store(ctx context.Context, mem *contract.Memory) error {
	if s.callTimeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, s.callTimeout)
		defer cancel()
	}

	resp, err := s.client.StoreMemory(ctx, mem)
	if err != nil {
		return err
	}

	if resp.GetRejected() {
		log.WithFields(log.Fields{
			"significance": mem.GetSignificance(),
			"group":        mem.GetGroup(),
		}).
			Debug("memory dropped below minimum significance")

		return nil
	}

	log.WithFields(log.Fields{
		"id":    resp.GetId(),
		"group": mem.GetGroup(),
	}).
		Trace("stored memory")

	return nil
}
