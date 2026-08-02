// Package kafka is the Apache Kafka broker adapter for the Hippocampus event-sourcing bridges. It
// reads a topic as part of a consumer group and hands every message to a bridge.Store, committing
// the message's offset only after the store succeeds. That gives at-least-once semantics: a store
// failure leaves the offset uncommitted so the message is re-read on the next fetch (after a
// backoff), rather than being silently skipped.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"time"

	kafkago "github.com/segmentio/kafka-go"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// Config describes the Kafka connection and consumer group.
type Config struct {
	// Brokers is the list of bootstrap broker addresses (host:port).
	Brokers []string

	// Topic is the topic to consume.
	Topic string

	// GroupID is the consumer group id; several bridge instances sharing it split the topic's
	// partitions between them. Required for offset commits and rebalancing.
	GroupID string

	// MinBytes and MaxBytes bound each fetch; 0 leaves the reader defaults.
	MinBytes int
	MaxBytes int

	// ErrorBackoff is how long to wait after a store failure before re-reading the uncommitted
	// message, so a persistently failing service is not hammered. <= 0 uses one second.
	ErrorBackoff time.Duration
}

// reader is the slice of *kafkago.Reader the Bridge uses, so tests can substitute a fake.
type reader interface {
	FetchMessage(ctx context.Context) (kafkago.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafkago.Message) error
	Close() error
}

// Bridge consumes a Kafka topic into a bridge.Store.
type Bridge struct {
	cfg   Config
	store *bridge.Store

	// newReader builds the reader; overridable in tests.
	newReader func(Config) reader
}

// New builds a Kafka Bridge from its config and the shared store.
func New(cfg Config, store *bridge.Store) *Bridge {
	return &Bridge{
		cfg:       cfg,
		store:     store,
		newReader: defaultReader,
	}
}

// defaultReader builds a real kafka-go reader from the config.
func defaultReader(cfg Config) reader {
	return kafkago.NewReader(kafkago.ReaderConfig{
		Brokers:  cfg.Brokers,
		Topic:    cfg.Topic,
		GroupID:  cfg.GroupID,
		MinBytes: cfg.MinBytes,
		MaxBytes: cfg.MaxBytes,
	})
}

// Run reads the topic and stores each message, committing offsets only after a successful store,
// until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	log.Trace("kafka.Bridge.Run()")

	r := b.newReader(b.cfg)

	defer func() { _ = r.Close() }()

	log.WithFields(log.Fields{
		"brokers": b.cfg.Brokers,
		"topic":   b.cfg.Topic,
		"group":   b.cfg.GroupID,
	}).
		Info("Kafka bridge consuming")

	return b.consume(ctx, r)
}

// consume drives the fetch/store/commit loop. Split out so it can be exercised with a fake reader.
func (b *Bridge) consume(ctx context.Context, r reader) error {
	backoff := b.cfg.ErrorBackoff
	if backoff <= 0 {
		backoff = time.Second
	}

	for {
		m, err := r.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("fetching from topic %q: %w", b.cfg.Topic, err)
		}

		if err := b.store.Handle(ctx, toMessage(m)); err != nil {
			log.WithError(err).WithFields(log.Fields{
				"topic":     m.Topic,
				"partition": m.Partition,
				"offset":    m.Offset,
			}).
				Warn("storing Kafka message failed; offset not committed, will retry after backoff")

			if sleepErr := sleep(ctx, backoff); sleepErr != nil {
				return nil
			}

			continue
		}

		if err := r.CommitMessages(ctx, m); err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("committing offset for topic %q: %w", b.cfg.Topic, err)
		}
	}
}

// sleep waits for d or until ctx is cancelled, returning ctx.Err() if cancelled first.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {

	case <-ctx.Done():
		return ctx.Err()

	case <-timer.C:
		return nil
	}
}

// toMessage normalises a Kafka message onto the broker-agnostic bridge.Message: the topic is the
// subject, record headers become string headers, and the record timestamp carries through.
func toMessage(m kafkago.Message) bridge.Message {
	var headers map[string]string

	if len(m.Headers) > 0 {
		headers = make(map[string]string, len(m.Headers))
		for _, v := range m.Headers {
			headers[v.Key] = string(v.Value)
		}
	}

	return bridge.Message{
		Subject:   m.Topic,
		Data:      m.Value,
		Headers:   headers,
		Timestamp: m.Time,
	}
}
