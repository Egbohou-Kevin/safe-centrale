package wire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/health"
	"safehome/internal/schematest"
)

// The example of docs/contract.md, from the messages the hub receives to the list it
// publishes: the registry, the zones of the site, the battery readings and the
// retained availability of the back door.
func TestTheDeviceListOfTheContractsExample(t *testing.T) {
	cfg := health.DefaultConfig()
	cfg.HubDeviceID = "dev_hub"
	back, door, store := "Arri\u00e8re-cour", "Entr\u00e9e", "R\u00e9serve"
	cfg.Zones = map[string]health.Zone{
		"dev_back": {Name: back, Kind: "instant"}, "dev_smoke": {Name: store, Kind: "24h_fire"}, "dev_door": {Name: door, Kind: "entry"},
	}
	m, err := health.New(cfg, contract.NewIDGenerator().New, t0)
	if err != nil {
		t.Fatal(err)
	}
	reg := Registry(contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{
		{DeviceID: "dev_back", Model: "aqara.contact.mccgq11lm", Capabilities: []string{"battery", "contact", "link"}, Adapter: contract.AdapterZigbee},
		{DeviceID: "dev_smoke", Model: "heiman.smoke.hs1sa", Capabilities: []string{"battery", "link", "smoke", "tamper"}, Adapter: contract.AdapterZigbee},
		{DeviceID: "dev_door", Model: "aqara.contact.mccgq11lm", Capabilities: []string{"battery", "contact", "link"}, Adapter: contract.AdapterZigbee},
		{DeviceID: "dev_siren1", Model: "heiman.siren.hs2wd", Capabilities: []string{"link", "siren"}, Adapter: contract.AdapterZigbee},
	}})
	reg.Now = t0
	m.Handle(reg)
	offline := event("dev_back", contract.CapAvailability, `"offline"`)
	offline.Since = contract.TSPtr(time.Date(2026, 10, 24, 18, 47, 0, 0, time.UTC))
	offline.Heard = contract.TSPtr(time.Date(2026, 10, 24, 14, 47, 0, 0, time.UTC))
	for _, d := range []contract.DeviceEventData{
		event("dev_back", contract.CapBattery, `61`), event("dev_smoke", contract.CapBattery, `9`),
		event("dev_door", contract.CapBattery, `78`), offline,
		event("dev_door", contract.CapContact, `"open"`), // never in the list
	} {
		if in, ok := Input(d, t0); ok {
			m.Handle(in)
		}
	}

	list := Devices(m.Devices())
	schematest.Validate(t, "system-devices.json", list)
	got, _ := json.Marshal(list)
	want := `{"devices":[` +
		`{"id":"dev_back","name":"` + back + `","zone":"instant","type":"contact","online":false,"since":"2026-10-24T18:47:00.000Z","heard":"2026-10-24T14:47:00.000Z","battery":60},` +
		`{"id":"dev_door","name":"` + door + `","zone":"entry","type":"contact","online":true,"battery":80},` +
		`{"id":"dev_siren1","type":"siren","online":true},` +
		`{"id":"dev_smoke","name":"` + store + `","zone":"24h_fire","type":"smoke","online":true,"battery":10,"battery_low":true}]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// The type is the first capability found in the order of the contract; tamper,
// battery and link never decide, and a device that is none of the types is left out.
func TestTheTypeIsTheFirstCapabilityInTheContractsOrder(t *testing.T) {
	for caps, want := range map[string]string{
		"battery,keypad,link,panic,tamper": contract.CapKeypad,
		"battery,link,smoke,tamper":        contract.CapSmoke,
		"link,siren":                       contract.CapSiren,
		"contact,vibration":                contract.CapContact,
		"battery,link,motion":              contract.CapMotion,
		"glass_break,vibration":            contract.CapGlassBreak,
		"tamper,vibration":                 contract.CapVibration,
		"battery,panic":                    contract.CapPanic,
		"battery,link,tamper":              "",
	} {
		list := Devices([]health.Device{{ID: "d", Capabilities: strings.Split(caps, ","), Online: true}})
		switch {
		case want == "" && len(list.Devices) != 0:
			t.Errorf("%s: listed as %+v", caps, list.Devices)
		case want != "" && (len(list.Devices) != 1 || list.Devices[0].Type != want):
			t.Errorf("%s: %+v, want %s", caps, list.Devices, want)
		}
	}
}

// The schema lists the same types as the order above: a type added on one side only
// would be refused by the cloud, or never sent.
func TestTheSchemaKnowsTheTypes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "system-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Devices struct {
				Items struct {
					Properties struct {
						Type struct {
							Enum []string `json:"enum"`
						} `json:"type"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"devices"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	got, want := s.Properties.Devices.Items.Properties.Type.Enum, slices.Clone(deviceTypes)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("schema %v, wire %v", got, want)
	}
}

// Since and heard go only with a device offline, to the minute; a name is cut like
// device_name, and a battery never read is absent.
func TestTheListSaysNoTimeForADeviceOnline(t *testing.T) {
	long := strings.Repeat("\u00e9", contract.MaxDeviceName+5)
	list := Devices([]health.Device{
		{ID: "a", Capabilities: []string{"contact"}, Online: true, Since: t0, Heard: t0, Zone: health.Zone{Name: long, Kind: "entry"}},
		{ID: "b", Capabilities: []string{"motion"}, BatteryLow: true},
		{ID: "c", Capabilities: []string{"contact"}, Since: t0.Add(4*time.Hour + 2*time.Second), Heard: t0.Add(2 * time.Second)},
	})
	a, b, c := list.Devices[0], list.Devices[1], list.Devices[2]
	if !c.Since.Equal(t0.Add(4*time.Hour)) || !c.Heard.Equal(t0) {
		t.Fatalf("to the minute: %+v", c)
	}
	if a.Since != nil || a.Heard != nil || a.Name != strings.Repeat("\u00e9", contract.MaxDeviceName) || a.Battery != nil {
		t.Fatalf("%+v", a)
	}
	if b.Online || b.Since != nil || b.Heard != nil || b.Battery != nil || !b.BatteryLow || b.Name != "" || b.Zone != "" {
		t.Fatalf("offline with no time known, a weak battery without a reading: %+v", b)
	}
	schematest.Validate(t, "system-devices.json", list)
	if got := Devices(nil); got.Devices == nil {
		t.Fatal("an empty list is a list, not null")
	}
}

// The retained availability state carries the times of an offline device; the input
// takes them.
func TestTheAvailabilityStateGivesItsTimes(t *testing.T) {
	since, heard := t0.Add(-time.Hour), t0.Add(-5*time.Hour)
	d := event("dev_back", contract.CapAvailability, `"offline"`)
	d.Since, d.Heard = contract.TSPtr(since), contract.TSPtr(heard)
	in, ok := Input(d, t0)
	if want := (health.Availability{Now: t0, DeviceID: "dev_back", Since: since, Heard: heard}); !ok || in != want {
		t.Fatalf("got %#v, want %#v", in, want)
	}
	reg := Registry(contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{{DeviceID: "", Capabilities: []string{"contact"}}}})
	if len(reg.Devices) != 0 {
		t.Fatalf("a device without an id is no device: %+v", reg)
	}
}
