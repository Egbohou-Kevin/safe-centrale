package hub

import (
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// --- health ------------------------------------------------------------------------

func TestHubBatteryAlertsAndHealth(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devHub, contract.CapBattery, "100")
	r.device(devHub, contract.CapMains, "lost")
	r.device(devHub, contract.CapBattery, "38")
	r.settle()

	alerts := eventsOf(r, contract.TypeAlertRequest)
	if len(alerts) != 1 {
		t.Fatalf("alerts: %d", len(alerts))
	}
	a := dataOf[contract.AlertRequestData](t, alerts[0])
	if a.Kind != contract.AlertBatteryLowHub || a.Urgency != contract.UrgencyHigh || a.Percent != 38 || alerts[0].Env.Src != "escalation" {
		t.Fatalf("%+v", a)
	}
	h := r.broker.on(contract.HealthTopic(site))
	last := dataOf[contract.HealthData](t, h[len(h)-1])
	if last.Mains != contract.ValLost || last.HubBattery == nil || *last.HubBattery != 38 || !h[len(h)-1].Retain {
		t.Fatalf("%+v", last)
	}

	r.device(devHub, contract.CapMains, "present")
	r.settle()
	restored := false
	for _, al := range eventsOf(r, contract.TypeAlertRequest) {
		if dataOf[contract.AlertRequestData](t, al).Kind == contract.AlertMainsRestored {
			restored = true
		}
	}
	if !restored {
		t.Fatal("the return of the power must be announced after a battery alert")
	}
	r.validateAll()
}

func TestDailySummaryOfFaultsFromTheClock(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devBack, contract.CapAvailability, "offline")
	r.device(devMotion, contract.CapBattery, "9")
	r.settle()
	r.clock.Advance(13 * time.Hour) // next morning, 10:00 (the summary is at 09:00)
	drivertest.WaitFor(t, "the summary", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) == 2 })
	kinds := map[string][]string{}
	for _, al := range eventsOf(r, contract.TypeAlertRequest) {
		d := dataOf[contract.AlertRequestData](t, al)
		kinds[d.Kind] = d.Devices
		if d.Urgency != contract.UrgencyLow {
			t.Errorf("%+v", d)
		}
	}
	if len(kinds[contract.AlertDeviceOffline]) != 1 || kinds[contract.AlertDeviceOffline][0] != devBack ||
		len(kinds[contract.AlertBatteryLow]) != 1 || kinds[contract.AlertBatteryLow][0] != devMotion {
		t.Fatalf("%v", kinds)
	}
	r.validateAll()
}

func TestTheAgentStatusGivesTheInternetLink(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	internet := func() string {
		h := r.broker.on(contract.HealthTopic(site))
		return dataOf[contract.HealthData](t, h[len(h)-1]).Links["internet"]
	}
	r.settle()
	if got := internet(); got != "" {
		t.Fatalf("internet %q before the agent spoke, want none", got)
	}
	agentStatus(r, contract.UplinkUp)
	drivertest.WaitFor(t, "internet ok", func() bool { r.settle(); return internet() == "ok" })
	agentStatus(r, contract.UplinkDown)
	drivertest.WaitFor(t, "internet down", func() bool { r.settle(); return internet() == "down" })
	agentStatus(r, contract.UplinkUp)
	drivertest.WaitFor(t, "internet ok again", func() bool { r.settle(); return internet() == "ok" })
	// The agent stops: after three minutes of silence, the internet counts as down.
	r.clock.Advance(3 * time.Minute)
	drivertest.WaitFor(t, "internet silent", func() bool { r.settle(); return internet() == "silent" })
	r.validateAll()
}

// The services repeat their status every minute: system.health, retained and sent to
// the cloud, goes out only when what it says changes.
func TestTheHealthIsPublishedOnlyWhenItChanges(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.settle()
	published := func() int { return len(r.broker.on(contract.HealthTopic(site))) }
	line := contract.GSMLine{ID: "line1", Responding: true, Registered: true, Signal: 70, Credit: "ok"}
	repeat := func() {
		agentStatus(r, contract.UplinkUp)
		r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(line))
		r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
			contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 4, Offline: []string{}})
		r.settle()
	}
	repeat()
	n := published()
	if d := dataOf[contract.HealthData](t, r.broker.on(contract.HealthTopic(site))[n-1]); d.Links["internet"] != "ok" ||
		d.Links["gsm"] != "ok" || d.Links["zigbee"] != "ok" || d.GSM == nil {
		t.Fatalf("%+v", d)
	}
	for range 5 {
		r.clock.Advance(time.Minute)
		repeat()
	}
	if got := published(); got != n {
		t.Fatalf("%d more system.health for statuses that said nothing new", got-n)
	}
	// A line that changes is news, even when the links stay as they are.
	line.Signal = 40
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(line))
	r.settle()
	if got := published(); got != n+1 {
		t.Fatalf("%d system.health, want %d", got, n+1)
	}
	r.validateAll()
}

// A device taken away from the site leaves no fault behind: no open door that
// blocks arming, no silent device in the health.
func TestARemovedDeviceIsForgotten(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.device(devBack, contract.CapContact, "open")
	r.device(devMotion, contract.CapAvailability, "offline")
	r.settle()
	if st := r.hub.faults.Load(); len(st.Offline) != 1 {
		t.Fatalf("offline: %v", st.Offline)
	}
	removed := func(id string) {
		r.send(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{DeviceID: id})
	}
	// The other events of the registry are not for the hub, and are no mistake.
	r.send(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceAdded,
		contract.RegistryDeviceAddedData{DeviceID: "dev_new", Model: "M", Capabilities: []string{contract.CapContact}})
	removed(devBack)
	removed(devMotion)
	removed("") // invalid: ignored
	drivertest.WaitFor(t, "the faults forgotten", func() bool { r.settle(); return len(r.hub.faults.Load().Offline) == 0 })
	r.order(contract.CmdArm, contract.CommandArgs{}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeArming)
	if strings.Contains(r.logs.String(), "wrong type") {
		t.Fatalf("a device added is not a mistake:\n%s", r.logs.String())
	}
}
