// Package outboxtest fakes the broker an outbox publishes to, for the tests of the
// application layers. Only tests may import it (see internal/archtest).
package outboxtest

import (
	"context"
	"sync"

	"safehome/internal/contract"
	"safehome/internal/outbox"
)

// Message is a message the broker took.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte
	Retain  bool
	Env     contract.Envelope // the payload read as an envelope, when it is one
	IsEnv   bool              // false for a message of another protocol (Zigbee2MQTT)
}

// Broker is an outbox.Publisher that records what it takes. It can be told to be
// away, to refuse some messages, or to hold every publication.
type Broker struct {
	mu     sync.Mutex
	msgs   []Message
	down   bool
	fails  int // the next publications answer outbox.ErrUnavailable
	refuse func(Message) error
	hold   chan struct{}
}

// Publish takes a message, or answers as the broker was told to.
func (b *Broker) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	b.mu.Lock()
	hold := b.hold
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m := Message{Topic: topic, Payload: append([]byte(nil), payload...), QoS: qos, Retain: retain}
	if env, err := contract.ParseEnvelope(payload); err == nil {
		m.Env, m.IsEnv = env, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return outbox.ErrUnavailable
	}
	if b.fails > 0 {
		b.fails--
		return outbox.ErrUnavailable
	}
	if b.refuse != nil {
		if err := b.refuse(m); err != nil {
			return err
		}
	}
	b.msgs = append(b.msgs, m)
	return nil
}

// SetDown makes the broker away (outbox.ErrUnavailable), or back.
func (b *Broker) SetDown(down bool) { b.mu.Lock(); b.down = down; b.mu.Unlock() }

// FailNext makes the next n publications answer outbox.ErrUnavailable.
func (b *Broker) FailNext(n int) { b.mu.Lock(); b.fails = n; b.mu.Unlock() }

// Refuse makes the broker answer the error refuse returns for a message, when it is
// not nil.
func (b *Broker) Refuse(refuse func(Message) error) { b.mu.Lock(); b.refuse = refuse; b.mu.Unlock() }

// Hold makes every publication wait until the returned function is called, or its
// context ends.
func (b *Broker) Hold() (release func()) {
	hold := make(chan struct{})
	b.mu.Lock()
	b.hold = hold
	b.mu.Unlock()
	return sync.OnceFunc(func() { close(hold) })
}

// All returns what the broker took, in order.
func (b *Broker) All() []Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Message(nil), b.msgs...)
}

// On returns what the broker took on a topic.
func (b *Broker) On(topic string) []Message {
	return b.filter(func(m Message) bool { return m.Topic == topic })
}

// OfType returns the envelopes of a type the broker took.
func (b *Broker) OfType(typ string) []Message {
	return b.filter(func(m Message) bool { return m.IsEnv && m.Env.Type == typ })
}

func (b *Broker) filter(keep func(Message) bool) []Message {
	var out []Message
	for _, m := range b.All() {
		if keep(m) {
			out = append(out, m)
		}
	}
	return out
}
