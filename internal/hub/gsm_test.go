package hub

import (
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
)

func gsmStatus(lines ...contract.GSMLine) contract.GSMStatusData {
	return contract.GSMStatusData{Lines: lines}
}

func TestTheGSMLinesGoIntoTheHealth(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus,
		gsmStatus(contract.GSMLine{ID: "line1", Operator: "TOGOCOM", Responding: true, Registered: true, Signal: 70, Credit: "ok"}))
	drivertest.WaitFor(t, "the health with the GSM", func() bool {
		r.settle()
		h := r.broker.On(contract.HealthTopic(site))
		d := dataOf[contract.HealthData](t, h[len(h)-1])
		return d.GSM != nil && len(d.GSM.Lines) == 1 && d.Links["gsm"] == "ok"
	})
	r.validateAll()
}

func TestTheOwnerIsToldWhenTheSMSBackupIsLost(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus,
		gsmStatus(contract.GSMLine{ID: "line1", Responding: true, Registered: true, Signal: 40, Credit: "empty"}))
	drivertest.WaitFor(t, "the link down", func() bool {
		r.settle()
		h := r.broker.On(contract.HealthTopic(site))
		return dataOf[contract.HealthData](t, h[len(h)-1]).Links["gsm"] == "down"
	})
	if len(eventsOf(r, contract.TypeAlertRequest)) != 0 {
		t.Fatal("not at once: a line may come back")
	}
	r.clock.Advance(10 * time.Minute)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(eventsOf(r, contract.TypeAlertRequest)) == 1 })
	a := dataOf[contract.AlertRequestData](t, eventsOf(r, contract.TypeAlertRequest)[0])
	// The SMS cannot carry it: the owner is told through the internet.
	if a.Kind != contract.AlertGSMDegraded || a.Contact != "u_owner" || a.Channel != contract.ChannelApp {
		t.Fatalf("%+v", a)
	}
	r.validateAll()
}

func TestAnInvalidGSMStatusIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	before := len(r.broker.On(contract.HealthTopic(site)))
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, map[string]any{"lines": "nope"})
	r.send(contract.GSMStatusTopic(site), contract.TypeHealth, gsmStatus()) // wrong type for the topic
	r.settle()
	if len(r.broker.On(contract.HealthTopic(site))) != before {
		t.Fatal("nothing must change")
	}
}

func TestASilentGatewayIsSeenAndReported(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	good := contract.GSMLine{ID: "line1", Responding: true, Registered: true, Signal: 70, Credit: "ok"}
	lastLink := func() string {
		h := r.broker.On(contract.HealthTopic(site))
		return dataOf[contract.HealthData](t, h[len(h)-1]).Links["gsm"]
	}
	r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(good))
	drivertest.WaitFor(t, "the link up", func() bool { r.settle(); return lastLink() == "ok" })
	// The gateway repeats its status: it stays up.
	for range 5 {
		r.clock.Advance(time.Minute)
		r.send(contract.GSMStatusTopic(site), contract.TypeGSMStatus, gsmStatus(good))
	}
	r.settle()
	if lastLink() != "ok" {
		t.Fatalf("link %q while the gateway speaks", lastLink())
	}
	// Then it stops.
	r.clock.Advance(3 * time.Minute)
	drivertest.WaitFor(t, "the link silent", func() bool { r.settle(); return lastLink() == "silent" })
	if len(eventsOf(r, contract.TypeAlertRequest)) != 0 {
		t.Fatal("not at once")
	}
	r.clock.Advance(10 * time.Minute)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(eventsOf(r, contract.TypeAlertRequest)) == 1 })
	a := dataOf[contract.AlertRequestData](t, eventsOf(r, contract.TypeAlertRequest)[0])
	if a.Kind != contract.AlertGSMDegraded || a.Channel != contract.ChannelApp {
		t.Fatalf("%+v", a)
	}
	r.validateAll()
}

func TestTheZigbeeLinkFollowsTheAdapter(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	lastLink := func() string {
		h := r.broker.On(contract.HealthTopic(site))
		return dataOf[contract.HealthData](t, h[len(h)-1]).Links["zigbee"]
	}
	r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
		contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 4, Offline: []string{}})
	drivertest.WaitFor(t, "the link up", func() bool { r.settle(); return lastLink() == "ok" })
	r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus,
		contract.ZigbeeStatusData{Bridge: contract.BridgeOffline, Devices: 4, Offline: []string{}})
	drivertest.WaitFor(t, "the link down", func() bool { r.settle(); return lastLink() == "down" })
	r.clock.Advance(3 * time.Minute)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(eventsOf(r, contract.TypeAlertRequest)) > 0 })
	a := dataOf[contract.AlertRequestData](t, eventsOf(r, contract.TypeAlertRequest)[0])
	if a.Kind != contract.AlertZigbeeDown || a.Urgency != contract.UrgencyHigh || a.Contact != "u_owner" || a.Text == "" {
		t.Fatalf("%+v", a)
	}
	if lastLink() != "silent" {
		t.Fatalf("link %q: the adapter also stopped repeating its status", lastLink())
	}
	// An invalid status changes nothing.
	n := len(r.broker.On(contract.HealthTopic(site)))
	r.send(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, map[string]any{"bridge": 3})
	r.settle()
	if len(r.broker.On(contract.HealthTopic(site))) != n {
		t.Fatal("ignored")
	}
	r.validateAll()
}
