package mqtt

import (
	"context"
	"errors"
	"testing"
	"time"

	pahomqtt "github.com/eclipse/paho.mqtt.golang"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

type okStorer struct{}

func (okStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

// fakeMessage implements the paho mqtt.Message interface and records whether it was acked.
type fakeMessage struct {
	topic   string
	payload []byte
	acked   bool
}

func (f *fakeMessage) Duplicate() bool   { return false }
func (f *fakeMessage) Qos() byte         { return 1 }
func (f *fakeMessage) Retained() bool    { return false }
func (f *fakeMessage) Topic() string     { return f.topic }
func (f *fakeMessage) MessageID() uint16 { return 1 }
func (f *fakeMessage) Payload() []byte   { return f.payload }
func (f *fakeMessage) Ack()              { f.acked = true }

func TestToMessage(t *testing.T) {
	m := &fakeMessage{topic: "sensors/temp", payload: []byte("21.5")}

	got := toMessage(m)

	if got.Subject != "sensors/temp" {
		t.Errorf("subject = %q, want sensors/temp", got.Subject)
	}

	if string(got.Data) != "21.5" {
		t.Errorf("data = %q, want 21.5", string(got.Data))
	}

	if !got.Timestamp.IsZero() {
		t.Errorf("timestamp should be zero for MQTT, got %v", got.Timestamp)
	}
}

func TestMessageHandler_AcksOnSuccess(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	b := New(Config{}, store)

	m := &fakeMessage{topic: "t", payload: []byte("x")}
	b.messageHandler(context.Background())(nil, m)

	if !m.acked {
		t.Errorf("message should be acked after a successful store")
	}
}

func TestMessageHandler_DoesNotAckOnFailure(t *testing.T) {
	failing := bridge.TransformerFunc(func(msg bridge.Message) ([]*contract.Memory, error) {
		return nil, errors.New("boom")
	})

	store := bridge.NewStore(okStorer{}, failing, 0)
	b := New(Config{}, store)

	m := &fakeMessage{topic: "t", payload: []byte("x")}
	b.messageHandler(context.Background())(nil, m)

	if m.acked {
		t.Errorf("message should not be acked when the store fails")
	}
}

// fakeToken implements mqtt.Token.
type fakeToken struct{ err error }

func (t *fakeToken) Wait() bool                     { return true }
func (t *fakeToken) WaitTimeout(time.Duration) bool { return true }

func (t *fakeToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)

	return ch
}

func (t *fakeToken) Error() error { return t.err }

// fakeClient implements the paho mqtt.Client interface; only Subscribe/Disconnect are meaningful.
type fakeClient struct {
	subTopic     string
	subQoS       byte
	handler      pahomqtt.MessageHandler
	subToken     *fakeToken
	disconnected bool
}

func (c *fakeClient) IsConnected() bool      { return true }
func (c *fakeClient) IsConnectionOpen() bool { return true }
func (c *fakeClient) Connect() pahomqtt.Token {
	return &fakeToken{}
}

func (c *fakeClient) Disconnect(quiesce uint) { c.disconnected = true }

func (c *fakeClient) Publish(topic string, qos byte, retained bool, payload interface{}) pahomqtt.Token {
	return &fakeToken{}
}

func (c *fakeClient) Subscribe(topic string, qos byte, callback pahomqtt.MessageHandler) pahomqtt.Token {
	c.subTopic = topic
	c.subQoS = qos
	c.handler = callback
	if c.subToken != nil {
		return c.subToken
	}

	return &fakeToken{}
}

func (c *fakeClient) SubscribeMultiple(filters map[string]byte, callback pahomqtt.MessageHandler) pahomqtt.Token {
	return &fakeToken{}
}

func (c *fakeClient) Unsubscribe(topics ...string) pahomqtt.Token             { return &fakeToken{} }
func (c *fakeClient) AddRoute(topic string, callback pahomqtt.MessageHandler) {}
func (c *fakeClient) OptionsReader() pahomqtt.ClientOptionsReader {
	return pahomqtt.ClientOptionsReader{}
}

func TestRun_ConnectsSubscribesAndDisconnects(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	fc := &fakeClient{}

	b := New(Config{Broker: "tcp://x:1883", Topic: "sensors/#", QoS: 1, Username: "u", Password: "p"}, store)
	b.connect = func(opts *pahomqtt.ClientOptions) (pahomqtt.Client, error) {
		// Simulate paho invoking the OnConnect handler so the subscribe path runs.
		if opts.OnConnect != nil {
			opts.OnConnect(fc)
		}

		return fc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fc.subTopic != "sensors/#" || fc.subQoS != 1 {
		t.Errorf("subscribed topic/qos = %q/%d, want sensors/# / 1", fc.subTopic, fc.subQoS)
	}

	if !fc.disconnected {
		t.Errorf("client should be disconnected on shutdown")
	}
}

func TestRun_ConnectError(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)

	b := New(Config{Broker: "tcp://x:1883", Topic: "t"}, store)
	b.connect = func(opts *pahomqtt.ClientOptions) (pahomqtt.Client, error) {
		return nil, errors.New("connect failed")
	}

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should return the connect error")
	}
}

func TestRun_SubscribeErrorIsLoggedNotFatal(t *testing.T) {
	store := bridge.NewStore(okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0)
	fc := &fakeClient{subToken: &fakeToken{err: errors.New("subscribe failed")}}

	b := New(Config{Broker: "tcp://x:1883", Topic: "t"}, store)
	b.connect = func(opts *pahomqtt.ClientOptions) (pahomqtt.Client, error) {
		if opts.OnConnect != nil {
			opts.OnConnect(fc)
		}

		return fc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Run(ctx); err != nil {
		t.Errorf("Run should not fail on a subscribe error (logged), got %v", err)
	}
}
