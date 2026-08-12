package nats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	natstest "github.com/nats-io/nats-server/v2/test"
	natsgo "github.com/nats-io/nats.go"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// okStorer satisfies bridge.NewStore's client seam by embedding the generated interface and
// overriding only StoreMemory, so widening that seam never touches this file. The embedded value is
// nil: a call to any other RPC panics, which is the assertion this adapter wants.
type okStorer struct {
	contract.HippocampusClient

	mu    sync.Mutex
	calls int
}

func (s *okStorer) StoreMemory(ctx context.Context, in *contract.Memory, opts ...grpc.CallOption) (*contract.StoreMemoryResponse, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()

	return &contract.StoreMemoryResponse{Id: "x"}, nil
}

func (s *okStorer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// fakeSub records Unsubscribe.
type fakeSub struct{ unsubscribed bool }

func (f *fakeSub) Unsubscribe() error {
	f.unsubscribed = true

	return nil
}

// fakeConn implements natsConn, capturing the subscription and optionally invoking the handler.
type fakeConn struct {
	subject   string
	queue     string
	handler   natsgo.MsgHandler
	sub       *fakeSub
	drained   bool
	subErr    error
	deliver   *natsgo.Msg
	queueMode bool
}

func (c *fakeConn) Subscribe(subject string, cb natsgo.MsgHandler) (natsSub, error) {
	c.subject = subject
	c.handler = cb

	return c.finish()
}

func (c *fakeConn) QueueSubscribe(subject string, queue string, cb natsgo.MsgHandler) (natsSub, error) {
	c.subject = subject
	c.queue = queue
	c.queueMode = true
	c.handler = cb

	return c.finish()
}

func (c *fakeConn) finish() (natsSub, error) {
	if c.subErr != nil {
		return nil, c.subErr
	}

	c.sub = &fakeSub{}

	if c.deliver != nil {
		c.handler(c.deliver)
	}

	return c.sub, nil
}

func (c *fakeConn) Drain() error {
	c.drained = true

	return nil
}

func TestToMessage(t *testing.T) {
	m := &natsgo.Msg{
		Subject: "orders.created",
		Data:    []byte("payload"),
		Header: natsgo.Header{
			"X-Sig": []string{"5"},
			"Empty": {},
			"Multi": []string{"a", "b"},
		},
	}

	got := toMessage(m)

	if got.Subject != "orders.created" {
		t.Errorf("subject = %q, want orders.created", got.Subject)
	}

	if string(got.Data) != "payload" {
		t.Errorf("data = %q, want payload", string(got.Data))
	}

	if got.Headers["X-Sig"] != "5" {
		t.Errorf("header X-Sig = %q, want 5", got.Headers["X-Sig"])
	}

	if _, ok := got.Headers["Empty"]; ok {
		t.Errorf("empty header should be omitted, got %q", got.Headers["Empty"])
	}

	if got.Headers["Multi"] != "b" {
		t.Errorf("multi-valued header = %q, want last value b", got.Headers["Multi"])
	}

	if !got.Timestamp.IsZero() {
		t.Errorf("timestamp should be zero for core NATS, got %v", got.Timestamp)
	}
}

func TestToMessage_NoHeaders(t *testing.T) {
	got := toMessage(&natsgo.Msg{Subject: "s", Data: []byte("d")})

	if got.Headers != nil {
		t.Errorf("headers = %v, want nil when the message has none", got.Headers)
	}
}

func TestRun_SubscribeDeliversToStore(t *testing.T) {
	storer := &okStorer{}
	store := bridge.NewStore(storer, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")

	fc := &fakeConn{deliver: &natsgo.Msg{Subject: "events.a", Data: []byte("hello")}}

	b := New(Config{Subject: "events.>"}, store)
	b.connect = func(url string, opts ...natsgo.Option) (natsConn, error) {
		return fc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if fc.subject != "events.>" || fc.queueMode {
		t.Errorf("expected plain Subscribe on events.>, got subject=%q queueMode=%v", fc.subject, fc.queueMode)
	}

	if storer.count() != 1 {
		t.Errorf("store calls = %d, want 1 (delivered message)", storer.count())
	}

	if !fc.drained || !fc.sub.unsubscribed {
		t.Errorf("expected drain=%v and unsubscribe=%v on shutdown", fc.drained, fc.sub.unsubscribed)
	}
}

func TestRun_QueueSubscribeWhenQueueSet(t *testing.T) {
	store := bridge.NewStore(&okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	fc := &fakeConn{}

	b := New(Config{Subject: "s", Queue: "workers"}, store)
	b.connect = func(url string, opts ...natsgo.Option) (natsConn, error) {
		return fc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !fc.queueMode || fc.queue != "workers" {
		t.Errorf("expected QueueSubscribe with queue=workers, got queueMode=%v queue=%q", fc.queueMode, fc.queue)
	}
}

func TestRun_ConnectError(t *testing.T) {
	store := bridge.NewStore(&okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")

	b := New(Config{Subject: "s"}, store)
	b.connect = func(url string, opts ...natsgo.Option) (natsConn, error) {
		return nil, errors.New("no server")
	}

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should return the connect error")
	}
}

func TestRun_SubscribeError(t *testing.T) {
	store := bridge.NewStore(&okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	fc := &fakeConn{subErr: errors.New("subscribe failed")}

	b := New(Config{Subject: "s"}, store)
	b.connect = func(url string, opts ...natsgo.Option) (natsConn, error) {
		return fc, nil
	}

	if err := b.Run(context.Background()); err == nil {
		t.Errorf("Run should return the subscribe error")
	}
}

func TestRun_AppliesAllConnectionOptions(t *testing.T) {
	store := bridge.NewStore(&okStorer{}, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")
	fc := &fakeConn{}

	var gotURL string
	var gotOpts int

	b := New(Config{
		URL:             "",
		Subject:         "s",
		Name:            "conn",
		CredentialsFile: "/tmp/x.creds",
		Token:           "tok",
		Username:        "u",
		Password:        "p",
	}, store)
	b.connect = func(url string, opts ...natsgo.Option) (natsConn, error) {
		gotURL = url
		gotOpts = len(opts)

		return fc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if gotURL != natsgo.DefaultURL {
		t.Errorf("empty URL should default to %q, got %q", natsgo.DefaultURL, gotURL)
	}

	// Name + creds + token + userinfo = 4 options.
	if gotOpts != 4 {
		t.Errorf("expected 4 connection options, got %d", gotOpts)
	}
}

func TestDefaultConnect_ErrorFastOnBadURL(t *testing.T) {
	// nats.Connect with no server and no retry returns quickly; the success branch needs a live
	// server and is covered by the manual end-to-end run.
	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, err := defaultConnect("nats://127.0.0.1:1"); err == nil {
			t.Errorf("defaultConnect to a dead address should error")
		}
	}()

	select {

	case <-done:

	case <-time.After(5 * time.Second):
		t.Fatalf("defaultConnect did not return promptly")
	}
}

func TestRun_EmbeddedServerRealConnect(t *testing.T) {
	opts := natstest.DefaultTestOptions
	opts.Port = -1

	srv := natstest.RunServer(&opts)
	defer srv.Shutdown()

	url := srv.ClientURL()

	for _, queue := range []string{"", "workers"} {
		t.Run("queue="+queue, func(t *testing.T) {
			storer := &okStorer{}
			store := bridge.NewStore(storer, bridge.NewDefaultTransformer(bridge.TransformConfig{}), 0, "test")

			b := New(Config{URL: url, Subject: "events.test", Queue: queue, Name: "test"}, store)

			ctx, cancel := context.WithCancel(context.Background())

			runErr := make(chan error, 1)

			go func() { runErr <- b.Run(ctx) }()

			// Publish once the subscription is live.
			pub, err := natsgo.Connect(url)
			if err != nil {
				t.Fatalf("publisher connect: %v", err)
			}

			defer pub.Close()

			deadline := time.After(5 * time.Second)

			for storer.count() == 0 {
				_ = pub.Publish("events.test", []byte("hello"))
				_ = pub.Flush()

				select {

				case <-deadline:
					t.Fatalf("message was not stored within the deadline")

				case <-time.After(50 * time.Millisecond):
				}
			}

			cancel()

			if err := <-runErr; err != nil {
				t.Errorf("Run returned %v", err)
			}
		})
	}
}
