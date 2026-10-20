package consoleapp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/install"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

// --- fakes ----------------------------------------------------------------------------

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers map[chan time.Time]time.Time
}

func newClock() *fakeClock { return &fakeClock{now: t0, timers: map[chan time.Time]time.Time{}} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) Timer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers[ch] = c.now.Add(d)
	return ch, func() { c.mu.Lock(); delete(c.timers, ch); c.mu.Unlock() }
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for ch, at := range c.timers {
		if !at.After(c.now) {
			ch <- c.now
			delete(c.timers, ch)
		}
	}
}

type fakeHotspot struct {
	mu       sync.Mutex
	up, down int
	fail     error
}

func (h *fakeHotspot) Up(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail != nil {
		return h.fail
	}
	h.up++
	return nil
}

func (h *fakeHotspot) Down(context.Context) error { h.mu.Lock(); h.down++; h.mu.Unlock(); return nil }

func (h *fakeHotspot) counts() (int, int) { h.mu.Lock(); defer h.mu.Unlock(); return h.up, h.down }

// fakeSites keeps the site in memory, and refuses one without siren as the hub does.
type fakeSites struct {
	mu   sync.Mutex
	site install.Site
}

func (s *fakeSites) Read() (install.Site, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := install.Site{Zones: map[string]install.Zone{}, SirenIDs: slices.Clone(s.site.SirenIDs)}
	for k, v := range s.site.Zones {
		out.Zones[k] = v
	}
	return out, nil
}

func (s *fakeSites) Check(site install.Site) error {
	if len(site.SirenIDs) == 0 {
		return errors.New("hub: at least one siren is required")
	}
	return nil
}

func (s *fakeSites) Write(site install.Site) error {
	if err := s.Check(site); err != nil {
		return err
	}
	s.mu.Lock()
	s.site = site
	s.mu.Unlock()
	return nil
}

type published struct {
	topic string
	env   contract.Envelope
}

// rig is a console with a fake Zigbee adapter behind its publisher.
// fakeCodes keeps codes in memory; it refuses "1234" as too easy.
type fakeCodes struct {
	mu    sync.Mutex
	codes map[string]string
}

func (c *fakeCodes) People() ([]Person, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, has := c.codes["u_owner"]
	return []Person{{ID: "u_owner", Name: "Kofi", Role: "owner", HasCode: has}}, nil
}

func (c *fakeCodes) Set(user, code string) error {
	if code == "1234" {
		return ErrCodeWeak
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codes[user] = code
	return nil
}

type rig struct {
	t       *testing.T
	codes   *fakeCodes
	app     *App
	clock   *fakeClock
	hotspot *fakeHotspot
	sites   *fakeSites
	ids     *contract.IDGenerator

	mu      sync.Mutex
	sent    []published
	devices []contract.RegistryDevice
	join    []contract.RegistryDevice
	serving int
}

func (r *rig) Publish(_ context.Context, topic string, payload []byte, _ byte, _ bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		r.t.Errorf("the console sent an invalid message: %v", err)
		return nil
	}
	r.mu.Lock()
	r.sent = append(r.sent, published{topic, env})
	r.mu.Unlock()
	if topic == contract.RegistryCmdTopic(site) {
		go r.answer(env)
	}
	return nil
}

// answer plays safehome-zigbee.
func (r *rig) answer(env contract.Envelope) {
	ev := contract.RegistryEventTopic(site)
	switch env.Type {
	case contract.TypeRegistryPair:
		d, _ := contract.DecodeData[contract.RegistryPairData](env)
		if d.DurationS == 0 {
			return
		}
		r.mu.Lock()
		join := r.join
		r.join = nil
		r.devices = append(r.devices, join...)
		r.mu.Unlock()
		r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(true), false)
		for _, j := range join {
			r.deliver(ev, contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{Ref: env.ID, DeviceID: j.DeviceID, Model: j.Model, Capabilities: j.Capabilities}, false)
		}
		r.registry()
		r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(false), false)
	case contract.TypeRegistryRemove:
		d, _ := contract.DecodeData[contract.RegistryRemoveData](env)
		r.mu.Lock()
		r.devices = slices.DeleteFunc(r.devices, func(x contract.RegistryDevice) bool { return x.DeviceID == d.DeviceID })
		r.mu.Unlock()
		r.deliver(ev, contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{Ref: env.ID, DeviceID: d.DeviceID}, false)
		r.registry()
	}
}

func zigbee(pairing bool) contract.ZigbeeStatusData {
	return contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 3, Offline: []string{}, Pairing: pairing}
}

func (r *rig) deliver(topic, typ string, data any, retain bool) {
	env, err := contract.NewEnvelope(site, "test", 1, r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		r.t.Error(err)
		return
	}
	b, _ := json.Marshal(env)
	r.app.HandleMessage(Message{Topic: topic, Payload: b, Retain: retain})
}

func (r *rig) registry() {
	r.mu.Lock()
	devices := slices.Clone(r.devices)
	r.mu.Unlock()
	r.deliver(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: devices}, true)
}

func dev(id string, caps ...string) contract.RegistryDevice {
	return contract.RegistryDevice{DeviceID: id, Model: "model." + id, Capabilities: caps, Adapter: contract.AdapterZigbee}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: newClock(), hotspot: &fakeHotspot{}, ids: contract.NewIDGenerator(), codes: &fakeCodes{codes: map[string]string{}},
		sites:   &fakeSites{site: install.Site{Zones: map[string]install.Zone{"dev_door": {Name: "Entree", Kind: "entry"}}, SirenIDs: []string{"dev_siren"}}},
		devices: []contract.RegistryDevice{dev("dev_door", "contact", "battery"), dev("dev_siren", "siren", "mains"), dev("dev_motion", "motion", "battery")},
	}
	cfg := DefaultConfig(site)
	cfg.PairFor = time.Second
	cfg.Timing = install.Timing{Confirm: time.Second, Grace: 100 * time.Millisecond}
	app, err := New(cfg, Deps{
		Clock: r.clock, Publisher: r, NewID: r.ids.New, Hotspot: r.hotspot, Sites: r.sites, Codes: r.codes,
		Serve: func(ctx context.Context) error {
			r.mu.Lock()
			r.serving++
			r.mu.Unlock()
			<-ctx.Done()
			r.mu.Lock()
			r.serving--
			r.mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	r.registry()
	r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(false), true)
	r.waitFor("the registry", func() bool { return r.app.Overview().Known })
	return r
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (r *rig) mode(d contract.InstallModeData) {
	r.deliver(contract.InstallModeTopic(site), contract.TypeInstallMode, d, true)
}

func (r *rig) open() {
	r.t.Helper()
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(r.clock.Now().Add(30 * time.Minute))})
	r.waitFor("the page", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.serving == 1 })
}

func (r *rig) isServing() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.serving > 0 }

func (r *rig) sentOn(topic string) []contract.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []contract.Envelope
	for _, p := range r.sent {
		if p.topic == topic {
			out = append(out, p.env)
		}
	}
	return out
}

// --- tests ----------------------------------------------------------------------------

func TestTheConsoleLivesOnlyWhileTheInstallationModeLasts(t *testing.T) {
	r := newRig(t)
	if r.isServing() || r.app.Overview().Open {
		t.Fatal("the console runs before the installation mode")
	}
	if err := r.app.Pair(); !errors.Is(err, ErrClosed) {
		t.Fatalf("pair while closed: %v", err)
	}
	r.open()
	if up, _ := r.hotspot.counts(); up != 1 || !r.app.Overview().Open {
		t.Fatal("the network is not up")
	}
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallStopped})
	r.waitFor("the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
	if r.isServing() || r.app.Overview().Open {
		t.Fatal("the page outlives the mode")
	}
	if err := r.app.SetZone("dev_motion", "Salle", "interior"); !errors.Is(err, ErrClosed) {
		t.Fatalf("a change while closed: %v", err)
	}
}

func TestTheConsoleStopsOnItsOwnWhenTheTimeIsUp(t *testing.T) {
	r := newRig(t)
	r.open()
	r.clock.Advance(30 * time.Minute)
	r.waitFor("the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
	if r.isServing() {
		t.Fatal("the page outlives the mode, though the hub said nothing")
	}
	// A retained message from before is no new mode.
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(10 * time.Minute))})
	if err := r.app.Alive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if up, _ := r.hotspot.counts(); up != 1 {
		t.Fatal("an old open mode started the network again")
	}
}

func TestANetworkThatDoesNotStartIsSaid(t *testing.T) {
	r := newRig(t)
	r.hotspot.fail = errors.New("no Wi-Fi device")
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(30 * time.Minute))})
	r.waitFor("the failure", func() bool { return r.app.Overview().Hotspot != "" })
	if r.isServing() {
		t.Fatal("a page without network")
	}
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallTimeout})
	r.waitFor("the end", func() bool { return !r.app.Overview().Open })
	if _, down := r.hotspot.counts(); down != 0 {
		t.Fatal("stopped a network that never started")
	}
}

func TestPairingShowsTheDevicesAsTheyJoin(t *testing.T) {
	r := newRig(t)
	r.open()
	r.mu.Lock()
	r.join = []contract.RegistryDevice{dev("dev_k7p2", "contact", "battery")}
	r.mu.Unlock()
	if err := r.app.Pair(); err != nil {
		t.Fatal(err)
	}
	r.waitFor("the new device", func() bool {
		p := r.app.Overview().Pairing
		return p != nil && len(p.Added) == 1 && p.Added[0].DeviceID == "dev_k7p2"
	})
	r.waitFor("the end of the window", func() bool { p := r.app.Overview().Pairing; return !p.Active && !r.app.Overview().Busy })
	o := r.app.Overview()
	if i := slices.IndexFunc(o.Devices, func(d Device) bool { return d.ID == "dev_k7p2" }); i < 0 || o.Devices[i].Zone != nil {
		t.Fatalf("devices %+v", o.Devices)
	}
	if !slices.ContainsFunc(o.Findings, func(f install.Finding) bool { return f.DeviceID == "dev_k7p2" }) {
		t.Fatalf("a device without use is not pointed out: %+v", o.Findings)
	}
}

func TestZonesAndSirensFromThePage(t *testing.T) {
	r := newRig(t)
	r.open()
	cases := []struct {
		name string
		err  func() error
		want error
	}{
		{"unknown device", func() error { return r.app.SetZone("dev_nope", "X", "entry") }, ErrUnknown},
		{"empty name", func() error { return r.app.SetZone("dev_motion", " ", "interior") }, nil},
		{"wrong kind", func() error { return r.app.SetZone("dev_motion", "Salle", "24h_fire") }, nil},
		{"a contact as a siren", func() error { return r.app.Siren("dev_door", true) }, ErrNotASiren},
		{"the last siren", func() error { return r.app.Siren("dev_siren", false) }, nil},
		{"no zone to remove", func() error { return r.app.Unzone("dev_motion") }, nil},
	}
	for _, c := range cases {
		err := c.err()
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v", c.name, err)
		}
	}
	if err := r.app.SetZone("dev_motion", "Salle", "interior"); err != nil {
		t.Fatal(err)
	}
	if err := r.app.Unzone("dev_door"); err != nil {
		t.Fatal(err)
	}
	site, _ := r.sites.Read()
	if site.Zones["dev_motion"] != (install.Zone{Name: "Salle", Kind: "interior"}) || len(site.Zones) != 1 {
		t.Fatalf("zones %+v", site.Zones)
	}
	// The zoned device that is not paired shows, as a mistake to undo.
	r.sites.Write(install.Site{Zones: map[string]install.Zone{"dev_ghost": {Name: "G", Kind: "entry"}}, SirenIDs: []string{"dev_siren"}})
	o := r.app.Overview()
	if i := slices.IndexFunc(o.Devices, func(d Device) bool { return d.ID == "dev_ghost" }); i < 0 || o.Devices[i].Paired {
		t.Fatalf("devices %+v", o.Devices)
	}
}

func TestRemovingADevice(t *testing.T) {
	r := newRig(t)
	r.open()
	if err := r.app.Remove(context.Background(), "dev_siren"); err == nil || !strings.Contains(err.Error(), "siren") {
		t.Fatalf("the last siren: %v", err)
	}
	if n := len(r.sentOn(contract.RegistryCmdTopic(site))); n != 0 {
		t.Fatalf("%d order(s) sent for a refused removal", n)
	}
	if err := r.app.Remove(context.Background(), "dev_door"); err != nil {
		t.Fatal(err)
	}
	site, _ := r.sites.Read()
	if _, ok := site.Zones["dev_door"]; ok {
		t.Fatal("the removed device keeps its zone")
	}
	r.waitFor("the registry without it", func() bool {
		return !slices.ContainsFunc(r.app.Overview().Devices, func(d Device) bool { return d.ID == "dev_door" && d.Paired })
	})
}

func TestTheTestsPageShowsEventsWithZonesAndNoCode(t *testing.T) {
	r := newRig(t)
	r.deliver(contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"open"`)}, false)
	r.clock.Advance(time.Second)
	r.open()
	r.deliver(contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"closed"`)}, false)
	r.deliver(contract.DeviceEventTopic(site, "dev_keypad"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_keypad", Capability: "keypad", Value: json.RawMessage(`"disarm"`), Code: "123456"}, false)
	r.waitFor("the events", func() bool { return len(r.app.Overview().Events) == 2 })
	ev := r.app.Overview().Events
	if ev[1].DeviceID != "dev_door" || ev[1].Zone != "Entree" || ev[1].Value != "closed" || ev[0].DeviceID != "dev_keypad" {
		t.Fatalf("events %+v: the latest first, only since the mode opened", ev)
	}
	if strings.Contains(ev[0].Value, "123456") {
		t.Fatal("the keypad's code shows")
	}
}

func TestFinishAsksTheHubToClose(t *testing.T) {
	r := newRig(t)
	r.open()
	if err := r.app.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := r.sentOn(contract.InstallCmdTopic(site))
	if len(sent) != 1 || sent[0].Type != contract.TypeInstallClose || sent[0].Src != Source {
		t.Fatalf("%+v", sent)
	}
}

func TestNewChecks(t *testing.T) {
	good := Deps{Clock: newClock(), Publisher: &rig{}, NewID: contract.NewIDGenerator().New, Hotspot: &fakeHotspot{}, Sites: &fakeSites{},
		Codes: &fakeCodes{}, Serve: func(context.Context) error { return nil }}
	if _, err := New(DefaultConfig(site), good); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Serve = nil
	if _, err := New(DefaultConfig(site), bad); err == nil {
		t.Error("no page accepted")
	}
	if _, err := New(DefaultConfig("X"), good); err == nil {
		t.Error("a bad site accepted")
	}
	cfg := DefaultConfig(site)
	cfg.PairFor = 0
	if _, err := New(cfg, good); err == nil {
		t.Error("no pairing time accepted")
	}
}

func TestCodesAreSetOnlyWhileTheModeIsOpenAndForAContact(t *testing.T) {
	r := newRig(t)
	if err := r.app.SetCode("u_owner", "2580", "2580"); !errors.Is(err, ErrClosed) {
		t.Fatalf("mode closed: %v", err)
	}
	r.open()
	if p := r.app.Overview().People; len(p) != 1 || p[0].HasCode {
		t.Fatalf("%+v", p)
	}
	for _, c := range []struct {
		user, code, again string
		want              error
	}{
		{"u_owner", "2580", "2581", ErrCodeMismatch},
		{"u_stranger", "2580", "2580", ErrNotAContact},
		{"u_owner", "1234", "1234", ErrCodeWeak},
	} {
		if err := r.app.SetCode(c.user, c.code, c.again); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if len(r.codes.codes) != 0 {
		t.Fatalf("a refused code was kept: %v", r.codes.codes)
	}
	if err := r.app.SetCode("u_owner", "2580", "2580"); err != nil {
		t.Fatal(err)
	}
	if p := r.app.Overview().People; !p[0].HasCode || r.codes.codes["u_owner"] != "2580" {
		t.Fatalf("%+v", p)
	}
}
