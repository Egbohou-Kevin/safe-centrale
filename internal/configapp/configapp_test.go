package configapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

type broker struct {
	mu      sync.Mutex
	answers []contract.ConfigAppliedData
	retain  []bool
}

func (b *broker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		return err
	}
	if topic != contract.ConfigAppliedTopic(site) || env.Type != contract.TypeConfigApplied || env.Src != "config" {
		return errors.New("unexpected " + topic)
	}
	if err := schematest.Check("config-applied.json", env.Data); err != nil {
		return err
	}
	d, _ := contract.DecodeData[contract.ConfigAppliedData](env)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.answers = append(b.answers, d)
	b.retain = append(b.retain, retain)
	return nil
}

func (b *broker) last() (contract.ConfigAppliedData, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.answers) == 0 {
		return contract.ConfigAppliedData{}, 0
	}
	return b.answers[len(b.answers)-1], len(b.answers)
}

type rig struct {
	t        *testing.T
	app      *App
	broker   *broker
	logs     *drivertest.SyncBuffer
	clock    *drivertest.FakeClock
	ids      *contract.IDGenerator
	mu       sync.Mutex
	saved    []contract.ConfigSetData
	restarts int
	checkErr error
	saveErr  error
	devices  bool // the installer's tools changed the zones or the sirens
}

func start(t *testing.T, cfg Config) *rig {
	t.Helper()
	r := &rig{t: t, broker: &broker{}, logs: drivertest.Logs(t), clock: drivertest.NewFakeClock(t0), ids: contract.NewIDGenerator()}
	cfg.Outbox.RetryMin, cfg.Outbox.RetryMax = 5*time.Millisecond, 20*time.Millisecond
	app, err := New(cfg, Deps{
		Clock: r.clock, Store: drivertest.NewMemStore(), Publisher: r.broker, NewID: r.ids.New,
		Check: func(contract.ConfigSetData) error { r.mu.Lock(); defer r.mu.Unlock(); return r.checkErr },
		Save: func(d contract.ConfigSetData) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.saveErr != nil {
				return r.saveErr
			}
			r.saved = append(r.saved, d)
			return nil
		},
		Restart:        func() { r.mu.Lock(); r.restarts++; r.mu.Unlock() },
		DevicesChanged: func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.devices },
		Logger:         slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func (r *rig) send(topic, typ string, data any) {
	r.t.Helper()
	env, err := contract.NewEnvelope(site, "cloud", 1, r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	r.app.HandleMessage(inbox.Message{Topic: topic, Payload: b})
}

func (r *rig) config(d contract.ConfigSetData) {
	r.send(contract.ConfigSetTopic(site), contract.TypeConfigSet, d)
}

func (r *rig) alarm(state string) {
	r.send(contract.AlarmStateTopic(site), contract.TypeAlarmState, contract.AlarmStateData{State: state, Since: contract.TS(r.clock.Now())})
}

// install publishes the installation mode as the hub does: open when reason is empty.
func (r *rig) install(reason string) {
	d := contract.InstallModeData{Status: contract.InstallClosed, Reason: reason}
	if reason == "" {
		d = contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(r.clock.Now().Add(30 * time.Minute))}
	}
	r.send(contract.InstallModeTopic(site), contract.TypeInstallMode, d)
}

func (r *rig) changeDevices() { r.mu.Lock(); r.devices = true; r.mu.Unlock() }

func (r *rig) restarted() int { r.mu.Lock(); defer r.mu.Unlock(); return r.restarts }

func (r *rig) waitAnswer(n int) contract.ConfigAppliedData {
	r.t.Helper()
	drivertest.WaitFor(r.t, fmt.Sprintf("answer %d", n), func() bool { _, got := r.broker.last(); return got >= n })
	d, _ := r.broker.last()
	return d
}

// sent waits until every answer given so far has left: HandleMessage puts an answer
// in the outbox before it returns.
func (r *rig) sent() {
	r.t.Helper()
	drivertest.WaitFor(r.t, "the outbox to empty", func() bool { return r.app.out.Depth() == 0 })
}

func phoneConfig(version int) contract.ConfigSetData {
	return contract.ConfigSetData{Version: version, Contacts: []contract.ConfigContact{
		{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"sms"}, Phone: "+22890000001"},
	}}
}

func TestAConfigurationIsKeptThenAppliedAtAQuietMoment(t *testing.T) {
	r := start(t, DefaultConfig(site, 3))
	if d := r.waitAnswer(1); d.Version != 3 || d.Status != contract.ConfigApplied {
		t.Fatalf("at start, the version running: %+v", d)
	}
	r.alarm(contract.StateTriggered)
	r.config(phoneConfig(4))
	if len(r.saved) != 1 || r.saved[0].Version != 4 {
		t.Fatalf("%+v", r.saved)
	}
	if r.restarted() != 0 {
		t.Fatal("never during an alarm")
	}
	r.alarm(contract.StateEntryDelay)
	if r.restarted() != 0 {
		t.Fatal("never during a delay")
	}
	r.config(phoneConfig(4)) // the cloud asks again: kept already
	if len(r.saved) != 1 {
		t.Fatal("kept once")
	}
	r.alarm(contract.StateDisarmed)
	if r.restarted() != 1 {
		t.Fatal("restart once quiet")
	}
	r.alarm(contract.StateArmed)
	if r.restarted() != 1 {
		t.Fatal("once only")
	}
	if _, n := r.broker.last(); n != 1 {
		t.Fatal("applied is said by the hub after its restart, not before")
	}
	if strings.Contains(r.logs.String(), "+228") {
		t.Fatalf("a phone in the logs:\n%s", r.logs.String())
	}
	if !r.broker.retain[0] {
		t.Fatal("config.applied is retained: a program that starts later sees it")
	}
}

func TestAQuietHubRestartsAtOnce(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.alarm(contract.StateArmed)
	r.config(phoneConfig(1))
	if r.restarted() != 1 {
		t.Fatal("armed is quiet")
	}
}

func TestNewZonesRestartTheHubAtAQuietMoment(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.alarm(contract.StateDisarmed)
	r.install("")
	r.changeDevices()
	r.alarm(contract.StateArming) // the installation mode closes because the alarm is arming
	r.install(contract.InstallArmed)
	if r.restarted() != 0 {
		t.Fatal("never during a delay")
	}
	r.alarm(contract.StateArmed)
	if r.restarted() != 1 {
		t.Fatal("restart once quiet")
	}
	r.alarm(contract.StateDisarmed)
	if r.restarted() != 1 {
		t.Fatal("once only")
	}
	if _, n := r.broker.last(); n != 0 {
		t.Fatal("new zones are no configuration from the cloud: nothing to answer")
	}
}

// The installation mode exists only while the alarm is disarmed, but it is no quiet
// moment: a restart would close it, cut the console's network and page, and bring the
// tamper alerts back while the installer has the devices open.
func TestNoRestartWhileTheInstallationModeIsOpen(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.alarm(contract.StateDisarmed)
	r.install("")
	r.config(phoneConfig(1))
	if len(r.saved) != 1 || r.restarted() != 0 {
		t.Fatalf("saved %d, restarts %d: kept, but no restart during the installation", len(r.saved), r.restarted())
	}
	r.alarm(contract.StateDisarmed)
	if r.restarted() != 0 {
		t.Fatal("still installing")
	}
	r.install(contract.InstallStopped)
	if r.restarted() != 1 {
		t.Fatal("restart once the mode closed")
	}
}

// A change that lands just after the mode closed (a removal from the console that
// ends late) is applied at a later quiet moment.
func TestZonesChangedAfterTheModeClosedAreAppliedLater(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.alarm(contract.StateDisarmed)
	r.install("")
	r.install(contract.InstallTimeout)
	if r.restarted() != 0 {
		t.Fatal("nothing changed")
	}
	r.changeDevices()
	r.alarm(contract.StateArming)
	r.alarm(contract.StateArmed)
	if r.restarted() != 1 {
		t.Fatal("the late change is applied at the next quiet moment")
	}
}

func TestAnInvalidInstallationModeIsIgnored(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.alarm(contract.StateDisarmed)
	r.send(contract.InstallModeTopic(site), contract.TypeInstallMode, contract.InstallModeData{Status: "ajar"})
	r.config(phoneConfig(1))
	if r.restarted() != 1 {
		t.Fatal("an invalid mode must not hold the restart back")
	}
}

func TestAnUnknownAlarmStateWaits(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	r.config(phoneConfig(1))
	if r.restarted() != 0 {
		t.Fatal("the alarm's state is not known yet")
	}
}

func TestRefusals(t *testing.T) {
	r := start(t, DefaultConfig(site, 5))
	r.waitAnswer(1)
	r.alarm(contract.StateDisarmed)

	r.config(phoneConfig(4))
	if d := r.waitAnswer(2); d.Status != contract.ConfigRejected || d.Version != 4 || !strings.Contains(d.Reason, "older than version 5") {
		t.Fatalf("%+v", d)
	}
	r.config(phoneConfig(5))
	if d := r.waitAnswer(3); d.Status != contract.ConfigApplied || d.Version != 5 {
		t.Fatalf("the version running, asked again: %+v", d)
	}
	bad := phoneConfig(6)
	bad.EntryDelayS = 900
	r.config(bad)
	if d := r.waitAnswer(4); d.Status != contract.ConfigRejected || !strings.Contains(d.Reason, "entry_delay_s") {
		t.Fatalf("%+v", d)
	}
	r.mu.Lock()
	r.checkErr = errors.New("escalation: there must be an owner")
	r.mu.Unlock()
	r.config(phoneConfig(7))
	if d := r.waitAnswer(5); d.Status != contract.ConfigRejected || d.Reason != "escalation: there must be an owner" {
		t.Fatalf("%+v", d)
	}
	r.mu.Lock()
	r.checkErr, r.saveErr = nil, errors.New("disk full")
	r.mu.Unlock()
	r.config(phoneConfig(8))
	if d := r.waitAnswer(6); d.Status != contract.ConfigRejected || d.Reason != "the hub could not keep it" {
		t.Fatalf("%+v", d)
	}
	r.send(contract.ConfigSetTopic(site), contract.TypeConfigSet, map[string]any{"version": "seven"})
	if d := r.waitAnswer(7); d.Status != contract.ConfigRejected {
		t.Fatalf("%+v", d)
	}
	if r.restarted() != 0 || len(r.saved) != 0 {
		t.Fatal("nothing refused is kept or applied")
	}
	// Ignored: another site, another type, a duplicate, garbage.
	env, _ := contract.NewEnvelope("tg-lom-9999", "cloud", 1, r.ids.New(), r.clock.Now(), contract.TypeConfigSet, phoneConfig(9))
	b, _ := json.Marshal(env)
	r.app.HandleMessage(inbox.Message{Topic: contract.ConfigSetTopic(site), Payload: b})
	r.app.HandleMessage(inbox.Message{Topic: contract.ConfigSetTopic(site), Payload: []byte("{")})
	r.sent()
	if _, n := r.broker.last(); n != 7 {
		t.Fatalf("%d answers", n)
	}
}

func TestAConfigurationThatCouldNotBeLoadedIsRefusedAtStart(t *testing.T) {
	cfg := DefaultConfig(site, 0)
	cfg.Refused, cfg.RefusedBecause = 4, "escalation: there must be an owner"
	r := start(t, cfg)
	if d := r.waitAnswer(1); d.Version != 4 || d.Status != contract.ConfigRejected || d.Reason != cfg.RefusedBecause {
		t.Fatalf("%+v", d)
	}
}

func TestNothingToSayWithoutConfiguration(t *testing.T) {
	r := start(t, DefaultConfig(site, 0))
	// An answer to a bad configuration: it leaves once Run is under way, after
	// anything Run would have said at start.
	bad := phoneConfig(1)
	bad.EntryDelayS = 900
	r.config(bad)
	r.waitAnswer(1)
	r.sent()
	if d, n := r.broker.last(); n != 1 || d.Status != contract.ConfigRejected {
		t.Fatalf("no configuration from the cloud: no answer at start, got %d, the last %+v", n, d)
	}
	if subs := r.app.Subscriptions(); len(subs) != 3 {
		t.Fatalf("%+v", subs)
	}
}

func TestNewChecks(t *testing.T) {
	if _, err := New(Config{Site: "X"}, Deps{}); err == nil {
		t.Fatal("bad site")
	}
	if err := DefaultConfig(site, 0).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no dedupe": func(c *Config) { c.DedupeSize = 0 },
		"no outbox": func(c *Config) { c.Outbox.Size = 0 },
	} {
		c := DefaultConfig(site, 0)
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := New(DefaultConfig(site, 0), Deps{}); err == nil {
		t.Fatal("missing ports")
	}
}
