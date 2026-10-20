// Package outbox sends the messages of an application to the broker, in order, without
// ever making the application wait: messages are queued in memory and a goroutine
// publishes them, retrying while the broker is away. It also holds the two other
// pieces every component of the site needs around the contract: the message counter
// (seq) that survives restarts, and the memory of recent message ids that makes an
// at-least-once delivery handled once.
//
// It knows the contract (it builds the envelopes) and the driver's Store port, and
// nothing of any domain.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"safehome/internal/contract"
)

// ErrUnavailable is what a Publisher returns when the broker cannot be reached right
// now. The outbox keeps the message and tries again; any other error is treated as
// a message the broker will never accept.
var ErrUnavailable = errors.New("outbox: broker unavailable")

// Publisher sends one message to the broker. It is the port an application needs from
// the MQTT adapter; the composition root adapts the real client to it.
type Publisher interface {
	Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error
}

// Item is a message to send. The envelope is built, and the sequence number taken,
// only when the message is about to leave: numbers then follow the order of
// departure, and the domains never wait for the disk.
type Item struct {
	Topic  string
	Src    string // the component, written in the envelope
	Type   string
	Data   json.RawMessage
	QoS    byte
	Retain bool
	ID     string
	TS     time.Time // when it happened
	// Raw sends Data as it is, without envelope or sequence number: a message for
	// another protocol (Zigbee2MQTT), or nothing at all, which clears a retained
	// message. Src, Type, ID and TS are then unused.
	Raw bool

	payload  []byte // the finished envelope, once built; kept across retries
	failures int    // refusals other than "unavailable"
}

// Outbox is an in-memory queue in front of the publisher. A domain loop must not
// wait for the broker, so it drops messages here and Run sends them in order,
// retrying while the broker is away.
type Outbox struct {
	pub  Publisher
	seq  *Sequencer
	site string
	log  *slog.Logger

	max            int
	retryMin       time.Duration
	retryMax       time.Duration
	attemptTimeout time.Duration

	mu      sync.Mutex
	queue   []*Item
	dropped int
	wake    chan struct{}
}

const maxRefusals = 3 // a message the broker keeps refusing is dropped after this many tries

// New builds an outbox. max is the most messages kept while the broker is away;
// retryMin and retryMax bound the wait between attempts.
func New(pub Publisher, seq *Sequencer, site string, max int, retryMin, retryMax time.Duration, log *slog.Logger) *Outbox {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Outbox{
		pub: pub, seq: seq, site: site, log: log,
		max: max, retryMin: retryMin, retryMax: retryMax, attemptTimeout: 5 * time.Second,
		wake: make(chan struct{}, 1),
	}
}

// Put queues a message. A retained message replaces an older retained message for
// the same topic that has not left yet: only the last state matters. When the queue
// is full the oldest message is dropped, because a hub with a dead broker for hours
// must not eat its memory.
func (o *Outbox) Put(it *Item) {
	o.mu.Lock()
	if it.Retain {
		for i, q := range o.queue {
			if q.Retain && q.Topic == it.Topic && q.payload == nil { // not started: safe to replace
				o.queue = append(o.queue[:i], o.queue[i+1:]...)
				break
			}
		}
	}
	if len(o.queue) >= o.max {
		old := o.queue[0]
		o.queue = o.queue[1:]
		o.dropped++
		o.log.Error("outbox full, dropping the oldest message", "topic", old.Topic, "type", old.Type, "dropped", o.dropped)
	}
	o.queue = append(o.queue, it)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// Depth is the number of messages waiting.
func (o *Outbox) Depth() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.queue)
}

func (o *Outbox) head() *Item {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) == 0 {
		return nil
	}
	return o.queue[0]
}

func (o *Outbox) pop(it *Item) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.queue) > 0 && o.queue[0] == it {
		o.queue = o.queue[1:]
	}
}

// build gives the message its sequence number and its envelope, once.
func (o *Outbox) build(it *Item) error {
	if it.payload != nil {
		return nil
	}
	if it.Raw {
		o.mu.Lock()
		it.payload = append([]byte{}, it.Data...) // never nil, even when empty
		o.mu.Unlock()
		return nil
	}
	env, err := contract.NewEnvelope(o.site, it.Src, o.seq.Next(), it.ID, it.TS, it.Type, it.Data)
	if err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	o.mu.Lock() // put reads payload under the lock to know whether a message has started
	it.payload = b
	o.mu.Unlock()
	return nil
}

// Run sends the queue until ctx is done.
func (o *Outbox) Run(ctx context.Context) {
	delay := o.retryMin
	for {
		it := o.head()
		if it == nil {
			select {
			case <-ctx.Done():
				return
			case <-o.wake:
			}
			continue
		}
		if err := o.build(it); err != nil {
			o.log.Error("dropping a message that cannot be built", "topic", it.Topic, "type", it.Type, "err", err)
			o.pop(it)
			continue
		}
		err := o.attempt(ctx, it)
		switch {
		case err == nil:
			o.pop(it)
			delay = o.retryMin
		case ctx.Err() != nil:
			return
		case errors.Is(err, ErrUnavailable):
			o.log.Warn("broker unavailable, will retry", "queued", o.Depth(), "retry_in", delay)
			if !Sleep(ctx, delay) {
				return
			}
			delay = min(delay*2, o.retryMax)
		default:
			it.failures++
			o.log.Error("the broker refused a message", "topic", it.Topic, "type", it.Type, "err", err, "attempt", it.failures)
			if it.failures >= maxRefusals {
				o.pop(it)
				continue
			}
			if !Sleep(ctx, o.retryMin) {
				return
			}
		}
	}
}

func (o *Outbox) attempt(ctx context.Context, it *Item) error {
	actx, cancel := context.WithTimeout(ctx, o.attemptTimeout)
	defer cancel()
	err := o.pub.Publish(actx, it.Topic, it.payload, it.QoS, it.Retain)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return ErrUnavailable // a broker that does not answer is a broker that is away
	}
	return err
}

// Flush makes one last attempt to send what is left, within the given time. It is
// used at shutdown, so the last state reaches the broker.
func (o *Outbox) Flush(grace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	for {
		it := o.head()
		if it == nil || ctx.Err() != nil {
			return
		}
		if err := o.build(it); err != nil {
			o.pop(it)
			continue
		}
		if err := o.attempt(ctx, it); err != nil {
			return
		}
		o.pop(it)
	}
}

// Sleep waits d, or less if ctx is done first; it reports whether the whole wait
// elapsed.
func Sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
