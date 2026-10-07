package kafka

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// lockedStorer records the body of every memory stored, safely across the reader's goroutines.
type lockedStorer struct {
	contract.HippocampusClient

	mu     sync.Mutex
	bodies []string
}

func (s *lockedStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.bodies = append(s.bodies, in.GetBody())

	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

func (s *lockedStorer) stored() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.bodies...)
}

// TestIntegration_RealBroker runs the real kafka-go reader against a broker (TODO-3 item 177): the
// adapter's fakes model kafka-go's semantics, and this checks the model against the thing. It skips
// unless HIPPOCAMPUS_TEST_KAFKA_BROKERS names one (e.g. localhost:9092).
//
// Three things are checked, in the order a deployment depends on them: messages published to the
// topic are stored; the group's committed offset is past them once they are, which is what
// at-least-once delivery rests on; and a bridge restarted under the same group resumes from that
// offset - storing the next message first, rather than re-reading the topic from the start.
func TestIntegration_RealBroker(t *testing.T) {
	brokersSetting := os.Getenv("HIPPOCAMPUS_TEST_KAFKA_BROKERS")
	if brokersSetting == "" {
		t.Skip("set HIPPOCAMPUS_TEST_KAFKA_BROKERS to run the Kafka integration test")
	}

	brokers := strings.Split(brokersSetting, ",")
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	topic := "hippo-eventsource-test-" + suffix
	group := "hippo-eventsource-test-" + suffix

	createTopic(t, brokers[0], topic)

	writer := &kafkago.Writer{Addr: kafkago.TCP(brokers...), Topic: topic}

	defer func() { _ = writer.Close() }()

	publish(t, writer, "a", "b", "c")

	first := &lockedStorer{}
	runUntil(t, Config{Brokers: brokers, Topic: topic, GroupID: group}, first, 3)

	if got := first.stored(); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("stored %v, want the three published messages in order", got)
	}

	client := &kafkago.Client{Addr: kafkago.TCP(brokers...), Timeout: 10 * time.Second}

	offsets, err := client.OffsetFetch(context.Background(), &kafkago.OffsetFetchRequest{
		GroupID: group,
		Topics:  map[string][]int{topic: {0}},
	})
	if err != nil {
		t.Fatalf("OffsetFetch: %s", err)
	}

	if committed := offsets.Topics[topic][0].CommittedOffset; committed != 3 {
		t.Errorf("committed offset %d after storing three messages, want 3", committed)
	}

	publish(t, writer, "d")

	second := &lockedStorer{}
	runUntil(t, Config{Brokers: brokers, Topic: topic, GroupID: group}, second, 1)

	if got := second.stored(); got[0] != "d" {
		t.Errorf("a restarted bridge stored %v first, want the next message after the committed offset", got)
	}
}

// createTopic creates a one-partition topic on the cluster's controller, so the test does not depend
// on the broker auto-creating topics.
func createTopic(t *testing.T, broker string, topic string) {
	t.Helper()

	conn, err := kafkago.Dial("tcp", broker)
	if err != nil {
		t.Fatalf("dialling %s: %s", broker, err)
	}

	defer func() { _ = conn.Close() }()

	controller, err := conn.Controller()
	if err != nil {
		t.Fatalf("finding the controller: %s", err)
	}

	controllerConn, err := kafkago.Dial("tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		t.Fatalf("dialling the controller: %s", err)
	}

	defer func() { _ = controllerConn.Close() }()

	if err := controllerConn.CreateTopics(kafkago.TopicConfig{Topic: topic, NumPartitions: 1, ReplicationFactor: 1}); err != nil {
		t.Fatalf("creating topic %s: %s", topic, err)
	}
}

// publish writes the given values to the topic, one message each, in order.
func publish(t *testing.T, writer *kafkago.Writer, values ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	messages := make([]kafkago.Message, 0, len(values))

	for _, v := range values {
		messages = append(messages, kafkago.Message{Value: []byte(v)})
	}

	if err := writer.WriteMessages(ctx, messages...); err != nil {
		t.Fatalf("publishing %v: %s", values, err)
	}
}

// runUntil runs a bridge until it has stored want messages, then stops it and checks it stopped
// cleanly. A group join takes a few seconds on a fresh broker, hence the generous deadline.
func runUntil(t *testing.T, cfg Config, storer *lockedStorer, want int) {
	t.Helper()

	store := bridge.NewStore(storer, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	b := New(cfg, store)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)

	go func() { runErr <- b.Run(ctx) }()

	deadline := time.Now().Add(60 * time.Second)

	for len(storer.stored()) < want {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("stored %d of %d messages within the deadline", len(storer.stored()), want)
		}

		time.Sleep(50 * time.Millisecond)
	}

	cancel()

	if err := <-runErr; err != nil {
		t.Errorf("Run returned %v", fmt.Errorf("after storing: %w", err))
	}
}
