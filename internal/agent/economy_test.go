package agent

import (
	"testing"
	"time"

	"safehome/internal/contract"
)

func economy(c *Config) { c.Economy = EconomyEvery }

// minuteByMinute publishes one message a minute for n minutes, link up, and returns
// the minutes at which the cloud got one.
func (h *harness) minuteByMinute(n int, publish func(minute int)) []int {
	h.t.Helper()
	var got []int
	for m := range n {
		publish(m)
		if len(h.send()) > 0 {
			got = append(got, m)
		}
		h.now = h.now.Add(time.Minute)
	}
	return got
}

func (h *harness) heartbeat(minute int) {
	h.local(topic("system", "heartbeat"), contract.TypeHeartbeat, contract.HeartbeatData{BootID: "b", UptimeS: int64(minute * 60)})
}

func (h *harness) status(uplink, transport string) {
	b, _ := h.msg(contract.TypeAgentStatus, contract.AgentStatusData{Uplink: uplink, Transport: transport})
	h.r.Handle(Own{Now: h.now, Topic: topic("agent", "status"), Payload: b})
}

func TestOnAFixedLineEveryHeartbeatGoes(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	if got := h.minuteByMinute(6, h.heartbeat); len(got) != 6 {
		t.Fatalf("the cloud got heartbeats at minutes %v, want every minute", got)
	}
}

func TestOnMobileDataTheHeartbeatGoesEveryFiveMinutes(t *testing.T) {
	h := newHarness(t, economy)
	h.linkUp()
	got := h.minuteByMinute(12, h.heartbeat)
	if want := []int{0, 5, 10}; !equal(got, want) {
		t.Fatalf("the cloud got heartbeats at minutes %v, want %v", got, want)
	}
	// Other messages are never held back.
	sent := h.minuteByMinute(3, func(int) { h.local(topic("system", "health"), contract.TypeHealth, health) })
	if len(sent) != 3 {
		t.Fatalf("health went at minutes %v, want every one", sent)
	}
}

func TestOnMobileDataTheCloudGetsTheLatestHeartbeat(t *testing.T) {
	h := newHarness(t, economy)
	h.linkUp()
	h.heartbeat(0)
	h.send()
	// The link breaks for a while. Within the five minutes, nothing waits: the cloud
	// is not owed one yet. After, the waiting heartbeat is replaced by each newer one,
	// and the latest goes when the link returns.
	h.r.Handle(Link{Now: h.now, Up: false})
	for m := 1; m <= 8; m++ {
		h.now = h.now.Add(time.Minute)
		h.heartbeat(m)
		if m == 4 && len(h.r.Snapshot().Queue) != 0 {
			t.Fatal("a heartbeat waits though the cloud got one 4 minutes ago")
		}
	}
	h.linkUp()
	sent := h.send()
	if len(sent) != 1 {
		t.Fatalf("sent %d heartbeats, want the latest only", len(sent))
	}
	d, err := contract.DecodeData[contract.HeartbeatData](must(contract.ParseEnvelope(sent[0].Payload)))
	if err != nil || d.UptimeS != 480 {
		t.Fatalf("sent the heartbeat of uptime %d (%v), want the latest (480)", d.UptimeS, err)
	}
}

func TestOnMobileDataALinkChangeGoesAtOnce(t *testing.T) {
	h := newHarness(t, economy)
	h.linkUp()
	steps := []struct {
		uplink, transport string
		goes              bool
	}{
		{contract.UplinkUp, "mqtts", true},
		{contract.UplinkUp, "mqtts", false}, // a repeat
		{contract.UplinkUp, "wss", true},    // the agent turned to WebSocket
		{contract.UplinkUp, "wss", false},
	}
	for i, s := range steps {
		h.status(s.uplink, s.transport)
		if goes := len(h.send()) > 0; goes != s.goes {
			t.Fatalf("step %d (%s %s): sent %v, want %v", i, s.uplink, s.transport, goes, s.goes)
		}
		h.now = h.now.Add(time.Minute)
	}
}

func TestAfterARestartTheFirstHeartbeatGoesAtOnce(t *testing.T) {
	h := newHarness(t, economy)
	h.linkUp()
	h.heartbeat(0)
	h.send()
	h.r.Restore(h.r.Snapshot())
	h.linkUp()
	h.now = h.now.Add(time.Minute)
	h.heartbeat(1)
	if len(h.send()) != 1 {
		t.Fatal("the first heartbeat after a restart was held back: the cloud may not have heard from the site for a while")
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
