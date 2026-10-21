// Package service holds what every safehome-* program does the same way around its
// application: the command line, the logger, the pid file, the MQTT clients and the
// life cycle under systemd (ready, keepalive, clean stop). It is assembly for cmd/:
// it knows the adapters and nothing of the domains, and only cmd/ imports it.
package service

import (
	"context"
	"errors"
	"sync/atomic"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/inbox"
	"safehome/internal/outbox"
)

// Publisher is a broker publisher the application can be given before the MQTT
// client exists: the client needs the application (for its messages) and the
// application needs a publisher. Daemon.Run attaches the client once dialed.
type Publisher struct{ client atomic.Pointer[mqtt.Client] }

// Attach gives the publisher its client.
func (p *Publisher) Attach(c *mqtt.Client) { p.client.Store(c) }

// Publish sends through the client. It returns outbox.ErrUnavailable, which the
// applications retry on, before the client exists and while the broker is away.
func (p *Publisher) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	c := p.client.Load()
	if c == nil {
		return outbox.ErrUnavailable
	}
	err := c.Publish(ctx, mqtt.Message{Topic: topic, Payload: payload, QoS: qos, Retain: retain})
	if errors.Is(err, mqtt.ErrDown) {
		return outbox.ErrUnavailable
	}
	return err
}

// Subscriptions turns an application's subscriptions into the client's.
func Subscriptions(in []inbox.Subscription) []mqtt.Subscription {
	out := make([]mqtt.Subscription, len(in))
	for i, s := range in {
		out[i] = mqtt.Subscription(s)
	}
	return out
}

// Deliver turns an application's message handler into the client's.
func Deliver(handle func(inbox.Message)) func(mqtt.Message) {
	return func(m mqtt.Message) { handle(inbox.Message{Topic: m.Topic, Payload: m.Payload, Retain: m.Retain}) }
}
