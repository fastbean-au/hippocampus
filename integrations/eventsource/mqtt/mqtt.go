// Package mqtt is the MQTT broker adapter for the Hippocampus event-sourcing bridges. It subscribes
// to a topic filter and hands every delivered message to a bridge.Store. Acknowledgement is manual:
// the PUBACK is sent only after the store succeeds, so with QoS >= 1 and a persistent session an
// unstored message is redelivered when the bridge reconnects rather than being lost.
package mqtt

import (
	"context"
	"fmt"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/integrations/eventsource/bridge"
)

// Config describes the MQTT connection and subscription.
type Config struct {
	// Broker is the MQTT broker URL (e.g. tcp://localhost:1883, ssl://host:8883, ws://...).
	Broker string

	// Topic is the topic filter to subscribe to (supports + and # wildcards).
	Topic string

	// ClientID is the MQTT client identifier. A stable id plus CleanSession=false lets the broker
	// hold the session (and redeliver unacked QoS>=1 messages) across restarts.
	ClientID string

	// Username and Password are optional credentials.
	Username string
	Password string

	// QoS is the subscription quality of service (0, 1, or 2). 1 (at-least-once) is the default
	// that makes manual acknowledgement meaningful.
	QoS byte

	// CleanSession, when true, starts a fresh session each connect (dropping any queued redelivery).
	// Off by default so a persistent session can redeliver unstored messages after a restart.
	CleanSession bool
}

// connectFunc connects a paho client built from the options; overridable in tests. It returns the
// live client so Run can disconnect it on shutdown.
type connectFunc func(opts *mqtt.ClientOptions) (mqtt.Client, error)

// Bridge consumes an MQTT topic into a bridge.Store.
type Bridge struct {
	cfg   Config
	store *bridge.Store

	connect connectFunc
}

// New builds an MQTT Bridge from its config and the shared store.
func New(cfg Config, store *bridge.Store) *Bridge {
	return &Bridge{
		cfg:     cfg,
		store:   store,
		connect: defaultConnect,
	}
}

// defaultConnect connects a real paho client and waits for the connection to establish.
func defaultConnect(opts *mqtt.ClientOptions) (mqtt.Client, error) {
	client := mqtt.NewClient(opts)

	token := client.Connect()
	if token.Wait() && token.Error() != nil {
		return nil, token.Error()
	}

	return client, nil
}

// Run connects and subscribes (resubscribing on every reconnect), then blocks until ctx is
// cancelled, at which point it disconnects cleanly. The message handler stores each message and acks
// it only on success.
func (b *Bridge) Run(ctx context.Context) error {
	log.Trace("mqtt.Bridge.Run()")

	handler := b.messageHandler(ctx)

	opts := mqtt.NewClientOptions().
		AddBroker(b.cfg.Broker).
		SetClientID(b.cfg.ClientID).
		SetCleanSession(b.cfg.CleanSession).
		SetAutoAckDisabled(true).
		SetOnConnectHandler(func(c mqtt.Client) {
			// Subscribe on connect so a reconnect re-establishes the subscription.
			if token := c.Subscribe(b.cfg.Topic, b.cfg.QoS, handler); token.Wait() && token.Error() != nil {
				log.WithError(token.Error()).WithField("topic", b.cfg.Topic).
					Error("subscribing to MQTT topic failed")
			}
		})

	if b.cfg.Username != "" {
		opts = opts.SetUsername(b.cfg.Username).SetPassword(b.cfg.Password)
	}

	client, err := b.connect(opts)
	if err != nil {
		return fmt.Errorf("connecting to MQTT broker %q: %w", b.cfg.Broker, err)
	}

	defer client.Disconnect(250)

	log.WithFields(log.Fields{
		"broker": b.cfg.Broker,
		"topic":  b.cfg.Topic,
		"qos":    b.cfg.QoS,
	}).
		Info("MQTT bridge subscribed")

	<-ctx.Done()

	return nil
}

// messageHandler returns the paho callback that stores a message and acks it on success. A store
// failure is logged and left unacked so a QoS>=1 persistent session redelivers it on reconnect.
func (b *Bridge) messageHandler(ctx context.Context) mqtt.MessageHandler {
	return func(_ mqtt.Client, m mqtt.Message) {
		if err := b.store.Handle(ctx, toMessage(m)); err != nil {
			log.WithError(err).WithField("topic", m.Topic()).
				Warn("storing MQTT message failed; not acked (redelivered on reconnect for QoS>=1)")

			return
		}

		m.Ack()
	}
}

// toMessage normalises an MQTT message onto the broker-agnostic bridge.Message. MQTT v3 carries no
// headers or broker timestamp, so those are left empty and the transformer falls back to now.
func toMessage(m mqtt.Message) bridge.Message {
	return bridge.Message{
		Subject: m.Topic(),
		Data:    m.Payload(),
	}
}
