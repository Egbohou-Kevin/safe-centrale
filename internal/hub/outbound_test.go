package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/health"
)

// --- the broker goes away ------------------------------------------------------------

func TestMessagesWaitInOrderWhileTheBrokerIsDown(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 5
	r.broker.mu.Unlock()

	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.device(devBack, contract.CapContact, "open")
	r.settle() // returns once the outbox has emptied, retries included

	var seqs []uint64
	for _, m := range r.broker.all() {
		seqs = append(seqs, m.Env.Seq)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("messages out of order after the outage: %v", seqs)
		}
	}
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 1 || len(r.broker.on(contract.DeviceCmdTopic(site, devSiren))) != 1 {
		t.Fatal("nothing may be lost during an outage")
	}
	r.validateAll()
}

func TestTheAlarmDoesNotWaitForTheBroker(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 1 << 30 // the broker stays down
	r.broker.mu.Unlock()

	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.device(devBack, contract.CapContact, "open")
	// The engine moves on regardless.
	deadline := time.Now().Add(3 * time.Second)
	for r.mode() != engine.ModeTriggered {
		if time.Now().After(deadline) {
			t.Fatal("the engine is stuck behind the broker")
		}
		time.Sleep(time.Millisecond)
	}
	if r.hub.out.Depth() == 0 {
		t.Fatal("the messages must be waiting in the outbox")
	}
}

func TestARetainedStateIsReplacedWhileItWaits(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.failures = 1 << 30
	r.broker.mu.Unlock()
	for i := 0; i < 5; i++ {
		agentStatus(r, []string{contract.UplinkUp, contract.UplinkDown}[i%2])
	}
	r.settle2()
	// Five changes, but only the last retained health still waits (plus the one in flight).
	if d := r.hub.out.Depth(); d > 2 {
		t.Fatalf("%d messages wait, a retained topic must keep only its latest", d)
	}
}

// settle2 waits for the domains only: the outbox is stuck on purpose.
func (r *rig) settle2() {
	r.t.Helper()
	r.hub.monitor.loop.Do(context.Background(), func(time.Time) []health.Action { return nil })
}

func TestARefusedMessageIsDroppedAndTheNextOnesGoOut(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	r.broker.mu.Lock()
	r.broker.refuse = func(topic string) bool { return topic == contract.AlarmAckTopic(site) }
	r.broker.mu.Unlock()
	r.keypad(contract.ValArm, "")
	r.settle()
	if len(eventsOf(r, contract.TypeAlarmArming)) != 1 {
		t.Fatal("the arming event must still go out behind a refused ack")
	}
	if len(r.broker.on(contract.AlarmAckTopic(site))) != 0 {
		t.Fatal("the refused message must not appear")
	}
}

func TestOutboxDropsTheOldestWhenFull(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.Outbox.Size = 3 })
	// Not started: nothing leaves, the queue fills.
	for i := 0; i < 10; i++ {
		r.hub.enqueue("engine", contract.AlarmEventTopic(site), contract.TypeAlarmArmed, contract.ArmedData{Origin: "keypad"}, false, t0)
	}
	if d := r.hub.out.Depth(); d != 3 {
		t.Fatalf("depth %d, want 3", d)
	}
	if !strings.Contains(r.logs.String(), "outbox full") {
		t.Fatal("dropping must be logged")
	}
}

// --- heartbeat -----------------------------------------------------------------------

func TestHeartbeatEveryMinute(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.clock.Advance(time.Minute)
	r.waitFor("the first heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 1 })
	r.keypad(contract.ValArm, "")
	r.settle()
	r.clock.Advance(time.Minute)
	r.waitFor("the second heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 2 })
	hbs := eventsOf(r, contract.TypeHeartbeat)
	a, b := dataOf[contract.HeartbeatData](t, hbs[0]), dataOf[contract.HeartbeatData](t, hbs[1])
	if a.BootID == "" || a.BootID != b.BootID || a.UptimeS != 60 || b.UptimeS != 120 || a.Engine != "disarmed" || b.Engine != "arming" ||
		a.FW["engine"] != "0.1.0" {
		t.Fatalf("%+v %+v", a, b)
	}
	if hbs[0].Retain {
		t.Fatal("a heartbeat is not retained")
	}
	r.settle()
	r.validateAll()
}

func TestTheHeartbeatSaysWhetherTheClockIsRight(t *testing.T) {
	for _, c := range []struct {
		name          string
		synced, known bool
		want          string
	}{
		{"synchronised", true, true, `"clock_ok":true`},
		{"not yet", false, true, `"clock_ok":false`},
		{"cannot tell", false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.hub.deps.ClockSynced = func() (bool, bool) { return c.synced, c.known }
			r.start()
			r.clock.Advance(time.Minute)
			r.waitFor("heartbeat", func() bool { return len(eventsOf(r, contract.TypeHeartbeat)) == 1 })
			data := string(eventsOf(r, contract.TypeHeartbeat)[0].Env.Data)
			if c.want == "" && strings.Contains(data, "clock_ok") || c.want != "" && !strings.Contains(data, c.want) {
				t.Fatalf("want %q in %s", c.want, data)
			}
			r.settle()
			r.validateAll()
		})
	}
}

func TestANewBootGetsANewBootID(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.clock.Advance(time.Minute)
	first.waitFor("heartbeat", func() bool { return len(eventsOf(first, contract.TypeHeartbeat)) == 1 })
	first.stop()
	second := newRig(t, first.store)
	second.clock.Advance(time.Hour)
	second.start()
	second.clock.Advance(time.Minute)
	second.waitFor("heartbeat", func() bool { return len(eventsOf(second, contract.TypeHeartbeat)) == 1 })
	if dataOf[contract.HeartbeatData](t, eventsOf(first, contract.TypeHeartbeat)[0]).BootID ==
		dataOf[contract.HeartbeatData](t, eventsOf(second, contract.TypeHeartbeat)[0]).BootID {
		t.Fatal("a restart is a new boot: the cloud tells restarts apart from silence by it")
	}
}
