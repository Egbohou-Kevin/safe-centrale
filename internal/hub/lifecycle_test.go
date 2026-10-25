package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/access"
	"safehome/internal/contract"
	"safehome/internal/driver"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
	"safehome/internal/inbox"
	"safehome/internal/outbox/outboxtest"
)

// --- boot and restart ---------------------------------------------------------------

func TestBootLearnsTheDevicesFromRetainedStates(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 50 * time.Millisecond })
	// The retained states arrive before Run's window ends: the back door was left open.
	state := func(id, capability, value string) {
		r.send(contract.DeviceStateTopic(site, id, capability), contract.TypeDeviceState, event(id, capability, value))
	}
	state(devBack, contract.CapContact, "open")
	state(devDoor, contract.CapContact, "closed")
	state(devHub, contract.CapBattery, "88")
	state(devHub, contract.CapMains, "lost")
	r.start()
	r.settle()

	if open := r.engineSnapshot().Open; !open[devBack] || open[devDoor] {
		t.Fatalf("open doors after boot: %v", open)
	}
	// The engine now refuses to arm with the back door open.
	r.keypad(contract.ValArm, "")
	r.settle()
	if got := lastAck(t, r); got.Status != contract.AckRefused || got.Reason != contract.ReasonZoneOpen {
		t.Fatalf("%+v", got)
	}
	// And the health knows the battery and the power cut, learned from states too.
	h := r.broker.On(contract.HealthTopic(site))
	if d := dataOf[contract.HealthData](t, h[len(h)-1]); d.HubBattery == nil || *d.HubBattery != 88 || d.Mains != contract.ValLost {
		t.Fatalf("%+v", d)
	}
}

func TestBootWaitsForTheLinkBeforeItsWindowStarts(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 20 * time.Millisecond; c.LinkWait = 10 * time.Second })
	r.startWithoutWaiting()
	r.clock.WaitTimer(t, t0.Add(10*time.Second))
	r.clock.Advance(time.Second) // far longer than the window
	r.settle()
	if len(r.broker.On(contract.AlarmStateTopic(site))) != 0 || r.clock.HasTimer(t0.Add(time.Second+20*time.Millisecond)) {
		t.Fatal("the hub booted before the link was up")
	}
	// The retained state arrives only now, after the window would have ended.
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.hub.LinkUp()
	r.hub.LinkUp() // harmless twice
	r.clock.PassTimer(t, 20*time.Millisecond)
	drivertest.WaitFor(t, "the first state", func() bool { return len(r.broker.On(contract.AlarmStateTopic(site))) > 0 })
	if !r.engineSnapshot().Open[devBack] {
		t.Fatal("the state that arrived with the link was not learned")
	}
}

func TestBootGivesUpWaitingForTheLink(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 10 * time.Millisecond; c.LinkWait = 80 * time.Millisecond })
	r.startWithoutWaiting()
	r.clock.PassTimer(t, 80*time.Millisecond)
	r.clock.PassTimer(t, 10*time.Millisecond)
	drivertest.WaitFor(t, "the boot without a link", func() bool { return len(r.broker.On(contract.AlarmStateTopic(site))) > 0 })
	if !strings.Contains(r.logs.String(), "the broker link did not come up in time") {
		t.Fatalf("the late link is said:\n%s", r.logs.String())
	}
}

func TestStatesAfterBootAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.settle()
	if r.engineSnapshot().Open[devBack] {
		t.Fatal("after boot only events carry changes")
	}
}

func TestARestartKeepsTheAlarmArmedAndTheCodes(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.setCode("u_owner", "482915")
	first.keypad(contract.ValArm, "")
	first.keypad(contract.ValArmNow, "")
	first.mustBe(engine.ModeArmed)
	first.stop()

	second := newRig(t, first.store)
	second.clock.Advance(time.Hour)
	second.start()
	second.mustBe(engine.ModeArmed)
	// The code survived: the owner can disarm.
	second.keypad(contract.ValDisarm, "482915")
	drivertest.WaitFor(t, "disarmed", func() bool { return len(eventsOf(second, contract.TypeAlarmDisarmed)) == 1 })
	second.mustBe(engine.ModeDisarmed)
}

// The entry door was opened during the exit delay and closed while the hub was off:
// the exit delay that ended meanwhile must see it closed, from its retained state, and
// arm quietly instead of starting the entry delay.
func TestARestartAfterTheExitDelayReadsTheDoorsFirst(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.keypad(contract.ValArm, "")
	first.device(devDoor, contract.CapContact, "open")
	first.mustBe(engine.ModeArming)
	first.stop()

	second := newRig(t, first.store, func(c *Config) { c.BootWindow = 50 * time.Millisecond })
	second.clock.Advance(10 * time.Minute)
	second.send(contract.DeviceStateTopic(site, devDoor, contract.CapContact), contract.TypeDeviceState, event(devDoor, contract.CapContact, "closed"))
	second.start()
	second.settle()
	second.mustBe(engine.ModeArmed)
	if n := len(eventsOf(second, contract.TypeAlarmEntryDelay)) + len(eventsOf(second, contract.TypeAlarmTriggered)); n != 0 {
		t.Fatalf("a door closed during the outage must not start the entry delay: %d events", n)
	}
}

// After an unclean stop the broker gives back the retained states of its last save,
// minutes old: the back door, closed before the arming, comes back open. Dated before
// the closing the engine heard, it is no opening; dated after, while the hub was off,
// it is one.
func TestARestartGivesNoCreditToARetainedStateOlderThanTheEngine(t *testing.T) {
	for _, c := range []struct {
		name  string
		since time.Duration // from the first rig's start
		rings bool
	}{{"stale", 0, false}, {"opened during the outage", 30 * time.Minute, true}} {
		t.Run(c.name, func(t *testing.T) {
			first := newRig(t, nil)
			first.start()
			first.device(devBack, contract.CapContact, "open")
			first.clock.Advance(time.Minute)
			first.device(devBack, contract.CapContact, "closed")
			first.keypad(contract.ValArm, "")
			first.keypad(contract.ValArmNow, "")
			first.mustBe(engine.ModeArmed)
			first.stop()

			second := newRig(t, first.store, func(c *Config) { c.BootWindow = 50 * time.Millisecond })
			second.clock.Advance(time.Hour)
			open := event(devBack, contract.CapContact, "open")
			open.Since = contract.TSPtr(t0.Add(c.since))
			second.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, open)
			second.start()
			second.settle()
			if got := len(eventsOf(second, contract.TypeAlarmTriggered)) == 1; got != c.rings {
				t.Fatalf("alarm %v, want %v", got, c.rings)
			}
		})
	}
}

func TestSequenceNumbersOnlyGoUpAcrossRestarts(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	first.keypad(contract.ValArm, "")
	first.settle()
	first.stop()
	var last uint64
	prev := uint64(0)
	for _, m := range first.broker.All() {
		if m.Env.Seq <= prev {
			t.Fatalf("seq %d after %d", m.Env.Seq, prev)
		}
		prev = m.Env.Seq
		last = m.Env.Seq
	}
	if last == 0 {
		t.Fatal("nothing was published")
	}

	second := newRig(t, first.store)
	second.start()
	second.settle()
	for _, m := range second.broker.All() {
		if m.Env.Seq <= last {
			t.Fatalf("after a restart, seq %d goes back under %d", m.Env.Seq, last)
		}
	}
}

// --- construction and shutdown ----------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if err := testConfig().validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"bad site":        func(c *Config) { c.Site = "Lome" },
		"no hub device":   func(c *Config) { c.HubDeviceID = "" },
		"no siren":        func(c *Config) { c.SirenIDs = nil },
		"empty siren id":  func(c *Config) { c.SirenIDs = []string{""} },
		"no heartbeat":    func(c *Config) { c.HeartbeatEvery = 0 },
		"no siren ttl":    func(c *Config) { c.SirenCommandTTL = 0 },
		"retry inverted":  func(c *Config) { c.Outbox.RetryMax = c.Outbox.RetryMin / 2 },
		"negative window": func(c *Config) { c.BootWindow = -1 },
		"negative wait":   func(c *Config) { c.LinkWait = -1 },
		"no dedupe":       func(c *Config) { c.DedupeSize = 0 },
		"no outbox":       func(c *Config) { c.Outbox.Size = 0 },
		"no auth queue":   func(c *Config) { c.AuthQueue = 0 },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(&c)
		if _, err := New(c, Deps{Clock: drivertest.NewFakeClock(t0), Store: drivertest.NewMemStore(), Publisher: &outboxtest.Broker{}, NewID: func() string { return "x" }}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := New(testConfig(), Deps{}); err == nil {
		t.Error("the ports are required")
	}
	c := testConfig()
	c.SirenIDs = nil
	if err := c.Validate(); !errors.Is(err, errNoSiren) {
		t.Errorf("no siren: %v, want errNoSiren", err)
	}
}

func TestNewRefusesADamagedSavedState(t *testing.T) {
	for _, key := range []string{keyEngine, keyHealth, keyAccess, keySeq} {
		store := drivertest.NewMemStore()
		store.Save(key, []byte(`{"broken": [`))
		clock := drivertest.NewFakeClock(t0)
		_, err := New(testConfig(), Deps{Clock: clock, Store: store, Publisher: &outboxtest.Broker{}, NewID: contract.NewIDGeneratorWith(clock.Now, &entropy{}).New})
		if err == nil {
			t.Errorf("a damaged %s must stop the start", key)
		}
	}
}

func TestRunTwiceFailsAndShutdownFlushes(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	if err := r.hub.Run(context.Background()); err == nil {
		t.Fatal("Run twice must fail")
	}
	r.keypad(contract.ValArm, "")
	r.settle()
	r.stop()
	if len(eventsOf(r, contract.TypeAlarmArming)) != 1 {
		t.Fatal("the last events must leave before shutdown")
	}
	// After shutdown the hub gives up quietly.
	r.hub.HandleMessage(inbox.Message{Topic: contract.AlarmCmdTopic(site), Payload: mustEnv(t, r, contract.TypeAlarmCommand,
		contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp})})
}

// A message that arrives while the hub stops is given up without an error: the loops
// stop only once the hub's life is over.
func TestMessagesDuringShutdownAreGivenUpQuietly(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	release := r.broker.Hold() // the last flush waits: the hub stays in its shutdown
	r.keypad(contract.ValArm, "")
	r.settle2()

	r.cancel()
	drivertest.WaitFor(t, "the engine to stop", func() bool {
		return errors.Is(r.hub.alarm.loop.Alive(context.Background()), driver.ErrStopped)
	})
	r.keypad(contract.ValArmNow, "")
	r.device(devBack, contract.CapBattery, "50")
	release()
	<-r.done
	r.cancel = nil
	if strings.Contains(r.logs.String(), "failed") {
		t.Fatalf("an error was logged for a message given up at shutdown:\n%s", r.logs.String())
	}
}

// An alarm the engine raises while the hub stops (a message it had already taken)
// is saved as raised: its alert must still reach the escalation, and leave, or nobody
// is ever told of it, not even after the restart.
func TestAnAlarmRaisedWhileTheHubStopsIsStillAlerted(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.armedNow()
	held, hold := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(hold) })
	t.Cleanup(release) // runs before the rig's own, which waits for the hub
	r.hub.alarm.loop.Post(context.Background(), func(time.Time) []engine.Action {
		close(held)
		<-hold
		return nil
	})
	<-held
	r.device(devBack, contract.CapContact, "open") // taken, behind the held task
	r.cancel()
	// The escalation must outlive the engine while the engine has work left.
	for end := time.Now().Add(100 * time.Millisecond); time.Now().Before(end); {
		if err := r.hub.escal.loop.Alive(context.Background()); err != nil {
			t.Fatalf("the escalation stopped before the engine: %v", err)
		}
	}
	release()
	<-r.done
	r.cancel = nil
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 1 || len(eventsOf(r, contract.TypeAlertRequest)) == 0 {
		t.Fatalf("triggered %d, alert requests %d: the alarm must be alerted",
			len(eventsOf(r, contract.TypeAlarmTriggered)), len(eventsOf(r, contract.TypeAlertRequest)))
	}
}

func TestFloodOfCodeChecksDoesNotStallTheEngine(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.AuthQueue = 2 })
	r.start()
	r.setCode("u_owner", "482915")
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	for i := 0; i < 50; i++ {
		r.keypad(contract.ValDisarm, fmt.Sprintf("%06d", 100000+i*7))
	}
	r.settle()
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmAuthFailed)) == 0 {
		t.Fatal("the flood must be counted as wrong codes")
	}
}

func TestSubscriptions(t *testing.T) {
	r := newRig(t, nil)
	want := map[string]bool{
		"safehome/" + site + "/device/+/event":       true,
		"safehome/" + site + "/device/+/state/+":     true,
		"safehome/" + site + "/device/dev_siren/ack": true,
		"safehome/" + site + "/alarm/cmd":            true,
		"safehome/" + site + "/alert/result":         true,
		"safehome/" + site + "/alert/ack":            true,
		"safehome/" + site + "/gsm/status":           true,
		"safehome/" + site + "/zigbee/status":        true,
		"safehome/" + site + "/agent/status":         true,
		"safehome/" + site + "/install/cmd":          true,
		"safehome/" + site + "/registry/event":       true,
		"safehome/" + site + "/registry/devices":     true,
	}
	for _, s := range r.hub.Subscriptions() {
		if !want[s.Filter] || s.QoS != 1 {
			t.Errorf("unexpected subscription %+v", s)
		}
		delete(want, s.Filter)
	}
	if len(want) != 0 {
		t.Errorf("missing subscriptions: %v", want)
	}
}

func TestAccessRegistryIsSharedWithTheInstaller(t *testing.T) {
	r := newRig(t, nil)
	r.setCode("u_owner", "482915")
	data, found, _ := r.store.Load(keyAccess)
	if !found {
		t.Fatal("a code must be saved as soon as it is set")
	}
	var recs []access.Record
	if err := json.Unmarshal(data, &recs); err != nil || len(recs) != 1 {
		t.Fatalf("%v %s", err, data)
	}
	if strings.Contains(string(data), "482915") {
		t.Fatal("the code must never be written in clear")
	}
}

// A device may not speak for another: the topic says who is talking, and the
// content must agree, or a compromised device could trip a zone that is not its own.
func TestADeviceMessageForAnotherDeviceIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	// "dev_motion" publishes on its own topic a message that claims to be the back door.
	r.send(contract.DeviceEventTopic(site, devMotion), contract.TypeDeviceEvent, event(devBack, contract.CapContact, "open"))
	r.settle()
	r.mustBe(engine.ModeArmed)
	if len(eventsOf(r, contract.TypeAlarmTriggered)) != 0 {
		t.Fatal("the zone of another device must not be tripped")
	}
	// The same message on the right topic does trip it.
	r.device(devBack, contract.CapContact, "open")
	r.mustBe(engine.ModeTriggered)
}

func TestEachCapabilityKeepsItsOwnRetainedState(t *testing.T) {
	// Two retained states of one device: the battery must not hide the open door.
	r := newRig(t, nil)
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapContact), contract.TypeDeviceState, event(devBack, contract.CapContact, "open"))
	r.send(contract.DeviceStateTopic(site, devBack, contract.CapBattery), contract.TypeDeviceState, event(devBack, contract.CapBattery, "80"))
	// A state on the topic of another capability is refused.
	r.send(contract.DeviceStateTopic(site, devDoor, contract.CapBattery), contract.TypeDeviceState, event(devDoor, contract.CapContact, "open"))
	r.start()
	drivertest.WaitFor(t, "the first state", func() bool { return len(r.broker.On(contract.AlarmStateTopic(site))) > 0 })
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	drivertest.WaitFor(t, "the reply", func() bool { return len(eventsOf(r, contract.TypeAlarmAck)) == 1 })
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	devs, _ := ack.Detail["devices"].([]any)
	if ack.Status != contract.AckRefused || len(devs) != 1 || devs[0] != devBack {
		t.Fatalf("only the back door is open: %+v", ack)
	}
}
