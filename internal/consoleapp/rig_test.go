package consoleapp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/install"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

// The fakes of the console's ports, and the rig that runs a console on them.

type fakeHotspot struct {
	mu       sync.Mutex
	up, down int
	fail     error
	slow     chan struct{} // if set, Up waits for it to close: nmcli taking its time
}

func (h *fakeHotspot) Up(ctx context.Context) error {
	if h.slow != nil {
		select {
		case <-h.slow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	mu       sync.Mutex
	site     install.Site
	slowRead time.Duration // a read returns this long after it took the site
	onCheck  func()        // if set, called as Check starts; may block
}

func (s *fakeSites) Read() (install.Site, error) {
	s.mu.Lock()
	out := install.Site{Zones: maps.Clone(s.site.Zones), SirenIDs: slices.Clone(s.site.SirenIDs)}
	s.mu.Unlock()
	if out.Zones == nil {
		out.Zones = map[string]install.Zone{}
	}
	time.Sleep(s.slowRead)
	return out, nil
}

func (s *fakeSites) Check(site install.Site) error {
	if s.onCheck != nil {
		s.onCheck()
	}
	return s.check(site)
}

func (s *fakeSites) check(site install.Site) error {
	if len(site.SirenIDs) == 0 {
		return errors.New("hub: at least one siren is required")
	}
	return nil
}

func (s *fakeSites) Write(site install.Site) error {
	if err := s.check(site); err != nil {
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

// fakeCodes keeps codes in memory; it refuses "1234" as too easy.
type fakeCodes struct {
	mu    sync.Mutex
	codes map[string]string
}

// People: u_owner is the only contact; any other user with a code is a leftover.
func (c *fakeCodes) People() ([]Person, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, has := c.codes["u_owner"]
	var leftovers []string
	for _, u := range slices.Sorted(maps.Keys(c.codes)) {
		if u != "u_owner" {
			leftovers = append(leftovers, u)
		}
	}
	return []Person{{ID: "u_owner", Name: "Kofi", Role: "owner", HasCode: has}}, leftovers, nil
}

func (c *fakeCodes) Remove(user string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.codes[user]; !ok {
		return ErrNoSuchCode
	}
	delete(c.codes, user)
	return nil
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

// rig is a console with a fake Zigbee adapter behind its publisher.
type rig struct {
	t       *testing.T
	codes   *fakeCodes
	app     *App
	clock   *drivertest.FakeClock
	hotspot *fakeHotspot
	sites   *fakeSites
	ids     *contract.IDGenerator
	stop    func() // stops the console and waits for its end

	mu       sync.Mutex
	sent     []published
	devices  []contract.RegistryDevice
	join     []contract.RegistryDevice
	serving  int
	keepOpen bool                    // the adapter keeps the network open for the whole window
	hold     func(contract.Envelope) // called before each message is taken, may block
}

func (r *rig) Publish(_ context.Context, topic string, payload []byte, _ byte, _ bool) error {
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		r.t.Errorf("the console sent an invalid message: %v", err)
		return nil
	}
	r.mu.Lock()
	hold := r.hold
	r.mu.Unlock()
	if hold != nil {
		hold(env)
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
		join, keepOpen := r.join, r.keepOpen
		r.join = nil
		r.devices = append(r.devices, join...)
		r.mu.Unlock()
		r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(true), false)
		for _, j := range join {
			r.deliver(ev, contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{Ref: env.ID, DeviceID: j.DeviceID, Model: j.Model, Capabilities: j.Capabilities}, false)
		}
		r.registry()
		if !keepOpen {
			r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(false), false)
		}
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
	r.app.HandleMessage(inbox.Message{Topic: topic, Payload: b, Retain: retain})
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
	r := &rig{t: t, clock: drivertest.NewFakeClock(t0), hotspot: &fakeHotspot{}, ids: contract.NewIDGenerator(), codes: &fakeCodes{codes: map[string]string{}},
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
	r.stop = sync.OnceFunc(func() { cancel(); <-done })
	t.Cleanup(r.stop)
	r.registry()
	r.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, zigbee(false), true)
	drivertest.WaitFor(r.t, "the registry", func() bool { return r.app.Overview().Known })
	return r
}

func (r *rig) mode(d contract.InstallModeData) {
	r.deliver(contract.InstallModeTopic(site), contract.TypeInstallMode, d, true)
}

func (r *rig) open() {
	r.t.Helper()
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(r.clock.Now().Add(30 * time.Minute))})
	drivertest.WaitFor(r.t, "the page", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.serving == 1 })
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
