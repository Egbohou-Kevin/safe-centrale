// Package mqtt is the adapter between safeHome and an MQTT 5 broker (Mosquitto on
// the hub). It is the only package that imports the MQTT library: the domains, the
// contract and the drivers see plain messages. Replacing the library means
// rewriting this package and nothing else.
//
// The adapter keeps the connection up (it reconnects with a growing delay) and
// subscribes again after every reconnection. It knows nothing about topics or
// payloads: those belong to the contract.
package mqtt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// ErrDown is returned by Publish when the broker cannot be reached right now. The
// caller decides whether to retry: for an alarm event, the answer is usually yes.
var ErrDown = errors.New("mqtt: broker unreachable")

// ErrRefused is wrapped by the error of Publish when the broker refused the message
// for good (not authorized, topic or payload invalid...): the same message would get
// the same answer. A refusal that may pass later (quota exceeded) is another error.
var ErrRefused = errors.New("mqtt: broker refused the message")

const (
	keepAliveSeconds = 30
	// The broker keeps the session (subscriptions and queued messages) for a day
	// after a disconnection, which covers any realistic outage of the hub itself.
	sessionExpirySeconds = 24 * 60 * 60
	maxBackoff           = 30 * time.Second
)

// subscribeWait bounds the wait for the broker's answer to a subscription, as the
// library does (a variable for the tests).
var subscribeWait = 10 * time.Second

// Client is a connection to the broker.
type Client struct {
	cm *autopaho.ConnectionManager
	s  *session
}

// Dial starts a connection that lives until ctx is cancelled. It returns at once;
// the client connects, and reconnects, in the background. Use Await to wait for
// the first connection.
func Dial(ctx context.Context, o Options) (*Client, error) {
	urls, err := o.validate()
	if err != nil {
		return nil, err
	}
	s := newSession(ctx, o, urls)
	cfg, err := s.config()
	if err != nil {
		return nil, err
	}
	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	return &Client{cm: cm, s: s}, nil
}

// Await blocks until the client is connected, or ctx is done.
func (c *Client) Await(ctx context.Context) error { return c.cm.AwaitConnection(ctx) }

// Publish sends a message and waits for the broker's acknowledgement (QoS 1 and 2).
// It returns ErrDown if the broker cannot be reached right now, and an error wrapping
// ErrRefused if the broker refused the message for good.
func (c *Client) Publish(ctx context.Context, m Message) error {
	resp, err := c.cm.Publish(ctx, publishPacket(m))
	switch {
	case resp != nil && refused(resp.ReasonCode):
		return fmt.Errorf("%w: %s (reason %#x)", ErrRefused, m.Topic, resp.ReasonCode)
	case resp != nil && failed(resp.ReasonCode):
		return fmt.Errorf("mqtt: broker did not take %s for now (reason %#x)", m.Topic, resp.ReasonCode)
	case errors.Is(err, autopaho.ConnectionDownError):
		return ErrDown
	case err != nil:
		return fmt.Errorf("mqtt: publish to %s: %w", m.Topic, err)
	}
	return nil
}

// Close disconnects cleanly and waits for the connection to be released.
func (c *Client) Close(ctx context.Context) error {
	err := c.cm.Disconnect(ctx)
	c.s.mu.Lock()
	c.s.endConnection()
	c.s.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mqtt: %w", err)
	}
	return nil
}

// --- the library's callbacks ------------------------------------------------------

// session holds what the library's callbacks share for one client.
type session struct {
	o      Options
	urls   []*url.URL
	log    *slog.Logger
	life   context.Context         // the client's
	trying atomic.Pointer[url.URL] // the URL of the current connection attempt

	// mu orders what is told of the link: an "up" never follows the "down" of its
	// connection.
	mu      sync.Mutex
	endConn context.CancelFunc // ends the work of the current connection; nil while down
}

func newSession(life context.Context, o Options, urls []*url.URL) *session {
	s := &session{o: o, urls: urls, log: o.Logger, life: life}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.trying.Store(urls[0])
	return s
}

// config translates the options into the library's settings.
func (s *session) config() (autopaho.ClientConfig, error) {
	tlsCfg, err := s.o.tlsConfig()
	if err != nil {
		return autopaho.ClientConfig{}, err
	}
	expiry := uint32(sessionExpirySeconds)
	if s.o.Ephemeral {
		expiry = 0
	}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    s.urls,
		TlsCfg:                        tlsCfg,
		AttemptConnection:             dialBroker,
		KeepAlive:                     keepAliveSeconds,
		CleanStartOnInitialConnection: s.o.Ephemeral,
		SessionExpiryInterval:         expiry,
		ReconnectBackoff:              reconnectDelay(s.o.Spread),
		ConnectUsername:               s.o.Username,
		ConnectPacketBuilder:          s.connectPacket,
		OnConnectionUp:                s.connectionUp,
		OnConnectionDown:              s.connectionDown,
		OnConnectError:                s.connectError,
		ClientConfig: paho.ClientConfig{
			ClientID:                   s.o.ClientID,
			EnableManualAcknowledgment: s.o.Take != nil,
			OnPublishReceived:          []func(paho.PublishReceived) (bool, error){s.received},
			OnClientError:              func(err error) { s.log.Warn("mqtt client error", "err", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				s.log.Warn("mqtt broker asked to disconnect", "reason", d.ReasonCode)
			},
		},
	}
	if s.o.Password != "" {
		cfg.ConnectPassword = []byte(s.o.Password)
	}
	return cfg, nil
}

// connectPacket notes the URL being tried and adds a fresh will.
func (s *session) connectPacket(cp *paho.Connect, u *url.URL) (*paho.Connect, error) {
	s.trying.Store(u)
	if s.o.Will != nil {
		w := s.o.Will()
		cp.WillMessage = &paho.WillMessage{Topic: w.Topic, Payload: w.Payload, QoS: w.QoS, Retain: w.Retain}
	}
	return cp, nil
}

// connectError says why a connection attempt failed. A broker that answers but refuses
// the client (its name or password, 29 October 2026) is said apart, as an error: the
// program stays up and retries, deaf to the broker, and deploy/update-hub.sh looks for
// these words in the journal.
func (s *session) connectError(err error) {
	var refused *autopaho.ConnackError
	if errors.As(err, &refused) {
		s.log.Error("mqtt broker refused the connection", "reason", refused.ReasonCode, "err", err)
		return
	}
	s.log.Warn("mqtt connection attempt failed", "err", err)
}

// connectionUp subscribes, then says the link is up, away from the library's
// goroutine, which it must not hold up.
func (s *session) connectionUp(cm *autopaho.ConnectionManager, _ *paho.Connack) {
	s.log.Info("mqtt connected", "broker", s.trying.Load().Redacted())
	conn, end := context.WithCancel(s.life)
	s.mu.Lock()
	s.endConn = end
	s.mu.Unlock()
	go s.subscribeThenAnnounce(conn, cm, s.trying.Load().Scheme)
}

// subscribeThenAnnounce subscribes, then says the link is up. A broker that does not
// answer in time may have subscribed all the same, or its answer was lost: it is asked
// again, after a growing wait, until it answers or the connection drops. Until then
// the link is not up: nothing could come from the broker.
func (s *session) subscribeThenAnnounce(conn context.Context, cm *autopaho.ConnectionManager, scheme string) {
	for attempt := 1; len(s.o.Subscriptions) > 0 && !s.subscribe(conn, cm); attempt++ {
		wait := time.NewTimer(backoff(attempt))
		select {
		case <-conn.Done():
			wait.Stop()
			return
		case <-wait.C:
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn.Err() != nil {
		return // the connection dropped meanwhile, and "down" was told
	}
	if s.o.OnLinkChange != nil {
		s.o.OnLinkChange(true, scheme)
	}
	if s.o.OnSubscribed != nil {
		s.o.OnSubscribed()
	}
}

// subscribe asks for the subscriptions and reports whether the broker answered. A
// filter it refuses is logged; the others work.
func (s *session) subscribe(conn context.Context, cm *autopaho.ConnectionManager) bool {
	ctx, cancel := context.WithTimeout(conn, subscribeWait)
	defer cancel()
	ack, err := cm.Subscribe(ctx, subscribePacket(s.o.Subscriptions))
	if ack == nil {
		if conn.Err() == nil {
			s.log.Error("mqtt subscribe failed, trying again: no message is received meanwhile", "err", err)
		}
		return false
	}
	for i, reason := range ack.Reasons {
		if failed(reason) && i < len(s.o.Subscriptions) {
			s.log.Error("mqtt subscription refused", "filter", s.o.Subscriptions[i].Filter, "reason", reason)
		}
	}
	return true
}

func (s *session) connectionDown() bool {
	s.log.Warn("mqtt connection lost, reconnecting")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endConnection()
	if s.o.OnLinkChange != nil {
		s.o.OnLinkChange(false, "")
	}
	return true // keep reconnecting
}

// endConnection stops the work of the current connection (its subscription). The
// caller holds mu.
func (s *session) endConnection() {
	if s.endConn != nil {
		s.endConn()
		s.endConn = nil
	}
}

// received hands a message to the application. With Take, the broker is told it
// arrived only once the application took it (see Options.Take).
func (s *session) received(pr paho.PublishReceived) (bool, error) {
	if s.o.Take == nil {
		s.o.OnMessage(fromPaho(pr.Packet))
		return true, nil
	}
	if !s.o.Take(fromPaho(pr.Packet)) {
		return true, nil // left with the broker: it gives it again at the next connection
	}
	if err := pr.Client.Ack(pr.Packet); err != nil {
		s.log.Warn("mqtt acknowledgement failed: the broker will give the message again", "topic", pr.Packet.Topic, "err", err)
	}
	return true, nil
}

// --- reconnection delays and translation to and from the library's types -----------

// backoff is the wait before connection attempt n: none for the first one (n = 0,
// as the library counts), then 1 s, 2 s, 4 s... up to 30 s. A hub that boots before
// its broker catches up quickly, and one whose broker is down for hours does not
// hammer it.
func backoff(n int) time.Duration {
	if n <= 0 {
		return 0
	}
	if n > 5 {
		return maxBackoff
	}
	return min(time.Second<<(n-1), maxBackoff)
}

// firstSpread bounds the random wait before the first attempt with Spread.
const firstSpread = 2 * time.Second

// reconnectDelay returns backoff, or with spread a random wait between none and the
// backoff (between none and firstSpread for the first attempt).
func reconnectDelay(spread bool) func(int) time.Duration {
	if !spread {
		return backoff
	}
	return func(n int) time.Duration {
		d := backoff(n)
		if d == 0 {
			d = firstSpread
		}
		return rand.N(d + 1)
	}
}

func subscribePacket(subs []Subscription) *paho.Subscribe {
	opts := make([]paho.SubscribeOptions, len(subs))
	for i, s := range subs {
		opts[i] = paho.SubscribeOptions{Topic: s.Filter, QoS: s.QoS, NoLocal: s.NoLocal}
	}
	return &paho.Subscribe{Subscriptions: opts}
}

func publishPacket(m Message) *paho.Publish {
	return &paho.Publish{Topic: m.Topic, Payload: m.Payload, QoS: m.QoS, Retain: m.Retain}
}

func fromPaho(p *paho.Publish) Message {
	return Message{Topic: p.Topic, Payload: p.Payload, QoS: p.QoS, Retain: p.Retain}
}

// failed reports whether an MQTT 5 reason code is an error (0x80 and above).
func failed(reason byte) bool { return reason >= 0x80 }

// The failures that may pass if the message is sent again later.
const (
	packetIDInUse = 0x91
	quotaExceeded = 0x97 // a limit of the broker: it refuses every message until it is lifted
)

// refused reports whether a reason code of an acknowledgement refuses the message for
// good.
func refused(reason byte) bool {
	return failed(reason) && reason != packetIDInUse && reason != quotaExceeded
}
