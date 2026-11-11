package mqtt

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eclipse/paho.golang/packets"
)

// fakeRules say how the fake broker answers. Mosquitto cannot play these: it grants
// every subscription, filters on delivery, and acknowledges every message.
type fakeRules struct {
	refuse  map[string]bool // subscriptions refused (0x87, not authorized), as the cloud's access list would
	puback  map[string]byte // the reason code of the acknowledgement, by topic (0 if absent)
	silence atomic.Int32    // how many subscriptions, the first ones, get no answer
	deliver []string        // topics of the messages (QoS 1) sent after the first subscription
	acked   chan uint16     // the packet ids the client acknowledged, if not nil
}

// startFakeBroker plays a broker that follows rules.
func startFakeBroker(t *testing.T, rules *fakeRules) string {
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
			go serveFake(conn, rules)
		}
	}()
	return "mqtt://" + l.Addr().String()
}

func serveFake(conn net.Conn, rules *fakeRules) {
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
			if rules.silence.Add(-1) >= 0 {
				continue
			}
			out = suback(in.Content.(*packets.Subscribe), rules.refuse)
			if _, err := out.WriteTo(conn); err != nil {
				return
			}
			out = nil
			deliver(conn, rules.deliver)
		case packets.PUBACK:
			if rules.acked != nil {
				rules.acked <- in.Content.(*packets.Puback).PacketID
			}
			continue
		case packets.PUBLISH:
			pub := in.Content.(*packets.Publish)
			out = packets.NewControlPacket(packets.PUBACK)
			ack := out.Content.(*packets.Puback)
			ack.PacketID, ack.ReasonCode = pub.PacketID, rules.puback[pub.Topic]
		case packets.PINGREQ:
			out = packets.NewControlPacket(packets.PINGRESP)
		case packets.DISCONNECT:
			return
		default:
			continue
		}
		if out == nil {
			continue
		}
		if _, err := out.WriteTo(conn); err != nil {
			return
		}
	}
}

// deliver sends the client a message (QoS 1) on each topic, numbered from 1.
func deliver(conn net.Conn, topics []string) {
	for i, topic := range topics {
		out := packets.NewControlPacket(packets.PUBLISH)
		pub := out.Content.(*packets.Publish)
		pub.Topic, pub.QoS, pub.PacketID, pub.Payload = topic, 1, uint16(i+1), []byte("{}")
		if _, err := out.WriteTo(conn); err != nil {
			return
		}
	}
}

func suback(sub *packets.Subscribe, refuse map[string]bool) *packets.ControlPacket {
	out := packets.NewControlPacket(packets.SUBACK)
	ack := out.Content.(*packets.Suback)
	ack.PacketID = sub.PacketID
	for _, s := range sub.Subscriptions {
		reason := s.QoS
		if refuse[s.Topic] {
			reason = 0x87
		}
		ack.Reasons = append(ack.Reasons, reason)
	}
	return out
}

func TestARefusedSubscriptionDoesNotKeepTheLinkDown(t *testing.T) {
	// The broker refuses one filter: the others work, so the link is up. Before, the
	// client took the partial refusal for a failure and never said so: the agent kept
	// the cloud for unreachable and sent it nothing.
	url := startFakeBroker(t, &fakeRules{refuse: map[string]bool{"safehome/s1/forbidden": true}})
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

func TestASubscriptionLeftUnansweredIsAskedAgain(t *testing.T) {
	// The broker does not answer the first subscription in time. The connection is
	// alive: before, the client gave up until the next connection, and the agent kept
	// the cloud for unreachable while it was there.
	defer func(wait time.Duration) { subscribeWait = wait }(subscribeWait)
	subscribeWait = 200 * time.Millisecond
	rules := &fakeRules{}
	rules.silence.Store(1)
	url := startFakeBroker(t, rules)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	l := newLinks()
	c, err := Dial(ctx, Options{
		BrokerURL: url, ClientID: uniqueID("late"), OnLinkChange: l.change,
		Subscriptions: []Subscription{{Filter: "safehome/s1/alarm/cmd", QoS: 1}},
		OnMessage:     func(Message) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s := l.next(t); s != "up mqtt" {
		t.Fatalf("link %q, want up", s)
	}
	if rules.silence.Load() >= 0 {
		t.Fatal("subscribed only once")
	}
	c.Close(ctx)
}

func TestABrokerRefusalIsTold(t *testing.T) {
	url := startFakeBroker(t, &fakeRules{puback: map[string]byte{"s/forbidden": 0x87, "s/quota": 0x97}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := dial(t, ctx, url, uniqueID("pub"), nil, nil)
	defer c.Close(ctx)
	if err := c.Publish(ctx, Message{Topic: "s/fine", Payload: []byte("{}"), QoS: 1}); err != nil {
		t.Fatal(err)
	}
	err := c.Publish(ctx, Message{Topic: "s/forbidden", Payload: []byte("{}"), QoS: 1})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("not authorized: %v, want ErrRefused", err)
	}
	err = c.Publish(ctx, Message{Topic: "s/quota", Payload: []byte("{}"), QoS: 1})
	if err == nil || errors.Is(err, ErrRefused) || errors.Is(err, ErrDown) {
		t.Fatalf("quota exceeded: %v, want an error worth another try", err)
	}
}

// A message the application did not take (it is stopping) is not acknowledged, and
// holds back those after it: the broker gives them all again at the next connection.
// Without Take, every message is acknowledged.
func TestOnlyTheMessagesTakenAreAcknowledged(t *testing.T) {
	for name, want := range map[string][]uint16{"take": {1}, "on message": {1, 2, 3}} {
		t.Run(name, func(t *testing.T) {
			rules := &fakeRules{deliver: []string{"s/a", "s/stopping", "s/c"}, acked: make(chan uint16, 8)}
			url := startFakeBroker(t, rules)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			got := make(chan string, 8)
			o := Options{BrokerURL: url, ClientID: uniqueID("ack"), Subscriptions: []Subscription{{Filter: "s/#", QoS: 1}}}
			if name == "take" {
				stopping := false
				o.Take = func(m Message) bool {
					got <- m.Topic
					stopping = stopping || m.Topic == "s/stopping"
					return !stopping
				}
			} else {
				o.OnMessage = func(m Message) { got <- m.Topic }
			}
			c, err := Dial(ctx, o)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(ctx)
			for range 3 {
				<-got
			}
			var acked []uint16
			deadline := time.After(500 * time.Millisecond) // ten times the library's batching of acknowledgements
			for len(acked) < 3 {
				select {
				case id := <-rules.acked:
					acked = append(acked, id)
					continue
				case <-deadline:
				}
				break
			}
			if !slices.Equal(acked, want) {
				t.Fatalf("acknowledged %v, want %v", acked, want)
			}
		})
	}
}
