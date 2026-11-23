package wire

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"safehome/internal/hub/core/engine"
	"safehome/internal/shared/contract"
	"safehome/internal/shared/contract/schematest"
)

// A duress leaves as an alarm.triggered of its own incident, capability "duress", with
// the place the code was typed as device and zone, never empty (the schema wants a
// device; the centre shows the zone).
func TestADuressLeavesAsAnAlarmTriggered(t *testing.T) {
	for origin, want := range map[engine.Origin]string{engine.OriginKeypad: "keypad", engine.OriginApp: "app", "": "hub"} {
		out, err := Translate([]engine.Action{engine.Emit{Event: engine.DuressRaised{Incident: "inc-9", Origin: origin}}})
		if err != nil || len(out.Events) != 1 || out.Events[0].Type != contract.TypeAlarmTriggered {
			t.Fatalf("%q: %+v %v", origin, out, err)
		}
		d := out.Events[0].Data.(contract.TriggeredData)
		if d != (contract.TriggeredData{Incident: "inc-9", DeviceID: want, Capability: "duress", Zone: want}) {
			t.Errorf("%q: %+v", origin, d)
		}
		schematest.Validate(t, "alarm-triggered.json", d)
	}
}

// What leaves of a duress disarm, but its own alarm.triggered, is byte for byte what
// the person's own code gives: the reply and the alarm.disarmed tell no threat.
func TestADuressDisarmPublishesWhatThePersonsCodeDoes(t *testing.T) {
	cfg := engine.DefaultConfig()
	cfg.Zones = map[string]engine.Zone{"dev_back": {Name: "back", Kind: engine.ZoneInstant}}
	publish := func(duress bool) (site []string, triggered []contract.TriggeredData) {
		n := 0
		e, err := engine.New(cfg, func() string { n++; return "inc-" + string(rune('0'+n)) }, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Handle(engine.Command{Now: t0, ID: "m-1", Name: engine.CmdArm, Origin: engine.OriginApp})
		now := t0.Add(cfg.ExitDelay)
		e.Handle(engine.Tick{Now: now})
		e.Handle(engine.Signal{Now: now, DeviceID: "dev_back", Sensor: engine.SensorContact, Active: true})
		auth := engine.Auth{Checked: true, OK: true, User: "u_owner", Duress: duress}
		out, err := Translate(e.Handle(engine.Command{Now: now.Add(time.Minute), ID: "m-2", Name: engine.CmdDisarm, Origin: engine.OriginKeypad, Auth: auth}))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range out.Events {
			if d, ok := ev.Data.(contract.TriggeredData); ok && d.Capability == "duress" {
				triggered = append(triggered, d)
				continue
			}
			b, _ := json.Marshal(ev)
			site = append(site, string(b))
		}
		for _, a := range out.Acks {
			b, _ := json.Marshal(a)
			site = append(site, string(b))
		}
		b, _ := json.Marshal(out.Siren)
		return append(site, string(b)), triggered
	}
	plain, none := publish(false)
	duress, raised := publish(true)
	if !reflect.DeepEqual(duress, plain) {
		t.Fatalf("a duress disarm shows:\n%v\n%v", duress, plain)
	}
	if len(none) != 0 || len(raised) != 1 || raised[0].Incident == "inc-1" || raised[0].Incident == "" {
		t.Fatalf("the duress alarm: %+v, a person's code: %+v", raised, none)
	}
}
