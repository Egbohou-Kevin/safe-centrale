package deploy

// The rights on the hub's broker (29 October 2026): mosquitto/safehome.acl gives each
// name exactly the topics its code reads and writes. The tests of cmd/ run each program
// against this file (internal/adapter/mqtt/mqtttest); these check the file itself, then
// what the real Mosquitto does with it.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/driver/drivertest"
)

// rights are the patterns a section of the ACL may read and write.
type rights struct{ read, write []string }

// readACL parses mosquitto/safehome.acl, failing on anything but a comment, a section
// (user NAME) or a topic line inside one: no pattern line, no line for anonymous clients.
func readACL(t *testing.T) (names []string, acl map[string]*rights) {
	t.Helper()
	acl = map[string]*rights{}
	var cur *rights
	for i, line := range strings.Split(string(must(os.ReadFile(filepath.Join("mosquitto", "safehome.acl")))), "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 0 || strings.HasPrefix(f[0], "#"):
		case len(f) == 2 && f[0] == "user":
			if acl[f[1]] != nil {
				t.Fatalf("safehome.acl:%d: a second section for %s", i+1, f[1])
			}
			cur = &rights{}
			acl[f[1]], names = cur, append(names, f[1])
		case len(f) == 3 && f[0] == "topic" && cur != nil:
			switch f[1] {
			case "read":
				cur.read = append(cur.read, f[2])
			case "write":
				cur.write = append(cur.write, f[2])
			case "readwrite":
				cur.read, cur.write = append(cur.read, f[2]), append(cur.write, f[2])
			default:
				t.Fatalf("safehome.acl:%d: %q", i+1, line)
			}
		default:
			t.Fatalf("safehome.acl:%d: %q: only sections and their topic lines (no pattern, nothing for anonymous clients)", i+1, line)
		}
	}
	return names, acl
}

// matches reports whether the MQTT filter matches topic.
func matches(filter, topic string) bool {
	f, tp := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, level := range f {
		if level == "#" {
			return true
		}
		if i >= len(tp) || (level != "+" && level != tp[i]) {
			return false
		}
	}
	return len(f) == len(tp)
}

// who returns the names whose patterns match topic, in the ACL's order.
func who(names []string, acl map[string]*rights, topic string, of func(*rights) []string) []string {
	var got []string
	for _, n := range names {
		if slices.ContainsFunc(of(acl[n]), func(p string) bool { return matches(p, topic) }) {
			got = append(got, n)
		}
	}
	slices.Sort(got)
	return got
}

// Each name install.sh makes a password for has its section, and only those; none
// writes a whole tree of safehome/.
func TestEachNameOnTheBrokerHasItsRights(t *testing.T) {
	names, acl := readACL(t)
	want := mqttNames(t)
	got := slices.Clone(names)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("sections %v, want one per name of the broker's password file: %v", got, want)
	}
	if acl[mqtttest.Peer] != nil {
		t.Errorf("%s is the tests' own name: no hub has it", mqtttest.Peer)
	}
	for _, n := range names {
		for _, p := range acl[n].write {
			if p == "#" || (strings.HasPrefix(p, "safehome/") && strings.Contains(p, "#")) {
				t.Errorf("%s writes %s: a name writes its own topics, never a tree of them", n, p)
			}
		}
	}
	if conf := readLines(t, filepath.Join("mosquitto", "mosquitto.conf")); directive(t, conf, "acl_file") != "/etc/mosquitto/safehome.acl" {
		t.Error("mosquitto.conf does not name /etc/mosquitto/safehome.acl, where install.sh puts the ACL")
	}
}

// Who writes and who reads each topic of the hub's broker, from the code
// (~/.claude/etudes/safeHome/topics-par-programme.md, section 11, at 290072f). The operator
// reads everything; an order (alarm/cmd, maint/cmd, config/set...) has its senders
// alone. A topic whose writers or readers in the ACL differ from here fails, and so does
// a line of the ACL that gives a name a topic no line here gives it. The test does not
// read the code: a topic added there and not to the ACL is seen by the tests of cmd/.
func TestEachTopicHasItsWritersAndReaders(t *testing.T) {
	names, acl := readACL(t)
	const s = "safehome/tg-lom-0001/"
	table := []struct {
		topic, writers, readers string
	}{
		{s + "device/dev_door/event", "safehome-power safehome-zigbee", "operator safehome-agent safehome-console safehome-hub"},
		{s + "device/dev_door/state/contact", "safehome-power safehome-zigbee", "operator safehome-hub"},
		{s + "device/dev_siren1/cmd", "safehome-hub", "operator safehome-zigbee"},
		{s + "device/dev_siren1/ack", "safehome-zigbee", "operator safehome-hub"},
		{s + "alarm/cmd", "operator safehome-agent", "operator safehome-hub"},
		{s + "alarm/event", "safehome-hub", "operator safehome-agent safehome-gsm safehome-video"},
		{s + "alarm/ack", "safehome-hub", "operator safehome-agent"},
		{s + "alarm/state", "safehome-hub", "operator safehome-agent safehome-hub"},
		{s + "alert/request", "safehome-hub", "operator safehome-agent safehome-gsm"},
		{s + "alert/result", "safehome-agent safehome-gsm", "operator safehome-agent safehome-hub"},
		{s + "alert/ack", "safehome-agent safehome-gsm", "operator safehome-agent safehome-gsm safehome-hub"},
		{s + "alert/seen", "safehome-agent", "operator safehome-hub"},
		{s + "gsm/status", "safehome-gsm", "operator safehome-hub"},
		{s + "zigbee/status", "safehome-zigbee", "operator safehome-console safehome-hub"},
		{s + "agent/status", "safehome-agent", "operator safehome-hub"},
		{s + "agent/replayed", "safehome-agent", "operator"},
		{s + "agent/cmd", "", "operator"},
		{s + "install/cmd", "operator safehome-console", "operator safehome-hub"},
		{s + "install/mode", "safehome-hub", "operator safehome-agent safehome-console safehome-hub"},
		{s + "registry/cmd", "operator safehome-console", "operator safehome-zigbee"},
		{s + "registry/event", "safehome-zigbee", "operator safehome-console safehome-hub"},
		{s + "registry/devices", "safehome-zigbee", "operator safehome-console safehome-hub"},
		{s + "system/health", "safehome-hub", "operator safehome-agent"},
		{s + "system/devices", "safehome-hub", "operator safehome-agent"},
		{s + "system/contacts", "safehome-hub", "operator safehome-agent"},
		{s + "system/heartbeat", "safehome-hub", "operator safehome-agent"},
		{s + "config/set", "safehome-agent", "operator safehome-hub"},
		{s + "config/applied", "safehome-hub", "operator safehome-agent safehome-gsm safehome-video"},
		{s + "video/cmd", "safehome-agent", "operator safehome-video"},
		{s + "video/session", "safehome-video", "operator safehome-agent"},
		{s + "video/status", "safehome-video", "operator safehome-agent"},
		{s + "photo/cmd", "", "operator"},
		{s + "photo/taken", "safehome-video", "operator safehome-agent"},
		{s + "photo/sent", "safehome-agent", "operator safehome-agent"},
		{s + "maint/cmd", "safehome-agent", "operator safehome-maint"},
		{s + "maint/session", "safehome-maint", "operator safehome-agent"},
		{"zigbee2mqtt/bridge/state", "safehome-zigbee zigbee2mqtt", "operator safehome-zigbee zigbee2mqtt"},
		{"zigbee2mqtt/0x00158d0001a2b3c4/set", "safehome-zigbee zigbee2mqtt", "operator safehome-zigbee zigbee2mqtt"},
		{"elsewhere/x", "", ""},
	}
	for _, c := range table {
		if got, want := who(names, acl, c.topic, func(r *rights) []string { return r.write }), strings.Fields(c.writers); !slices.Equal(got, want) {
			t.Errorf("%s written by %v, want %v", c.topic, got, want)
		}
		if got, want := who(names, acl, c.topic, func(r *rights) []string { return r.read }), strings.Fields(c.readers); !slices.Equal(got, want) {
			t.Errorf("%s read by %v, want %v", c.topic, got, want)
		}
	}
	// Each line of the ACL is needed by a topic the table gives that name that way.
	needed := func(n, p string, writing bool) bool {
		for _, c := range table {
			who := c.readers
			if writing {
				who = c.writers
			}
			if matches(p, c.topic) && slices.Contains(strings.Fields(who), n) {
				return true
			}
		}
		return false
	}
	for _, n := range names {
		for _, p := range acl[n].write {
			if !needed(n, p, true) {
				t.Errorf("%s writes %s: no topic of the table needs it (add the topic to the table, or drop the line)", n, p)
			}
		}
		for _, p := range acl[n].read {
			if !needed(n, p, false) {
				t.Errorf("%s reads %s: no topic of the table needs it (add the topic to the table, or drop the line)", n, p)
			}
		}
	}
}

// What Mosquitto does with the ACL (2.1.2, measured 29 October 2026): a message a name
// may not write is refused (PUBACK 0x87, ErrRefused for the client); a subscription to
// a topic it may not read is granted, and nothing is ever delivered on it.
func TestTheBrokerHoldsEachNameToItsRights(t *testing.T) {
	broker := mqtttest.Start(t, "safehome-power")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const s = "safehome/tg-lom-0001/"

	// A power board taken over sends no order; it still sends its own news.
	power := dialAs(t, ctx, broker, "safehome-power", nil, nil)
	for _, topic := range []string{s + "alarm/cmd", s + "maint/cmd", s + "config/set"} {
		if err := power.Publish(ctx, mqtt.Message{Topic: topic, Payload: []byte("{}"), QoS: 1}); !errors.Is(err, mqtt.ErrRefused) {
			t.Errorf("safehome-power published on %s: %v, want it refused", topic, err)
		}
	}
	if err := power.Publish(ctx, mqtt.Message{Topic: s + "device/dev_power/event", Payload: []byte("{}"), QoS: 1}); err != nil {
		t.Errorf("safehome-power cannot send its own event: %v", err)
	}
	if err := power.Publish(ctx, mqtt.Message{Topic: s + "device/dev_power/state/mains", Payload: []byte("{}"), QoS: 1, Retain: true}); err != nil {
		t.Errorf("safehome-power cannot keep its own state: %v", err)
	}

	// The Zigbee adapter clears a retained state of a device it removes.
	zigbee := dialAs(t, ctx, broker, "safehome-zigbee", nil, nil)
	if err := zigbee.Publish(ctx, mqtt.Message{Topic: s + "device/dev_door/state/contact", QoS: 1, Retain: true}); err != nil {
		t.Errorf("safehome-zigbee cannot clear a retained state: %v", err)
	}

	// The video gateway never hears a maintenance order, though it may subscribe.
	var mu sync.Mutex
	var heard []string
	dialAs(t, ctx, broker, "safehome-video",
		[]mqtt.Subscription{{Filter: s + "maint/cmd", QoS: 1}, {Filter: s + "video/cmd", QoS: 1}},
		func(m mqtt.Message) { mu.Lock(); heard = append(heard, m.Topic); mu.Unlock() })
	peer := dialAs(t, ctx, broker, mqtttest.Peer, nil, nil)
	for _, topic := range []string{s + "maint/cmd", s + "video/cmd"} {
		if err := peer.Publish(ctx, mqtt.Message{Topic: topic, Payload: []byte("{}"), QoS: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// In order from one sender: once video/cmd arrived, maint/cmd would have too.
	drivertest.WaitFor(t, "video/cmd at the video gateway", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(heard, s+"video/cmd")
	})
	mu.Lock()
	defer mu.Unlock()
	if slices.Contains(heard, s+"maint/cmd") {
		t.Error("safehome-video received maint/cmd")
	}
}

// dialAs connects to broker as name, with its test password, and waits until it is
// connected and subscribed.
func dialAs(t *testing.T, ctx context.Context, broker *mqtttest.Broker, name string, subs []mqtt.Subscription, on func(mqtt.Message)) *mqtt.Client {
	t.Helper()
	subscribed := make(chan struct{}, 1)
	c, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("%s-%d", name, time.Now().UnixNano()),
		Username: name, Password: mqtttest.Password(name), Ephemeral: true,
		Subscriptions: subs, OnMessage: on,
		OnLinkChange: func(up bool, _ string) {
			if up {
				select {
				case subscribed <- struct{}{}:
				default:
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		c.Close(closeCtx)
	})
	select {
	case <-subscribed:
	case <-ctx.Done():
		t.Fatalf("%s cannot connect", name)
	}
	return c
}
