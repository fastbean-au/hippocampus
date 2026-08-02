// Package nats is the NATS broker adapter for the Hippocampus event-sourcing bridges. It subscribes
// to a subject (optionally as a queue-group member for load-balanced consumers) and hands every
// delivered message to a bridge.Store, which turns it into a memory. Core NATS delivery is
// at-most-once by nature - there is no per-message ack - so a store failure is logged and the
// message is not redelivered; use a queue group plus multiple bridge instances for throughput, or
// front the bridge with JetStream if you need at-least-once.
package nats

import (
	"context"
	"fmt"

	nats "github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// Config describes the NATS connection and subscription.
type Config struct {
	// URL is the NATS server URL (e.g. nats://localhost:4222). Empty uses nats.DefaultURL.
	URL string

	// Subject is the subject to subscribe to (supports NATS wildcards).
	Subject string

	// Queue, when set, joins a queue group so the subject's messages are load-balanced across every
	// bridge instance sharing the group name.
	Queue string

	// Name is the connection name reported to the server, for observability.
	Name string

	// CredentialsFile is an optional NATS credentials (.creds) file for decentralised auth.
	CredentialsFile string

	// Token is an optional server auth token.
	Token string

	// Username and Password are optional user/password credentials.
	Username string
	Password string
}

// natsConn is the slice of *nats.Conn the Bridge uses, so Run can be exercised with a fake. A
// subscription is returned as natsSub (satisfied by *nats.Subscription) so the fake need not build a
// concrete subscription.
type natsConn interface {
	Subscribe(subject string, cb nats.MsgHandler) (natsSub, error)
	QueueSubscribe(subject string, queue string, cb nats.MsgHandler) (natsSub, error)
	Drain() error
}

// natsSub is the subscription handle the Bridge holds; *nats.Subscription satisfies it.
type natsSub interface {
	Unsubscribe() error
}

// realNatsConn adapts *nats.Conn onto natsConn, wrapping each returned *nats.Subscription as a
// natsSub.
type realNatsConn struct {
	nc *nats.Conn
}

func (c realNatsConn) Subscribe(subject string, cb nats.MsgHandler) (natsSub, error) {
	return c.nc.Subscribe(subject, cb)
}

func (c realNatsConn) QueueSubscribe(subject string, queue string, cb nats.MsgHandler) (natsSub, error) {
	return c.nc.QueueSubscribe(subject, queue, cb)
}

func (c realNatsConn) Drain() error {
	return c.nc.Drain()
}

// Bridge consumes a NATS subject into a bridge.Store.
type Bridge struct {
	cfg   Config
	store *bridge.Store

	// connect builds the connection; overridable in tests.
	connect func(url string, opts ...nats.Option) (natsConn, error)
}

// New builds a NATS Bridge from its config and the shared store.
func New(cfg Config, store *bridge.Store) *Bridge {
	return &Bridge{
		cfg:     cfg,
		store:   store,
		connect: defaultConnect,
	}
}

// defaultConnect dials a real NATS server and adapts the connection onto natsConn.
func defaultConnect(url string, opts ...nats.Option) (natsConn, error) {
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, err
	}

	return realNatsConn{nc: nc}, nil
}

// Run connects, subscribes, and blocks handing each delivered message to the store until ctx is
// cancelled, then drains the connection so in-flight handlers finish before returning.
func (b *Bridge) Run(ctx context.Context) error {
	log.Trace("nats.Bridge.Run()")

	url := b.cfg.URL
	if url == "" {
		url = nats.DefaultURL
	}

	opts := []nats.Option{}

	if b.cfg.Name != "" {
		opts = append(opts, nats.Name(b.cfg.Name))
	}

	if b.cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(b.cfg.CredentialsFile))
	}

	if b.cfg.Token != "" {
		opts = append(opts, nats.Token(b.cfg.Token))
	}

	if b.cfg.Username != "" {
		opts = append(opts, nats.UserInfo(b.cfg.Username, b.cfg.Password))
	}

	nc, err := b.connect(url, opts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS at %q: %w", url, err)
	}

	defer func() { _ = nc.Drain() }()

	// The handler runs on the subscription's own goroutine; a store failure is at-most-once, so it
	// is logged and the message is dropped (core NATS cannot redeliver).
	handler := func(m *nats.Msg) {
		if err := b.store.Handle(ctx, toMessage(m)); err != nil {
			log.WithError(err).WithField("subject", m.Subject).
				Warn("storing NATS message failed; message dropped (core NATS is at-most-once)")
		}
	}

	var sub natsSub

	if b.cfg.Queue != "" {
		sub, err = nc.QueueSubscribe(b.cfg.Subject, b.cfg.Queue, handler)
	} else {
		sub, err = nc.Subscribe(b.cfg.Subject, handler)
	}

	if err != nil {
		return fmt.Errorf("subscribing to subject %q: %w", b.cfg.Subject, err)
	}

	defer func() { _ = sub.Unsubscribe() }()

	log.WithFields(log.Fields{
		"url":     url,
		"subject": b.cfg.Subject,
		"queue":   b.cfg.Queue,
	}).
		Info("NATS bridge subscribed")

	<-ctx.Done()

	return nil
}

// toMessage normalises a NATS message onto the broker-agnostic bridge.Message. Core NATS carries no
// message timestamp, so Timestamp is left zero and the transformer falls back to the current time.
func toMessage(m *nats.Msg) bridge.Message {
	var headers map[string]string

	if len(m.Header) > 0 {
		headers = make(map[string]string, len(m.Header))
		for k, vs := range m.Header {
			if len(vs) > 0 {
				headers[k] = vs[len(vs)-1]
			}
		}
	}

	return bridge.Message{
		Subject: m.Subject,
		Data:    m.Data,
		Headers: headers,
	}
}
