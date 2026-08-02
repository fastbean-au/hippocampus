package rabbitmq

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// okStorer satisfies the (unexported) storer bridge.NewStore accepts, structurally.
type okStorer struct{}

func (okStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

// fakeAck records ack/nack calls so the handle routing can be asserted without a broker. It is
// mutex-guarded because a Run test reads the counters from a watcher goroutine while the consume
// loop writes them.
type fakeAck struct {
	mu      sync.Mutex
	acks    int
	nacks   int
	requeue bool
}

func (f *fakeAck) Ack(tag uint64, multiple bool) error {
	f.mu.Lock()
	f.acks++
	f.mu.Unlock()

	return nil
}

func (f *fakeAck) Nack(tag uint64, multiple bool, requeue bool) error {
	f.mu.Lock()
	f.nacks++
	f.requeue = requeue
	f.mu.Unlock()

	return nil
}

func (f *fakeAck) Reject(tag uint64, requeue bool) error {
	return nil
}

func (f *fakeAck) counts() (int, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.acks, f.nacks, f.requeue
}

func TestToMessage(t *testing.T) {
	ts := time.Unix(1_700_000_000, 0)
	d := amqp.Delivery{
		RoutingKey:  "orders.created",
		Body:        []byte("payload"),
		Timestamp:   ts,
		Type:        "OrderCreated",
		AppId:       "checkout",
		ContentType: "application/json",
		Headers:     amqp.Table{"x-sig": int32(5), "tenant": "acme"},
	}

	got := toMessage(d)

	if got.Subject != "orders.created" {
		t.Errorf("subject = %q, want orders.created", got.Subject)
	}

	if string(got.Data) != "payload" {
		t.Errorf("data = %q", string(got.Data))
	}

	if !got.Timestamp.Equal(ts) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, ts)
	}

	if got.Headers["x-sig"] != "5" {
		t.Errorf("header x-sig = %q, want 5", got.Headers["x-sig"])
	}

	if got.Headers["type"] != "OrderCreated" || got.Headers["app_id"] != "checkout" || got.Headers["content_type"] != "application/json" {
		t.Errorf("well-known property headers not mapped: %#v", got.Headers)
	}
}

func TestHandle_AcksOnSuccess(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{RequeueOnError: true}, store)

	ack := &fakeAck{}
	d := amqp.Delivery{Acknowledger: ack, RoutingKey: "s", Body: []byte("x")}

	b.handle(context.Background(), d)

	if acks, nacks, _ := ack.counts(); acks != 1 || nacks != 0 {
		t.Errorf("acks=%d nacks=%d, want 1/0", acks, nacks)
	}
}

func TestHandle_NacksWithRequeueOnFailure(t *testing.T) {
	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0)
	b := New(Config{RequeueOnError: true}, store)

	ack := &fakeAck{}
	d := amqp.Delivery{Acknowledger: ack, RoutingKey: "s", Body: []byte("x")}

	b.handle(context.Background(), d)

	acks, nacks, requeue := ack.counts()

	if acks != 0 || nacks != 1 {
		t.Fatalf("acks=%d nacks=%d, want 0/1", acks, nacks)
	}

	if !requeue {
		t.Errorf("requeue = false, want true when RequeueOnError is set")
	}
}

func TestConsume_StopsOnContextCancel(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{}, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.consume(ctx, make(chan amqp.Delivery)); err != nil {
		t.Errorf("consume on cancelled context = %v, want nil", err)
	}
}

func TestConsume_ErrorsWhenChannelCloses(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{}, store)

	ch := make(chan amqp.Delivery)
	close(ch)

	if err := b.consume(context.Background(), ch); err == nil {
		t.Errorf("consume on closed channel = nil, want an error")
	}
}

// fakeChannel implements amqpChannel with scripted behaviour.
type fakeChannel struct {
	qosErr       error
	declareErr   error
	consumeErr   error
	declared     bool
	deliveries   chan amqp.Delivery
	closed       bool
	consumeQueue string
}

func (c *fakeChannel) Qos(prefetchCount int, prefetchSize int, global bool) error {
	return c.qosErr
}

func (c *fakeChannel) QueueDeclare(name string, durable bool, autoDelete bool, exclusive bool, noWait bool, args amqp.Table) (amqp.Queue, error) {
	c.declared = true

	return amqp.Queue{Name: name}, c.declareErr
}

func (c *fakeChannel) Consume(queue string, consumer string, autoAck bool, exclusive bool, noLocal bool, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error) {
	c.consumeQueue = queue
	if c.consumeErr != nil {
		return nil, c.consumeErr
	}

	return c.deliveries, nil
}

func (c *fakeChannel) Close() error {
	c.closed = true

	return nil
}

// fakeConn implements amqpConn.
type fakeConn struct {
	ch         *fakeChannel
	channelErr error
	closed     bool
}

func (c *fakeConn) Channel() (amqpChannel, error) {
	if c.channelErr != nil {
		return nil, c.channelErr
	}

	return c.ch, nil
}

func (c *fakeConn) Close() error {
	c.closed = true

	return nil
}

func newRunBridge(t *testing.T, cfg Config, conn *fakeConn, dialErr error) *Bridge {
	t.Helper()

	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(cfg, store)
	b.dial = func(url string) (amqpConn, error) {
		if dialErr != nil {
			return nil, dialErr
		}

		return conn, nil
	}

	return b
}

func TestRun_HappyPathConsumesAndCancels(t *testing.T) {
	deliveries := make(chan amqp.Delivery, 1)
	ack := &fakeAck{}
	deliveries <- amqp.Delivery{Acknowledger: ack, RoutingKey: "s", Body: []byte("x")}

	ch := &fakeChannel{deliveries: deliveries}
	conn := &fakeConn{ch: ch}

	b := newRunBridge(t, Config{Queue: "events", Prefetch: 5, DeclareQueue: true}, conn, nil)

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		// Give the loop time to process the queued delivery, then stop.
		for {
			if acks, _, _ := ack.counts(); acks > 0 {
				break
			}

			time.Sleep(5 * time.Millisecond)
		}

		cancel()
	}()

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !ch.declared {
		t.Errorf("expected the queue to be declared when DeclareQueue is set")
	}

	if ch.consumeQueue != "events" {
		t.Errorf("consumed queue = %q, want events", ch.consumeQueue)
	}

	if acks, _, _ := ack.counts(); acks != 1 {
		t.Errorf("acks = %d, want 1", acks)
	}

	if !conn.closed || !ch.closed {
		t.Errorf("connection/channel should be closed on shutdown (conn=%v ch=%v)", conn.closed, ch.closed)
	}
}

func TestRun_DialError(t *testing.T) {
	b := newRunBridge(t, Config{Queue: "q"}, nil, errors.New("dial failed"))

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should surface the dial error")
	}
}

func TestRun_ChannelError(t *testing.T) {
	conn := &fakeConn{channelErr: errors.New("no channel")}
	b := newRunBridge(t, Config{Queue: "q"}, conn, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should surface the channel error")
	}
}

func TestRun_QosError(t *testing.T) {
	conn := &fakeConn{ch: &fakeChannel{qosErr: errors.New("qos failed")}}
	b := newRunBridge(t, Config{Queue: "q"}, conn, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should surface the QoS error")
	}
}

func TestRun_DeclareError(t *testing.T) {
	conn := &fakeConn{ch: &fakeChannel{declareErr: errors.New("declare failed")}}
	b := newRunBridge(t, Config{Queue: "q", DeclareQueue: true}, conn, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should surface the declare error")
	}
}

func TestRun_ConsumeError(t *testing.T) {
	conn := &fakeConn{ch: &fakeChannel{consumeErr: errors.New("consume failed")}}
	b := newRunBridge(t, Config{Queue: "q"}, conn, nil)

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should surface the consume error")
	}
}

func TestHandle_AckErrorLogged(t *testing.T) {
	// A delivery with a nil Acknowledger makes Ack() error; handle must not panic.
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{}, store)

	b.handle(context.Background(), amqp.Delivery{RoutingKey: "s", Body: []byte("x")})
}

func TestDefaultDial_ErrorOnBadURL(t *testing.T) {
	if _, err := defaultDial("amqp://guest:guest@127.0.0.1:1/"); err == nil {
		t.Errorf("defaultDial to a dead address should error")
	}
}

func TestHandle_NackErrorLogged(t *testing.T) {
	// Store fails (failing transformer) AND the delivery has a nil Acknowledger, so Nack() errors:
	// handle must log and not panic, exercising the nack-error branch.
	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0)
	b := New(Config{RequeueOnError: true}, store)

	b.handle(context.Background(), amqp.Delivery{RoutingKey: "s", Body: []byte("x")})
}
