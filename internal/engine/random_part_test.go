package engine

import (
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"
)

// The restored copies of a rig's engine, and where the random sequences let a restored
// engine part from the live one: on a movement only it set aside (see setAsideApart and
// parting).

// fork is a rig whose engine restarts from r's saved state, Boot given the sensors as
// they are now. It returns Boot's actions; the fork observes nothing.
func (r *rig) fork() (*rig, []Action) {
	f := r.clone()
	return f, f.boot()
}

// clone is a rig whose engine is restored from r's saved state, not booted yet. It
// observes nothing.
func (r *rig) clone() *rig {
	h := &harness{t: r.t, e: r.e, now: r.now, ids: r.ids, cfg: r.cfg}
	h.e = restart(r.t, h)
	return &rig{
		harness: h, states: maps.Clone(r.states), blurred: maps.Clone(r.blurred),
		prev: maps.Clone(r.prev), retained: maps.Clone(r.retained), apart: maps.Clone(r.apart),
		reached: r.reached, bootAt: r.bootAt, bootKnown: r.bootKnown, shown: r.shown,
		stepAtBoot: r.stepAtBoot,
	}
}

// asIf is a clone of r whose engine holds the states twin set aside: what r would do
// had its Boots set aside what twin's did.
func (r *rig) asIf(twin *rig) *rig {
	c := r.clone()
	snap := c.e.Snapshot()
	snap.SetAside = twin.e.Snapshot().SetAside
	c.e.Restore(snap)
	return c
}

// apartOn is an active momentary sensor set aside by one engine and not the other, and
// which holds it (see setAsideApart).
type apartOn struct {
	found          bool
	s              sensorRef
	live, restored bool
}

// setAsideApart finds an active momentary sensor set aside by one engine and not the
// other: a Boot of the restored one set it aside, when the start of the mode still told
// it from a movement dated since. At a restart the rig cannot check, the other may take
// it for news and ring: the clock set back while the engines ran, the start of the mode
// brought back no longer telling it (the limit written in docs/engine.md, 28 October
// 2026), or set back at the restart, the boot time unknown or the state dated since the
// system started (a state dated up to now is trusted). An alarm too many, the restored
// engine right (see parting).
func (r *rig) setAsideApart(twin *rig) apartOn {
	mine, theirs := r.e.Snapshot().SetAside, twin.e.Snapshot().SetAside
	for _, s := range randomSensors {
		sig, ok := r.states[s]
		key := sensorKey(s.dev, s.sensor)
		live, restored := mine[key].Equal(sig.Now), theirs[key].Equal(sig.Now)
		if ok && !keepsTime(s.sensor) && sig.Active && live != restored {
			return apartOn{found: true, s: s, live: live, restored: restored}
		}
	}
	return apartOn{}
}

// parting checks a restart where p sets the engines apart, the rig unable to check it:
// only the restored engine holds the sensor set aside; it does all the live one would
// do had it set aside the same states (asIf), so that no other alert is lost; and where
// the two part, the live one sends an intrusion alert for that sensor and the restored
// one does nothing about it (docs/engine.md, 28 October 2026).
func (p apartOn) parting(live, restored, asIf []Action) error {
	if p.live || !p.restored {
		return fmt.Errorf("%s/%s set aside by the live engine and not the restored one", p.s.dev, p.s.sensor)
	}
	if a, b := describe(asIf), describe(restored); a != b {
		return fmt.Errorf("the restored engine diverged from the live one holding its states set aside:\n  live     %s\n  restored %s", a, b)
	}
	if describe(live) == describe(restored) {
		return nil
	}
	if !alertsFor(live, p.s) {
		return fmt.Errorf("the engines parted on %s/%s without the live one ringing for it:\n  live     %s\n  restored %s", p.s.dev, p.s.sensor, describe(live), describe(restored))
	}
	if mentions(restored, p.s) {
		return fmt.Errorf("the restored engine acted on %s/%s it set aside: %s", p.s.dev, p.s.sensor, describe(restored))
	}
	return nil
}

// alertsFor tells whether as sends an intrusion alert caused by s: a new incident, its
// Triggered caused by s too, or one already ringing that s joins.
func alertsFor(as []Action, s sensorRef) bool {
	for _, a := range as {
		if al, ok := a.(Alert); ok && al.Kind == AlertIntrusion && causedBy(al.Cause, s) {
			fresh := slices.ContainsFunc(eventsOf[Triggered](as), func(tr Triggered) bool { return tr.Incident == al.Incident })
			return !fresh || slices.ContainsFunc(eventsOf[Triggered](as), func(tr Triggered) bool {
				return tr.Incident == al.Incident && causedBy(&tr.Cause, s)
			})
		}
	}
	return false
}

// mentions tells whether as does anything about s.
func mentions(as []Action, s sensorRef) bool {
	for _, a := range as {
		switch v := a.(type) {
		case Alert:
			if causedBy(v.Cause, s) || slices.Contains(v.Devices, s.dev) {
				return true
			}
		case Emit:
			switch ev := v.Event.(type) {
			case Triggered:
				if causedBy(&ev.Cause, s) {
					return true
				}
			case EntryDelayStarted:
				if ev.DeviceID == s.dev {
					return true
				}
			}
		}
	}
	return false
}

func causedBy(c *Cause, s sensorRef) bool {
	return c != nil && c.DeviceID == s.dev && c.Sensor == s.sensor
}

// parting allows the restored engine silent only on the movement it set aside, the live
// one alerting for it: fed one of the other partings, it fails.
func TestPartingSeesItsDefects(t *testing.T) {
	moved := sensorRef{motion, SensorMotion}
	cause := Cause{DeviceID: motion, Sensor: SensorMotion, Zone: "shop"}
	rings := []Action{Emit{Triggered{Incident: "i", Cause: cause}}, SirenOn{Duration: time.Second},
		Alert{Incident: "i", Kind: AlertIntrusion, Cause: &cause}}
	doorCause := Cause{DeviceID: back, Sensor: SensorContact, Zone: "back"}
	door := []Action{Emit{Triggered{Incident: "d", Cause: doorCause}}, Alert{Incident: "d", Kind: AlertIntrusion, Cause: &doorCause}}
	held := apartOn{found: true, s: moved, restored: true}
	if err := held.parting(rings, nil, nil); err != nil {
		t.Fatalf("the restored engine silent on the movement it set aside: %v", err)
	}
	if err := held.parting(rings, rings, rings); err != nil {
		t.Fatalf("both engines ringing: %v", err)
	}
	if (apartOn{found: true, s: moved, live: true}).parting(rings, nil, nil) == nil {
		t.Error("parting passed a movement set aside by the live engine only")
	}
	if held.parting(door, nil, door) == nil {
		t.Error("parting passed a door lost by the restored engine")
	}
	if held.parting([]Action{SirenOn{Duration: time.Second}}, nil, nil) == nil {
		t.Error("parting passed engines parting without the live one ringing for the movement")
	}
	joined := []Action{SirenOn{Duration: time.Second}, rings[2]}
	if err := held.parting(joined, joined[:1], joined[:1]); err != nil {
		t.Fatalf("the restored engine silent on the movement joining the incident ringing: %v", err)
	}
	half := rings[2:]
	if held.parting(rings, half, half) == nil {
		t.Error("parting passed a restored engine acting on the movement it set aside")
	}
}
