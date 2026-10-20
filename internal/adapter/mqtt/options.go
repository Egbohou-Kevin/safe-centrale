package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/url"
	"os"
)

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
	// (after every connection): retained messages follow right after. A filter the
	// broker refuses is logged and does not hold the others back.
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

// validate checks the options and returns the parsed broker URLs, the first one
// first.
func (o Options) validate() ([]*url.URL, error) {
	var urls []*url.URL
	for _, raw := range append([]string{o.BrokerURL}, o.FallbackURLs...) {
		u, err := parseBrokerURL(raw)
		if err != nil {
			return nil, err
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

func parseBrokerURL(raw string) (*url.URL, error) {
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
	return u, nil
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
