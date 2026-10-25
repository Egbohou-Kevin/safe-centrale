package contract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/schematest"
)

// devicesItem is the schema of one device of system.devices.
type devicesItem struct {
	Properties map[string]map[string]any `json:"properties"`
	Required   []string                  `json:"required"`
}

func loadDevicesItem(t *testing.T) devicesItem {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(schemasDir, "system-devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Devices struct {
				Items devicesItem `json:"items"`
			} `json:"devices"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Properties.Devices.Items
}

// The devices of the list are described field by field, like the messages
// themselves (TestSchemasMatchGoTypes looks at the top level only).
func TestTheDeviceListSchemaMatchesGo(t *testing.T) {
	item := loadDevicesItem(t)
	all, required := jsonFields(reflect.TypeOf(DeviceHealth{}))
	if got := keys(item.Properties); !slices.Equal(got, all) {
		t.Errorf("schema properties %v, Go fields %v", got, all)
	}
	if got := sorted(item.Required); !slices.Equal(got, required) {
		t.Errorf("schema required fields %v, Go fields without omitempty %v", got, required)
	}
	if got := item.Properties["name"]["maxLength"]; got != float64(MaxDeviceName) {
		t.Errorf("name maxLength %v, MaxDeviceName %d", got, MaxDeviceName)
	}
}

// The example of docs/contract.md: a contact offline since 18:47, last heard at
// 14:47; a smoke detector with a weak battery; a door that answers; a siren outside
// any zone.
func TestTheDeviceListSchemaAcceptsTheContractsExample(t *testing.T) {
	since, heard := TSPtr(time.Date(2026, 10, 24, 18, 47, 0, 0, time.UTC)), TSPtr(time.Date(2026, 10, 24, 14, 47, 0, 0, time.UTC))
	list := DevicesData{Devices: []DeviceHealth{
		{ID: "dev_back", Name: "Arri\u00e8re-cour", Zone: "instant", Type: CapContact, Since: since, Heard: heard, Battery: intPtr(60)},
		{ID: "dev_smoke", Name: "R\u00e9serve", Zone: "24h_fire", Type: CapSmoke, Online: true, Battery: intPtr(10), BatteryLow: true},
		{ID: "dev_door", Name: "Entr\u00e9e", Zone: "entry", Type: CapContact, Online: true, Battery: intPtr(80)},
		{ID: "dev_siren1", Type: CapSiren, Online: true},
	}}
	schematest.Validate(t, "system-devices.json", list)
	schematest.Validate(t, "system-devices.json", DevicesData{Devices: []DeviceHealth{}}) // a site without devices yet
	raw, _ := json.Marshal(list.Devices[3])
	if string(raw) != `{"id":"dev_siren1","type":"siren","online":true}` {
		t.Fatalf("an absent field is left out, never null: %s", raw)
	}
}

// What the list must never say: a time for a device that answers, a battery as
// precise as the reading, a weak battery said false, a type or a zone the app does
// not know.
func TestTheDeviceListSchemaRefusesWhatTheContractRefuses(t *testing.T) {
	const ok = `{"id":"dev_back","type":"contact","online":false,"since":"2026-10-24T18:47:00.000Z","heard":"2026-10-24T14:47:00.000Z","battery":60}`
	if err := schematest.Check("system-devices.json", []byte(`{"devices":[`+ok+`]}`)); err != nil {
		t.Fatalf("the reference device must be valid: %v", err)
	}
	online := strings.Replace(ok, `"online":false`, `"online":true`, 1)
	for name, device := range map[string]string{
		"online, with since":        strings.Replace(online, `,"heard":"2026-10-24T14:47:00.000Z"`, "", 1),
		"online, with heard":        strings.Replace(online, `"since":"2026-10-24T18:47:00.000Z",`, "", 1),
		"a battery not rounded":     strings.Replace(ok, `"battery":60`, `"battery":64`, 1),
		"a battery above 100":       strings.Replace(ok, `"battery":60`, `"battery":110`, 1),
		"a weak battery said false": strings.Replace(ok, `"battery":60`, `"battery":60,"battery_low":false`, 1),
		"an unknown type":           strings.Replace(ok, `"contact"`, `"tamper"`, 1),
		"an unknown zone":           strings.Replace(ok, `"type"`, `"zone":"perimeter","type"`, 1),
		"a name too long":           strings.Replace(ok, `"type"`, `"name":"`+strings.Repeat("x", MaxDeviceName+1)+`","type"`, 1),
		"without a type":            strings.Replace(ok, `"type":"contact",`, "", 1),
		"without online":            strings.Replace(ok, `"online":false,`, "", 1),
		"a malformed time":          strings.Replace(ok, `"2026-10-24T14:47:00.000Z"`, `"14:47"`, 1),
		"an empty id":               strings.Replace(ok, `"dev_back"`, `""`, 1),
		"a list that is not one":    `"dev_back"`,
		"a device that is not one":  `42`,
		"online said in words":      strings.Replace(ok, `"online":false`, `"online":"no"`, 1),
	} {
		raw := `{"devices":[` + device + `]}`
		if err := schematest.Check("system-devices.json", []byte(raw)); err == nil {
			t.Errorf("%s: accepted %s", name, raw)
		}
	}
	if err := schematest.Check("system-devices.json", []byte(`{}`)); err == nil {
		t.Error("a list without devices accepted")
	}
}
