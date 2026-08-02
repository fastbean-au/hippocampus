// Package rabbitmq is the RabbitMQ (AMQP 0-9-1) broker adapter for the Hippocampus event-sourcing
// bridges. It consumes a queue with manual acknowledgement and hands every delivery to a
// bridge.Store: a successfully stored message is acked, a store failure is nacked with requeue so
// the broker redelivers it. That gives at-least-once semantics - the natural fit for AMQP.
package rabbitmq

import (
	"context"
	"fmt"

	amqp "github.com/rabbitmq/amqp091-go"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// Config describes the RabbitMQ connection and consumer.
type Config struct {
	// URL is the AMQP URL (e.g. amqp://guest:guest@localhost:5672/).
	URL string

	// Queue is the queue to consume from. It must already exist unless DeclareQueue is set.
	Queue string

	// ConsumerTag identifies this consumer to the broker; empty lets the server generate one.
	ConsumerTag string

	// Prefetch bounds unacknowledged deliveries in flight (QoS). <= 0 uses 1, which keeps
	// processing strictly ordered; raise it for throughput when order does not matter.
	Prefetch int

	// DeclareQueue declares (idempotently) a durable queue named Queue before consuming, for a
	// standalone demo where no producer has declared it yet. Off by default so the bridge does not
	// impose a topology on an existing broker.
	DeclareQueue bool

	// RequeueOnError requeues a delivery whose store failed so the broker redelivers it. Defaults
	// on; set false to drop a failed delivery (dead-letter it, if the queue has a DLX) instead of
	// risking a hot redelivery loop on a poison message.
	RequeueOnError bool
}

// amqpConn and amqpChannel are the slices of *amqp.Connection / *amqp.Channel the Bridge uses, so
// Run can be exercised without a broker.
type amqpConn interface {
	Channel() (amqpChannel, error)
	Close() error
}

type amqpChannel interface {
	Qos(prefetchCount int, prefetchSize int, global bool) error
	QueueDeclare(name string, durable bool, autoDelete bool, exclusive bool, noWait bool, args amqp.Table) (amqp.Queue, error)
	Consume(queue string, consumer string, autoAck bool, exclusive bool, noLocal bool, noWait bool, args amqp.Table) (<-chan amqp.Delivery, error)
	Close() error
}

// realAmqpConn / realAmqpChannel adapt the concrete driver types onto the interfaces above.
type realAmqpConn struct {
	conn *amqp.Connection
}

func (c realAmqpConn) Channel() (amqpChannel, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, err
	}

	return ch, nil
}

func (c realAmqpConn) Close() error {
	return c.conn.Close()
}

// Bridge consumes a RabbitMQ queue into a bridge.Store.
type Bridge struct {
	cfg   Config
	store *bridge.Store

	// dial opens the connection; overridable in tests.
	dial func(url string) (amqpConn, error)
}

// New builds a RabbitMQ Bridge from its config and the shared store.
func New(cfg Config, store *bridge.Store) *Bridge {
	return &Bridge{
		cfg:   cfg,
		store: store,
		dial:  defaultDial,
	}
}

// defaultDial opens a real AMQP connection and adapts it onto amqpConn.
func defaultDial(url string) (amqpConn, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, err
	}

	return realAmqpConn{conn: conn}, nil
}

// Run connects, opens a channel, and consumes the queue until ctx is cancelled or the broker closes
// the delivery stream, acking/nacking each delivery by the store's outcome.
func (b *Bridge) Run(ctx context.Context) error {
	log.Trace("rabbitmq.Bridge.Run()")

	conn, err := b.dial(b.cfg.URL)
	if err != nil {
		return fmt.Errorf("connecting to RabbitMQ at %q: %w", b.cfg.URL, err)
	}

	defer func() { _ = conn.Close() }()

	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("opening channel: %w", err)
	}

	defer func() { _ = ch.Close() }()

	prefetch := b.cfg.Prefetch
	if prefetch <= 0 {
		prefetch = 1
	}

	if err := ch.Qos(prefetch, 0, false); err != nil {
		return fmt.Errorf("setting channel QoS: %w", err)
	}

	if b.cfg.DeclareQueue {
		if _, err := ch.QueueDeclare(b.cfg.Queue, true, false, false, false, nil); err != nil {
			return fmt.Errorf("declaring queue %q: %w", b.cfg.Queue, err)
		}
	}

	// autoAck=false: the bridge acks explicitly once the store call succeeds.
	deliveries, err := ch.Consume(b.cfg.Queue, b.cfg.ConsumerTag, false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consuming queue %q: %w", b.cfg.Queue, err)
	}

	log.WithFields(log.Fields{
		"queue":    b.cfg.Queue,
		"prefetch": prefetch,
	}).
		Info("RabbitMQ bridge consuming")

	return b.consume(ctx, deliveries)
}

// consume drives the delivery loop until ctx is cancelled or the channel closes. Split out so it can
// be exercised with a synthetic delivery channel in tests.
func (b *Bridge) consume(ctx context.Context, deliveries <-chan amqp.Delivery) error {
	for {
		select {

		case <-ctx.Done():
			return nil

		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("RabbitMQ delivery channel closed")
			}

			b.handle(ctx, d)
		}
	}
}

// handle stores one delivery and acks or nacks it by the outcome.
func (b *Bridge) handle(ctx context.Context, d amqp.Delivery) {
	if err := b.store.Handle(ctx, toMessage(d)); err != nil {
		log.WithError(err).WithField("routing_key", d.RoutingKey).
			Warn("storing RabbitMQ delivery failed")

		if nackErr := d.Nack(false, b.cfg.RequeueOnError); nackErr != nil {
			log.WithError(nackErr).Warn("nacking RabbitMQ delivery failed")
		}

		return
	}

	if ackErr := d.Ack(false); ackErr != nil {
		log.WithError(ackErr).Warn("acking RabbitMQ delivery failed")
	}
}

// toMessage normalises an AMQP delivery onto the broker-agnostic bridge.Message: the routing key is
// the subject, the AMQP headers table and a few well-known properties become string headers, and the
// message Timestamp (when the publisher set one) carries through.
func toMessage(d amqp.Delivery) bridge.Message {
	headers := map[string]string{}

	for k, v := range d.Headers {
		headers[k] = bridge.HeaderString(v)
	}

	if d.Type != "" {
		headers["type"] = d.Type
	}

	if d.AppId != "" {
		headers["app_id"] = d.AppId
	}

	if d.ContentType != "" {
		headers["content_type"] = d.ContentType
	}

	if len(headers) == 0 {
		headers = nil
	}

	return bridge.Message{
		Subject:   d.RoutingKey,
		Data:      d.Body,
		Headers:   headers,
		Timestamp: d.Timestamp,
	}
}
