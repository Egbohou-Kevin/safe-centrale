package powerapp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/outbox/outboxtest"
	"safehome/internal/power"
	"safehome/internal/schematest"
)

const (
	site   = "tg-lom-0001"
	device = "dev_hubpower"
)

var t0 = time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)

// broker is the shared fake broker that refuses anything but a valid reading of the
// hub's device.
type broker struct{ outboxtest.Broker }

func (b *broker) Publish(ctx context.Context, topic string, payload []byte, qos byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		return err
	}
	d, err := contract.ValidData[contract.DeviceEventData](env)
	if err != nil || d.DeviceID != device {
		return errors.New("unexpected " + topic)
	}
	want := contract.DeviceEventTopic(site, device)
	if env.Type == contract.TypeDeviceState {
		want = contract.DeviceStateTopic(site, device, d.Capability)
	}
	if topic != want || retain != (env.Type == contract.TypeDeviceState) {
		return errors.New("unexpected " + topic)
	}
	if err := schematest.Check("device-event.json", env.Data); err != nil {
		return err
	}
	return b.Broker.Publish(ctx, topic, payload, qos, retain)
}

// told lists the values told as events for a capability, in order.
func (b *broker) told(capability string) []string {
	var out []string
	for _, m := range b.OfType(contract.TypeDeviceEvent) {
		if d, _ := contract.DecodeData[contract.DeviceEventData](m.Env); d.Capability == capability {
			out = append(out, string(d.Value))
		}
	}
	return out
}

type fakeBoard struct {
	mu       sync.Mutex
	lost     bool
	mv, pct  int
	gaugeErr error
	cuts     int
}

func (b *fakeBoard) MainsLost(context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lost, nil
}

func (b *fakeBoard) Gauge(context.Context) (int, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mv, b.pct, b.gaugeErr
}

func (b *fakeBoard) Cut(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cuts++
	return nil
}

func (b *fakeBoard) set(change func(*fakeBoard)) { b.mu.Lock(); change(b); b.mu.Unlock() }

func (b *fakeBoard) cutCount() int { b.mu.Lock(); defer b.mu.Unlock(); return b.cuts }

type rig struct {
	t      *testing.T
	clock  *drivertest.FakeClock
	broker *broker
	board  *fakeBoard
	logs   *drivertest.SyncBuffer
}

func start(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: drivertest.NewFakeClock(t0), broker: &broker{}, board: &fakeBoard{mv: 4100, pct: 95}, logs: drivertest.Logs(t)}
	app, err := New(DefaultConfig(site, device, power.DefaultConfig()), Deps{
		Clock: r.clock, Store: drivertest.NewMemStore(), Publisher: r.broker, Board: r.board,
		NewID: contract.NewIDGenerator().New, Logger: slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

// polls lets n readings go by.
func (r *rig) polls(n int) {
	r.t.Helper()
	for range n {
		r.clock.PassTimer(r.t, time.Second)
	}
}

func (r *rig) waitTold(capability string, values ...string) {
	r.t.Helper()
	want := strings.Join(values, " ")
	drivertest.WaitFor(r.t, capability+" "+want, func() bool { return strings.Join(r.broker.told(capability), " ") == want })
}

func TestTheReadingsAreToldAtOnceAndKept(t *testing.T) {
	r := start(t)
	r.waitTold(contract.CapMains, `"present"`)
	r.waitTold(contract.CapBattery, `95`)
	for _, c := range []string{contract.CapMains, contract.CapBattery} {
		// The state follows the event on the outbox: it may land just after.
		topic := contract.DeviceStateTopic(site, device, c)
		drivertest.WaitFor(t, c+" state", func() bool { return len(r.broker.On(topic)) > 0 })
		if st := r.broker.On(topic); len(st) != 1 || !st[0].Retain {
			t.Fatalf("%s state: %+v", c, st)
		}
	}
}

func TestACutIsToldAndANearlyEmptyCellShutsTheHubDown(t *testing.T) {
	r := start(t)
	r.waitTold(contract.CapMains, `"present"`)
	r.board.set(func(b *fakeBoard) { b.lost = true })
	r.polls(4)
	r.waitTold(contract.CapMains, `"present"`, `"lost"`)
	r.board.set(func(b *fakeBoard) { b.mv, b.pct = 2950, 1 })
	r.polls(31)
	drivertest.WaitFor(t, "the cut", func() bool { return r.board.cutCount() == 1 })
	r.waitTold(contract.CapBattery, `95`, `1`)
	if !strings.Contains(r.logs.String(), "nearly empty") {
		t.Fatalf("the cut must be logged:\n%s", r.logs)
	}
}

func TestAGaugeThatStopsAnsweringIsLoggedOnce(t *testing.T) {
	r := start(t)
	r.waitTold(contract.CapBattery, `95`)
	r.board.set(func(b *fakeBoard) { b.gaugeErr = errors.New("i2c: no answer") })
	r.polls(5)
	r.board.set(func(b *fakeBoard) { b.gaugeErr, b.pct = nil, 80 })
	r.polls(1)
	r.waitTold(contract.CapBattery, `95`, `80`)
	logs := r.logs.String()
	if strings.Count(logs, "does not answer") != 1 || strings.Count(logs, "answers again") != 1 {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestTheConfigurationIsChecked(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"site":   func(c *Config) { c.Site = "bad site" },
		"device": func(c *Config) { c.DeviceID = "" },
		"poll":   func(c *Config) { c.PollEvery = time.Minute },
		"power":  func(c *Config) { c.Power.BatteryStep = 0 },
	} {
		c := DefaultConfig(site, device, power.DefaultConfig())
		change(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The Pi has no battery clock: after a power cut it starts from the last time it knew,
// hours late, and NTP sets it forward a few seconds later (over the 4G, during the
// cut too). A cell low for a few seconds does not shut the hub down for it: the cut
// waits the 30 s that really pass.
func TestAClockSetForwardCutsNothingEarly(t *testing.T) {
	r := start(t)
	r.waitTold(contract.CapMains, `"present"`)
	r.board.set(func(b *fakeBoard) { b.lost = true })
	r.polls(4)
	r.waitTold(contract.CapMains, `"present"`, `"lost"`)
	r.board.set(func(b *fakeBoard) { b.mv, b.pct = 2950, 1 })
	r.polls(5)
	r.clock.Set(r.clock.Now().Add(14 * time.Hour)) // NTP
	r.polls(5)
	if n := r.board.cutCount(); n != 0 {
		t.Fatalf("%d cuts, 10 s after the cell went low", n)
	}
	r.polls(21)
	drivertest.WaitFor(t, "the cut", func() bool { return r.board.cutCount() == 1 })
}
