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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"os"
	"sync/atomic"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/paho"
)

// ErrDown is returned by Publish when the broker cannot be reached right now. The
// caller decides whether to retry: for an alarm event, the answer is usually yes.
var ErrDown = errors.New("mqtt: broker unreachable")

// Message is one MQTT message, as sent or received.
type Message struct {
	Topic   string
	Payload []byte
	QoS     byte // 0, 1 or 2; the contract uses 1 for events and orders
	Retain  bool // the broker keeps the last retained message of a topic (states)
}

// Subscription is a topic filter to listen to, with the best QoS wanted.
type Subscription struct {
	Filter string // for example "safehome/tg-lom-0001/device/+/event"
	QoS    byte
	// NoLocal: the client does not receive what it publishes itself on this filter
	// (the link agent, which republishes between two brokers, must not hear itself).
	NoLocal bool
}

// Options configures a client.
type Options struct {
	BrokerURL string // "mqtt://127.0.0.1:1883"; "mqtts://" and "wss://" use TLS
	// FallbackURLs are tried in turn when BrokerURL cannot be reached: for example
	// MQTT in a WebSocket on port 443, where a network blocks port 8883.
	FallbackURLs []string
	// CAFile, when set, is the authority (PEM) that must have signed the broker's TLS
	// certificate; otherwise the system's authorities are trusted.
	CAFile string
	// ClientID must be stable and unique on the broker. With a stable id, the broker
	// keeps the QoS 1 messages published while the client was away.
	ClientID string
	Username string
	Password string
	// Ephemeral asks the broker to forget the session at disconnection: nothing is
	// queued for the client while it is away. For short-lived tools (the installer's),
	// whose next run must not receive the messages of the past day.
	Ephemeral bool

	Subscriptions []Subscription
	// OnMessage receives every message that matches a subscription. It is called
	// from the client's own goroutine: it must return quickly (hand the message to
	// the driver's queue) and must not call Publish synchronously.
	OnMessage func(Message)
	// OnSubscribed, when set, is called each time the subscriptions are active
	// (after every connection): retained messages follow right after.
	OnSubscribed func()
	// OnLinkChange, when set, is told when the connection is up and subscribed,
	// with the scheme of the URL in use ("mqtts", "wss"...), and when it drops.
	// It must return quickly and must not call Publish synchronously.
	OnLinkChange func(up bool, scheme string)
	// Will, when set, gives the message the broker publishes if the connection drops
	// without a clean disconnection (the site went offline). It is called before
	// each connection attempt, so the message can be fresh (a new id, the time).
	Will func() Message
	// Spread waits a random part of each reconnection delay, the first one included
	// (up to 2 s): hundreds of sites that lose the cloud at the same instant do not
	// all come back at the same instant. Not worth it for the hub's own broker.
	Spread bool

	Logger *slog.Logger // nil discards the logs
}

const (
	keepAliveSeconds = 30
	// The broker keeps the session (subscriptions and queued messages) for a day
	// after a disconnection, which covers any realistic outage of the hub itself.
	sessionExpirySeconds = 24 * 60 * 60
	maxBackoff           = 30 * time.Second
)

// Client is a connection to the broker.
type Client struct {
	cm  *autopaho.ConnectionManager
	log *slog.Logger
}

// validate checks the options and returns the parsed broker URL.
func (o Options) validate() ([]*url.URL, error) {
	var urls []*url.URL
	for _, raw := range append([]string{o.BrokerURL}, o.FallbackURLs...) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("mqtt: invalid broker URL %q", raw)
		}
		switch u.Scheme {
		case "mqtt", "mqtts", "tls", "ws", "wss":
		default:
			return nil, fmt.Errorf("mqtt: unsupported scheme %q in %q", u.Scheme, raw)
		}
		if u.User != nil {
			return nil, fmt.Errorf("mqtt: put the credentials in Username and Password, not in %q", u.Redacted())
		}
		urls = append(urls, u)
	}
	if o.ClientID == "" {
		return nil, fmt.Errorf("mqtt: ClientID is required")
	}
	if len(o.Subscriptions) > 0 && o.OnMessage == nil {
		return nil, fmt.Errorf("mqtt: OnMessage is required when there are subscriptions")
	}
	for _, s := range o.Subscriptions {
		if s.Filter == "" || s.QoS > 2 {
			return nil, fmt.Errorf("mqtt: invalid subscription %+v", s)
		}
	}
	return urls, nil
}

// tlsConfig returns the TLS settings: nil (the system's authorities) without CAFile.
func (o Options) tlsConfig() (*tls.Config, error) {
	if o.CAFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(o.CAFile)
	if err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mqtt: no certificate in %s", o.CAFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// Dial starts a connection that lives until ctx is cancelled. It returns at once;
// the client connects, and reconnects, in the background. Use Await to wait for
// the first connection.
func Dial(ctx context.Context, o Options) (*Client, error) {
	urls, err := o.validate()
	if err != nil {
		return nil, err
	}
	tlsCfg, err := o.tlsConfig()
	if err != nil {
		return nil, err
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{log: log}

	expiry := uint32(sessionExpirySeconds)
	if o.Ephemeral {
		expiry = 0
	}
	var trying atomic.Value // the URL of the current connection attempt
	trying.Store(urls[0])
	linkUp := func() {
		if o.OnLinkChange != nil {
			o.OnLinkChange(true, trying.Load().(*url.URL).Scheme)
		}
		if o.OnSubscribed != nil {
			o.OnSubscribed()
		}
	}
	cfg := autopaho.ClientConfig{
		ServerUrls:                    urls,
		TlsCfg:                        tlsCfg,
		AttemptConnection:             dialBroker,
		KeepAlive:                     keepAliveSeconds,
		CleanStartOnInitialConnection: o.Ephemeral,
		SessionExpiryInterval:         expiry,
		ReconnectBackoff:              reconnectDelay(o.Spread),
		ConnectUsername:               o.Username,
		ConnectPacketBuilder: func(cp *paho.Connect, u *url.URL) (*paho.Connect, error) {
			trying.Store(u)
			if o.Will != nil {
				w := o.Will()
				cp.WillMessage = &paho.WillMessage{Topic: w.Topic, Payload: w.Payload, QoS: w.QoS, Retain: w.Retain}
			}
			return cp, nil
		},
		OnConnectionUp: func(cm *autopaho.ConnectionManager, _ *paho.Connack) {
			log.Info("mqtt connected", "broker", trying.Load().(*url.URL).Redacted())
			if len(o.Subscriptions) == 0 {
				linkUp()
				return
			}
			ack, err := cm.Subscribe(context.Background(), subscribePacket(o.Subscriptions))
			if err != nil {
				log.Error("mqtt subscribe failed: no message will be received", "err", err)
				return
			}
			for i, r := range ack.Reasons {
				if failed(r) {
					log.Error("mqtt subscription refused", "filter", o.Subscriptions[i].Filter, "reason", r)
				}
			}
			linkUp()
		},
		OnConnectionDown: func() bool {
			log.Warn("mqtt connection lost, reconnecting")
			if o.OnLinkChange != nil {
				o.OnLinkChange(false, "")
			}
			return true
		},
		OnConnectError: func(err error) { log.Warn("mqtt connection attempt failed", "err", err) },
		ClientConfig: paho.ClientConfig{
			ClientID: o.ClientID,
			OnPublishReceived: []func(paho.PublishReceived) (bool, error){
				func(pr paho.PublishReceived) (bool, error) {
					o.OnMessage(fromPaho(pr.Packet))
					return true, nil
				},
			},
			OnClientError: func(err error) { log.Warn("mqtt client error", "err", err) },
			OnServerDisconnect: func(d *paho.Disconnect) {
				log.Warn("mqtt broker asked to disconnect", "reason", d.ReasonCode)
			},
		},
	}
	if o.Password != "" {
		cfg.ConnectPassword = []byte(o.Password)
	}

	cm, err := autopaho.NewConnection(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("mqtt: %w", err)
	}
	c.cm = cm
	return c, nil
}

// Await blocks until the client is connected, or ctx is done.
func (c *Client) Await(ctx context.Context) error { return c.cm.AwaitConnection(ctx) }

// Publish sends a message and waits for the broker's acknowledgement (QoS 1 and 2).
// It returns ErrDown if the broker cannot be reached right now.
func (c *Client) Publish(ctx context.Context, m Message) error {
	resp, err := c.cm.Publish(ctx, publishPacket(m))
	if err != nil {
		if errors.Is(err, autopaho.ConnectionDownError) {
			return ErrDown
		}
		return fmt.Errorf("mqtt: publish to %s: %w", m.Topic, err)
	}
	if resp != nil && failed(resp.ReasonCode) {
		return fmt.Errorf("mqtt: broker refused %s (reason %d)", m.Topic, resp.ReasonCode)
	}
	return nil
}

// Close disconnects cleanly and waits for the connection to be released.
func (c *Client) Close(ctx context.Context) error {
	err := c.cm.Disconnect(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mqtt: %w", err)
	}
	return nil
}

// --- translation to and from the library's types -------------------------------

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
