package rabbitmq

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// countingStorer counts StoreMemory calls for the integration test. It embeds the generated
// interface so widening bridge.NewStore's client seam never touches this file; the embedded value is
// nil, so a call to any other RPC panics.
type countingStorer struct {
	contract.HippocampusClient

	mu    sync.Mutex
	calls int
}

func (s *countingStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

func (s *countingStorer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// TestIntegration_RealBroker exercises the real amqp.Dial path (defaultDial + realAmqpConn) end to
// end. It skips unless HIPPOCAMPUS_TEST_RABBITMQ_URL names a broker (e.g.
// amqp://guest:guest@localhost:5672/).
func TestIntegration_RealBroker(t *testing.T) {
	url := os.Getenv("HIPPOCAMPUS_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("set HIPPOCAMPUS_TEST_RABBITMQ_URL to run the RabbitMQ integration test")
	}

	const queue = "hippo-eventsource-test"

	storer := &countingStorer{}
	store := bridge.NewStore(storer, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")

	b := New(Config{URL: url, Queue: queue, DeclareQueue: true, RequeueOnError: true, Prefetch: 1}, store)

	ctx, cancel := context.WithCancel(context.Background())

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	// Publish through a separate connection, declaring the queue first so the message is not lost if
	// it arrives before the consumer declares it.
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("publisher dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("publisher channel: %v", err)
	}

	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declaring queue: %v", err)
	}

	deadline := time.After(10 * time.Second)

	for storer.count() == 0 {
		if err := ch.PublishWithContext(context.Background(), "", queue, false, false, amqp.Publishing{Body: []byte("hello")}); err != nil {
			t.Fatalf("publishing: %v", err)
		}

		select {

		case <-deadline:
			t.Fatalf("message was not stored within the deadline")

		case <-time.After(100 * time.Millisecond):
		}
	}

	cancel()

	if err := <-runErr; err != nil {
		t.Errorf("Run returned %v", err)
	}
}
