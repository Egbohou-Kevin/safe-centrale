package health

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The test shop of docs/contract.md: a back door that stopped answering, a smoke
// detector with a weak battery, an entrance door, and a siren outside any zone.
func shopZones(c *Config) {
	c.Zones = map[string]Zone{
		"dev_back":  {Name: "Back yard", Kind: "instant"},
		"dev_smoke": {Name: "Store room", Kind: "24h_fire"},
		"dev_door":  {Name: "Entrance", Kind: "entry"},
		"dev_ghost": {Name: "Window", Kind: "instant"}, // in the configuration, never paired
	}
}

func (h *harness) registry(devices map[string][]string) []Action {
	return h.m.Handle(Registry{Now: h.now, Devices: devices})
}

func (h *harness) offline(dev string, since, heard time.Time) []Action {
	return h.m.Handle(Availability{Now: h.now, DeviceID: dev, Since: since, Heard: heard})
}

var shopRegistry = map[string][]string{
	"dev_back":   {"battery", "contact", "link"},
	"dev_smoke":  {"battery", "link", "smoke", "tamper"},
	"dev_door":   {"battery", "contact", "link"},
	"dev_siren1": {"link", "siren"},
}

func TestTheListHoldsTheRegisteredDevicesWithTheirZones(t *testing.T) {
	h := newHarness(t, shopZones)
	since, heard := time.Date(2026, 10, 24, 18, 47, 0, 0, time.UTC), time.Date(2026, 10, 24, 14, 47, 0, 0, time.UTC)
	none(t, h.registry(shopRegistry))
	none(t, h.battery("dev_back", 58))
	none(t, h.battery("dev_smoke", 10))
	none(t, h.battery("dev_door", 81))
	none(t, h.offline("dev_back", since, heard))
	none(t, h.battery("dev_ghost", 5)) // heard of, but not registered: not listed

	want := []Device{
		{ID: "dev_back", Capabilities: []string{"battery", "contact", "link"}, Zone: Zone{"Back yard", "instant"},
			Since: since, Heard: heard, Battery: 60, BatteryKnown: true},
		{ID: "dev_door", Capabilities: []string{"battery", "contact", "link"}, Zone: Zone{"Entrance", "entry"},
			Online: true, Battery: 80, BatteryKnown: true},
		{ID: "dev_siren1", Capabilities: []string{"link", "siren"}, Online: true},
		{ID: "dev_smoke", Capabilities: []string{"battery", "link", "smoke", "tamper"}, Zone: Zone{"Store room", "24h_fire"},
			Online: true, Battery: 10, BatteryKnown: true, BatteryLow: true},
	}
	if got := h.m.Devices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	h.m.Devices()[0].Capabilities[0] = "x"
	if h.m.Devices()[0].Capabilities[0] != "battery" {
		t.Fatal("changing the list given must not change the monitor")
	}
}

// Only an offline device has times, and they are those of its latest word: a device
// back online has none, an offline one whose times are unknown has none either.
func TestOnlyAnOfflineDeviceHasTimes(t *testing.T) {
	h := newHarness(t)
	h.registry(map[string][]string{"dev_back": {"contact"}})
	since, heard := t0.Add(-time.Hour), t0.Add(-5*time.Hour)
	h.offline("dev_back", since, heard)
	if d := h.m.Devices()[0]; d.Online || !d.Since.Equal(since) || !d.Heard.Equal(heard) {
		t.Fatalf("%+v", d)
	}
	h.online("dev_back", true)
	if d := h.m.Devices()[0]; !d.Online || !d.Since.IsZero() || !d.Heard.IsZero() {
		t.Fatalf("back online, it has no times: %+v", d)
	}
	h.offline("dev_back", since, time.Time{}) // silent since the adapter's restart
	if d := h.m.Devices()[0]; d.Online || !d.Since.Equal(since) || !d.Heard.IsZero() {
		t.Fatalf("%+v", d)
	}
	h.offline("dev_back", time.Time{}, time.Time{})
	if d := h.m.Devices()[0]; d.Online || !d.Since.IsZero() {
		t.Fatalf("the latest word says it all: %+v", d)
	}
	if st := h.m.Status(); len(st.Offline) != 1 {
		t.Fatalf("the faults are as before: %+v", st)
	}
	h.online(hub, false) // the hub reports its own faults another way
	if _, ok := h.m.Snapshot().Absences[hub]; ok {
		t.Fatal("the hub is no device of the list")
	}
}

// A battery is listed to the nearest ten: the list does not move with every point. The
// weak battery follows the sensors' rule, with its gap.
func TestTheBatteryIsListedToTheNearestTen(t *testing.T) {
	h := newHarness(t)
	h.registry(map[string][]string{"dev_door": {"battery", "contact"}})
	for _, c := range []struct {
		reading, listed int
		low             bool
	}{{100, 100, false}, {64, 60, false}, {65, 70, false}, {16, 20, false}, {15, 20, true}, {4, 0, true}, {28, 30, true}, {31, 30, false}} {
		h.battery("dev_door", c.reading)
		if d := h.m.Devices()[0]; !d.BatteryKnown || d.Battery != c.listed || d.BatteryLow != c.low {
			t.Errorf("reading %d: %+v", c.reading, d)
		}
	}
	h.battery("dev_door", 120) // not a reading
	if d := h.m.Devices()[0]; d.Battery != 30 {
		t.Fatalf("%+v", d)
	}
	h.hubBattery(37)
	if _, ok := h.m.Snapshot().Batteries[hub]; ok {
		t.Fatal("the hub's battery is not a sensor's")
	}
}

// The list follows the registry: a new list replaces the old one. A device removed
// from the site is forgotten with all the monitor knew of it.
func TestTheListFollowsTheRegistry(t *testing.T) {
	h := newHarness(t)
	h.registry(shopRegistry)
	h.battery("dev_door", 80)
	h.offline("dev_door", t0, t0)
	h.registry(map[string][]string{"dev_door": {"battery", "contact"}, "dev_new": {"motion"}})
	if got := h.m.Devices(); len(got) != 2 || got[0].ID != "dev_door" || got[0].Online || got[1].ID != "dev_new" {
		t.Fatalf("%+v", got)
	}
	h.m.Handle(Forget{Now: h.now, DeviceID: "dev_door"})
	s := h.m.Snapshot()
	if len(s.Registry) != 1 || len(s.Batteries) != 0 || len(s.Absences) != 0 || len(s.Offline) != 0 {
		t.Fatalf("%+v", s)
	}
}

// The list is part of the saved state: after a restart, it is whole before the first
// retained message comes back. A state saved before the list existed loads too.
func TestTheListSurvivesARestart(t *testing.T) {
	h := newHarness(t, shopZones)
	h.registry(shopRegistry)
	h.battery("dev_smoke", 10)
	h.offline("dev_back", t0, t0.Add(-4*time.Hour))
	want := h.m.Devices()

	raw, _ := json.Marshal(h.m.Snapshot())
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	m, _ := New(h.cfg, func() string { return "x" }, h.now)
	m.Restore(s)
	if got := m.Devices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}

	var old Snapshot
	if err := json.Unmarshal([]byte(`{"offline":{"dev_back":true},"low_battery":{},"next_summary":"2026-10-18T09:00:00Z"}`), &old); err != nil {
		t.Fatal(err)
	}
	m.Restore(old)
	m.Handle(Battery{Now: h.now, DeviceID: "dev_door", Percent: 50})
	m.Handle(Registry{Now: h.now, Devices: map[string][]string{"dev_back": {"contact"}}})
	if got := m.Devices(); len(got) != 1 || got[0].Online || !got[0].Since.IsZero() {
		t.Fatalf("offline, its times unknown until the adapter says them: %+v", got)
	}
}
