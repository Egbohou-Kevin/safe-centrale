package mqtt

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
)

// startFakeBroker plays a broker that refuses the subscriptions in refuse (reason
// 0x87, not authorized), as the cloud's access list would. Mosquitto cannot do it: it
// grants every subscription and filters on delivery.
func startFakeBroker(t *testing.T, refuse map[string]bool) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveFake(conn, refuse)
		}
	}()
	return "mqtt://" + l.Addr().String()
}

func serveFake(conn net.Conn, refuse map[string]bool) {
	defer conn.Close()
	for {
		in, err := packets.ReadPacket(conn)
		if err != nil {
			return
		}
		var out *packets.ControlPacket
		switch in.Type {
		case packets.CONNECT:
			out = packets.NewControlPacket(packets.CONNACK)
		case packets.SUBSCRIBE:
			sub := in.Content.(*packets.Subscribe)
			out = packets.NewControlPacket(packets.SUBACK)
			ack := out.Content.(*packets.Suback)
			ack.PacketID = sub.PacketID
			for _, s := range sub.Subscriptions {
				reason := s.QoS
				if refuse[s.Topic] {
					reason = 0x87
				}
				ack.Reasons = append(ack.Reasons, reason)
			}
		case packets.PINGREQ:
			out = packets.NewControlPacket(packets.PINGRESP)
		case packets.DISCONNECT:
			return
		default:
			continue
		}
		if _, err := out.WriteTo(conn); err != nil {
			return
		}
	}
}

func TestARefusedSubscriptionDoesNotKeepTheLinkDown(t *testing.T) {
	// The broker refuses one filter: the others work, so the link is up. Before, the
	// client took the partial refusal for a failure and never said so: the agent kept
	// the cloud for unreachable and sent it nothing.
	url := startFakeBroker(t, map[string]bool{"safehome/s1/forbidden": true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l := newLinks()
	subscribed := make(chan struct{}, 4)
	c, err := Dial(ctx, Options{
		BrokerURL: url, ClientID: uniqueID("acl"), OnLinkChange: l.change,
		OnSubscribed: func() { subscribed <- struct{}{} },
		Subscriptions: []Subscription{
			{Filter: "safehome/s1/alarm/cmd", QoS: 1},
			{Filter: "safehome/s1/forbidden", QoS: 1},
		},
		OnMessage: func(Message) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up mqtt" {
		t.Fatalf("link %q, want up", s)
	}
	select {
	case <-subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("OnSubscribed was never called")
	}
	c.Close(ctx)
}
