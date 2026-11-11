package mqtt

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"sync"
	"time"

	"github.com/eclipse/paho.golang/autopaho"
	"github.com/eclipse/paho.golang/packets"
	"github.com/gorilla/websocket"
)

// dialBroker opens the network connection to the broker. It does what the library does,
// except for WebSockets: there each MQTT packet travels in a single WebSocket
// message (see packetConn).
func dialBroker(ctx context.Context, cfg autopaho.ClientConfig, u *url.URL) (net.Conn, error) {
	switch u.Scheme {
	case "ws":
		return dialWebSocket(ctx, nil, u)
	case "wss":
		return dialWebSocket(ctx, cfg.TlsCfg, u)
	case "mqtt":
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(c), nil
	default: // mqtts, tls
		d := tls.Dialer{Config: cfg.TlsCfg}
		c, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		return packets.NewThreadSafeConn(c), nil
	}
}

func dialWebSocket(ctx context.Context, tlsCfg *tls.Config, u *url.URL) (net.Conn, error) {
	d := *websocket.DefaultDialer
	d.TLSClientConfig = tlsCfg
	d.Subprotocols = []string{"mqtt"}
	ws, _, err := d.DialContext(ctx, u.String(), nil)
	if err != nil {
		return nil, err
	}
	return &packetConn{ws: ws}, nil
}

// packetConn is a WebSocket seen as a net.Conn, which sends each MQTT packet in one
// WebSocket message. The library writes a packet in several pieces while holding the
// connection's lock (packets.ControlPacket.WriteTo): the pieces are gathered while
// the lock is held and sent when it is released. The MQTT standard lets a packet span
// several messages, but some brokers refuse it (Mosquitto 2.1's own WebSockets
// closes the connection as "malformed packet").
type packetConn struct {
	ws *websocket.Conn

	mu      sync.Mutex // the lock the library takes around a packet
	writing bool       // the lock is held: Write gathers
	pending []byte     // the pieces gathered so far
	werr    error      // a failed send, returned by the next Write

	rmu sync.Mutex
	r   io.Reader // the WebSocket message being read
}

// Lock starts a packet.
func (c *packetConn) Lock() {
	c.mu.Lock()
	c.writing = true
}

// Unlock sends the packet gathered since Lock.
func (c *packetConn) Unlock() {
	if len(c.pending) > 0 && c.werr == nil {
		c.werr = c.ws.WriteMessage(websocket.BinaryMessage, c.pending)
	}
	c.pending = c.pending[:0]
	c.writing = false
	c.mu.Unlock()
}

// Write gathers a piece of the packet under way, or sends p alone when no packet is
// under way.
func (c *packetConn) Write(p []byte) (int, error) {
	if c.werr != nil {
		return 0, c.werr
	}
	if c.writing {
		c.pending = append(c.pending, p...)
		return len(p), nil
	}
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Read reads the incoming messages as one stream of bytes.
func (c *packetConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			_, r, err := c.ws.NextReader()
			if err != nil {
				return 0, err
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (c *packetConn) Close() error         { return c.ws.Close() }
func (c *packetConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *packetConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *packetConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}

func (c *packetConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *packetConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
