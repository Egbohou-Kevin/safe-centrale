package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// --- the device list (system.devices) ---------------------------------------------

// registry plays the adapter's retained registry.
func (r *rig) registry(devices ...contract.RegistryDevice) {
	r.t.Helper()
	r.send(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: devices})
}

func registered(id string, capabilities ...string) contract.RegistryDevice {
	return contract.RegistryDevice{DeviceID: id, Model: "profile", Capabilities: capabilities, Adapter: contract.AdapterZigbee}
}

// theShop is the registry of the test shop: two doors, a motion sensor, a keypad and a
// siren.
func (r *rig) theShop() {
	r.t.Helper()
	r.registry(
		registered(devDoor, "battery", "contact", "link"), registered(devBack, "battery", "contact", "link"),
		registered(devMotion, "battery", "link", "motion"), registered(devKeypad, "battery", "keypad", "panic", "tamper"),
		registered(devSiren, "link", "siren"),
	)
}

func lists(r *rig) []published { return r.broker.On(contract.DevicesTopic(site)) }

// lastList returns the last system.devices published, as its JSON.
func lastList(t *testing.T, r *rig) string {
	t.Helper()
	all := lists(r)
	if len(all) == 0 {
		t.Fatal("no system.devices published")
	}
	return string(all[len(all)-1].Env.Data)
}

func TestTheDeviceListShowsEachRegisteredDevice(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	if got := lists(r); len(got) != 1 || string(got[0].Env.Data) != `{"devices":[]}` || !got[0].Retain || got[0].Env.Src != srcHealth {
		t.Fatalf("at boot, the list of a site without devices yet: %+v", got)
	}
	r.theShop()
	r.device(devBack, contract.CapBattery, "58")
	r.device(devMotion, contract.CapBattery, "9")
	heard := t0.Add(-4*time.Hour + 13*time.Second)
	r.clock.Advance(time.Minute)
	r.offline(devBack, heard)
	r.settle()

	want := `{"devices":[` +
		`{"id":"dev_back","name":"back","zone":"instant","type":"contact","online":false,"since":"2026-10-17T21:01:00.000Z","heard":"2026-10-17T17:00:00.000Z","battery":60},` +
		`{"id":"dev_door","name":"entrance","zone":"entry","type":"contact","online":true},` +
		`{"id":"dev_keypad","type":"keypad","online":true},` +
		`{"id":"dev_motion","name":"shop","zone":"interior","type":"motion","online":true,"battery":10,"battery_low":true},` +
		`{"id":"dev_siren","type":"siren","online":true}]}`
	if got := lastList(t, r); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	r.validateAll()
}

// The list goes to the cloud: it is published again only when what it says changes.
// The doors, the passers-by and the statuses repeated every minute are no news.
func TestTheDeviceListGoesOnlyOnARealChange(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.theShop()
	r.device(devDoor, contract.CapBattery, "82")
	r.settle()
	n := len(lists(r))
	for range 3 {
		r.clock.Advance(time.Minute)
		r.device(devDoor, contract.CapContact, "open")
		r.device(devDoor, contract.CapContact, "closed")
		r.device(devMotion, contract.CapMotion, "detected")
		r.device(devMotion, contract.CapMotion, "clear")
		r.device(devDoor, contract.CapLink, "40")
		r.device(devDoor, contract.CapBattery, "78") // still 80 once rounded
		r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
			contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 5, Offline: []string{}})
		agentStatus(r, contract.UplinkUp)
		r.theShop() // the same registry again
		r.settle()
	}
	if got := len(lists(r)); got != n {
		t.Fatalf("%d more system.devices for nothing new", got-n)
	}
	if got := lastList(t, r); strings.Contains(got, "open") || strings.Contains(got, "detected") || strings.Contains(got, "link") {
		t.Fatalf("a door, a passer-by or a radio link in the list: %s", got)
	}

	changes := []func(){
		func() { r.device(devDoor, contract.CapBattery, "74") }, // 70 now
		func() { r.offline(devMotion, t0) },
		func() { r.online(devMotion) },
		func() { r.registry(registered(devDoor, "battery", "contact")) },
	}
	for i, change := range changes {
		change()
		r.settle()
		if got := len(lists(r)); got != n+i+1 {
			t.Fatalf("change %d: %d system.devices, want %d", i, got, n+i+1)
		}
	}
	if got := lastList(t, r); got != `{"devices":[{"id":"dev_door","name":"entrance","zone":"entry","type":"contact","online":true,"battery":70}]}` {
		t.Fatalf("%s", got)
	}
}

// The availability is followed by its retained state, which says since when a device
// is offline and when it was last heard: the event alone says neither, and changes
// nothing. Back online, the device has no time.
func TestTheAvailabilityIsReadFromItsRetainedState(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.theShop()
	r.device(devBack, contract.CapAvailability, contract.ValOffline)
	r.settle()
	if st := r.hub.faults.Load(); len(st.Offline) != 0 || strings.Contains(lastList(t, r), `"online":false`) {
		t.Fatalf("the event alone: %v %s", st.Offline, lastList(t, r))
	}
	r.offline(devBack, t0.Add(-4*time.Hour))
	r.settle()
	if st := r.hub.faults.Load(); !slices.Equal(st.Offline, []string{devBack}) {
		t.Fatalf("%v", st.Offline)
	}
	if got := lastList(t, r); !strings.Contains(got, `{"id":"dev_back","name":"back","zone":"instant","type":"contact","online":false,"since":"2026-10-17T21:00:00.000Z","heard":"2026-10-17T17:00:00.000Z"}`) {
		t.Fatalf("%s", got)
	}
	r.online(devBack)
	r.settle()
	if got := lastList(t, r); strings.Contains(got, "since") || strings.Contains(got, "heard") {
		t.Fatalf("back online: %s", got)
	}
	r.validateAll()
}

// The retained messages that come back at start make one list, published at the end
// of the boot, not one per message.
func TestTheDeviceListWaitsForTheEndOfTheBoot(t *testing.T) {
	r := newRig(t, nil, func(c *Config) { c.BootWindow = 50 * time.Millisecond })
	r.startWithoutWaiting()
	r.theShop()
	r.device(devMotion, contract.CapBattery, "9")
	r.send(contract.DeviceStateTopic(site, devDoor, contract.CapBattery), contract.TypeDeviceState, event(devDoor, contract.CapBattery, "90"))
	r.offline(devBack, time.Time{})
	r.settle()
	if got := lists(r); len(got) != 0 {
		t.Fatalf("a list before the end of the boot: %s", got[0].Env.Data)
	}
	r.clock.PassTimer(t, 50*time.Millisecond)
	drivertest.WaitFor(t, "boot", func() bool { return len(r.broker.On(contract.AlarmStateTopic(site))) > 0 })
	r.settle()
	got := lists(r)
	if len(got) != 1 {
		t.Fatalf("%d system.devices at boot, want 1", len(got))
	}
	list := string(got[0].Env.Data)
	for _, part := range []string{`"id":"dev_back","name":"back","zone":"instant","type":"contact","online":false,"since"`, `"battery":90`, `"battery":10,"battery_low":true`} {
		if !strings.Contains(list, part) {
			t.Errorf("%s: no %s", list, part)
		}
	}
}

// A device removed from the site leaves the list at once, before the adapter's new
// registry.
func TestARemovedDeviceLeavesTheList(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.theShop()
	r.settle()
	r.send(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{DeviceID: devMotion})
	drivertest.WaitFor(t, "the device gone", func() bool { r.settle(); return !strings.Contains(lastList(t, r), devMotion) })
}

// The list says the zone's kind as the configuration writes it: the schema knows
// each kind the engine does.
func TestTheSchemaKnowsTheZoneKinds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "system-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Devices struct {
				Items struct {
					Properties struct {
						Zone struct {
							Enum []string `json:"enum"`
						} `json:"zone"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"devices"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, k := range engine.ZoneKinds() {
		kinds = append(kinds, string(k))
	}
	got := s.Properties.Devices.Items.Properties.Zone.Enum
	slices.Sort(got)
	if !slices.Equal(got, kinds) {
		t.Fatalf("schema %v, engine %v", got, kinds)
	}
}

// The people the app shows: once the hub is up, who an intrusion tells and when, by
// the plan the hub follows, retained; never a phone number.
func TestTheHubSaysWhoAnIntrusionTellsAndWhen(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	got := r.broker.On(contract.ContactsTopic(site))
	if len(got) != 1 || !got[0].Retain || got[0].Env.Src != srcEscalation || got[0].Env.Type != contract.TypeContacts {
		t.Fatalf("system.contacts: %+v", got)
	}
	var d contract.ContactsData
	if err := json.Unmarshal(got[0].Env.Data, &d); err != nil || len(d.Contacts) == 0 {
		t.Fatalf("%s: %v", got[0].Env.Data, err)
	}
	first := d.Contacts[0]
	if first.ID != "u_owner" || first.ToldAfterS == nil || *first.ToldAfterS != 0 || first.Role != "owner" {
		t.Fatalf("the owner first, at once: %+v", first)
	}
	if strings.Contains(string(got[0].Env.Data), "+228") || strings.Contains(string(got[0].Env.Data), "phone") {
		t.Fatalf("a number in %s", got[0].Env.Data)
	}
}
