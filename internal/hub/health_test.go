package hub

import (
	"testing"
	"time"

	"safehome/internal/contract"
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
	r.waitFor("the summary", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) == 2 })
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
	r.waitFor("internet ok", func() bool { r.settle(); return internet() == "ok" })
	agentStatus(r, contract.UplinkDown)
	r.waitFor("internet down", func() bool { r.settle(); return internet() == "down" })
	agentStatus(r, contract.UplinkUp)
	r.waitFor("internet ok again", func() bool { r.settle(); return internet() == "ok" })
	// The agent stops: after three minutes of silence, the internet counts as down.
	r.clock.Advance(3 * time.Minute)
	r.waitFor("internet silent", func() bool { r.settle(); return internet() == "silent" })
	r.validateAll()
}
