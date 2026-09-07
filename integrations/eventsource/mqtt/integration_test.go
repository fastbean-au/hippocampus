package mqtt

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// countingStorer counts StoreMemory calls for the integration tests. It embeds the generated
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

// TestIntegration_RealBroker exercises the real paho connect path (defaultConnect) end to end against
// a broker. It skips unless HIPPOCAMPUS_TEST_MQTT_BROKER names one (e.g. tcp://localhost:1883).
func TestIntegration_RealBroker(t *testing.T) {
	broker := os.Getenv("HIPPOCAMPUS_TEST_MQTT_BROKER")
	if broker == "" {
		t.Skip("set HIPPOCAMPUS_TEST_MQTT_BROKER to run the MQTT integration test")
	}

	storer := &countingStorer{}
	store := bridge.NewStore(storer, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")

	b := New(Config{
		Broker:       broker,
		Topic:        "hippo/test",
		ClientID:     "hippo-eventsource-test-sub",
		QoS:          1,
		CleanSession: true,
	}, store)

	ctx, cancel := context.WithCancel(context.Background())

	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	pub := pahomqtt.NewClient(pahomqtt.NewClientOptions().AddBroker(broker).SetClientID("hippo-eventsource-test-pub"))
	if token := pub.Connect(); token.Wait() && token.Error() != nil {
		t.Fatalf("publisher connect: %v", token.Error())
	}

	defer pub.Disconnect(100)

	deadline := time.After(10 * time.Second)

	for storer.count() == 0 {
		pub.Publish("hippo/test", 1, false, []byte("hello")).Wait()

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

// StoreMemories delegates to StoreMemory so a fake answers a batch write exactly as it answers the
// single one, and every existing expectation holds through the batch path. A gRPC status error
// becomes that memory's own result, as the service would report it; anything else is a transport
// failure and fails the call.
func (s *countingStorer) StoreMemories(ctx context.Context, in *contract.StoreMemoriesRequest, opts ...grpc.CallOption) (*contract.StoreMemoriesResponse, error) {
	res := &contract.StoreMemoriesResponse{}

	for _, m := range in.GetMemories() {
		resp, err := s.StoreMemory(ctx, m, opts...)

		switch {

		case err != nil:
			st, ok := status.FromError(err)
			if !ok {
				return nil, err
			}

			res.Failed++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Code: int32(st.Code()), Error: st.Message()})

		case resp.GetRejected():
			res.Rejected++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Rejected: true})

		default:
			res.Stored++

			res.Results = append(res.Results, &contract.StoreMemoryResult{Id: resp.GetId()})

		}
	}

	return res, nil
}
