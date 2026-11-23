package engine

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

func duressCode(user string) func(*Command) {
	return func(c *Command) { c.Auth = Auth{Checked: true, OK: true, User: user, Duress: true} }
}

func withID(id string) func(*Command) { return func(c *Command) { c.ID = id } }

// onSite drops what only the escalation and the centre hear of a duress: what is left
// is what the site sees (the siren, the mode, the reply, the journal).
func onSite(as []Action) []Action {
	var out []Action
	for _, a := range as {
		if al, ok := a.(Alert); ok && al.Kind == AlertDuress {
			continue
		}
		if em, ok := a.(Emit); ok {
			if _, ok := em.Event.(DuressRaised); ok {
				continue
			}
		}
		out = append(out, a)
	}
	return out
}

func alertsOf(as []Action) []Alert {
	var out []Alert
	for _, a := range as {
		if al, ok := a.(Alert); ok {
			out = append(out, al)
		}
	}
	return out
}

// The states a duress code may be typed in: armed, during the entry delay, while the
// siren rings, after it ran out, and already disarmed.
var duressStates = []struct {
	name string
	set  func(h *harness)
}{
	{"armed", func(h *harness) { h.armed() }},
	{"entry delay", func(h *harness) { h.armed(); h.open(door) }},
	{"ringing", func(h *harness) { h.armed(); h.breakWindow() }},
	{"siren ran out", func(h *harness) {
		h.armed()
		h.breakWindow()
		for i := 0; i < 6 && h.e.Mode() == ModeTriggered; i++ {
			h.advance(h.cfg.SirenCycle + h.cfg.SirenPause)
		}
		h.mustBe(ModeArmed)
	}},
	{"disarmed", func(h *harness) {}},
}

// A disarm with a duress code is, on site, the disarm of the person's own code: the
// same mode, siren, reply and events, nothing more and nothing less.
func TestADuressDisarmsExactlyAsThePersonsCode(t *testing.T) {
	for _, st := range duressStates {
		t.Run(st.name, func(t *testing.T) {
			plain, duress := newHarness(t), newHarness(t)
			st.set(plain)
			st.set(duress)
			want := plain.cmd(CmdDisarm, goodCode("u_owner"), withID("m-1"))
			got := duress.cmd(CmdDisarm, duressCode("u_owner"), withID("m-1"))
			if !reflect.DeepEqual(onSite(got), want) {
				t.Fatalf("a duress disarm shows on site:\n got %v\nwant %v", onSite(got), want)
			}
			if plain.e.Mode() != duress.e.Mode() || duress.e.Mode() != ModeDisarmed {
				t.Fatalf("mode %q, the person's code gives %q", duress.e.Mode(), plain.e.Mode())
			}
			if !reflect.DeepEqual(plain.e.Snapshot(), duress.e.Snapshot()) {
				t.Fatalf("a duress leaves a trace in the state:\n%+v\n%+v", duress.e.Snapshot(), plain.e.Snapshot())
			}
		})
	}
}

// It raises one critical duress alert, of an incident of its own that no Ended
// carries and that is not the alarm's: nothing rings, no alarm starts.
func TestADuressRaisesOneAlertOfItsOwn(t *testing.T) {
	for _, st := range duressStates {
		t.Run(st.name, func(t *testing.T) {
			h := newHarness(t)
			st.set(h)
			before := h.e.Snapshot()
			as := h.cmd(CmdDisarm, duressCode("u_owner"))
			alerts := alertsOf(as)
			if len(alerts) != 1 {
				t.Fatalf("want one alert, got %v", as)
			}
			al := alerts[0]
			if al.Kind != AlertDuress || al.Urgency != UrgencyCritical || al.User != "u_owner" || al.Cause != nil || len(al.Devices) != 0 {
				t.Fatalf("the duress alert: %+v", al)
			}
			if al.Incident == "" || slices.Contains(endedOf(as), al.Incident) || slices.Contains(before.TimedOut, al.Incident) ||
				(before.Incident != nil && before.Incident.ID == al.Incident) {
				t.Fatalf("the duress incident %q is not new or is ended: %v", al.Incident, as)
			}
			if s := h.e.Snapshot(); s.Incident != nil || slices.Contains(s.TimedOut, al.Incident) {
				t.Fatalf("the duress incident is kept by the engine: %+v", s)
			}
			if count[SirenOn](as) != 0 || len(eventsOf[Triggered](as)) != 0 || h.e.Mode() == ModeTriggered {
				t.Fatalf("a duress rings: %v", as)
			}
			if ev := onlyEvent[DuressRaised](t, as); ev != (DuressRaised{Incident: al.Incident, Origin: OriginKeypad}) {
				t.Fatalf("the duress event: %+v", ev)
			}
		})
	}
}

// A person's own code raises nothing more than before; a duress code that silences
// the siren or ends the alarm acts as the person's code, without an alert.
func TestOnlyADisarmWithADuressCodeRaisesIt(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	if as := h.cmd(CmdDisarm, goodCode("u_owner")); len(alertsOf(as)) != 0 || len(eventsOf[DuressRaised](as)) != 0 {
		t.Fatalf("a person's code raises: %v", as)
	}

	for _, name := range []CommandName{CmdSilence, CmdEnd} {
		plain, duress := newHarness(t), newHarness(t)
		for _, x := range []*harness{plain, duress} {
			x.armed()
			x.breakWindow()
		}
		want := plain.cmd(name, goodCode("u_owner"), withID("m-1"))
		got := duress.cmd(name, duressCode("u_owner"), withID("m-1"))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s with a duress code:\n got %v\nwant %v", name, got, want)
		}
	}
}

// A restart right after the duress raises it no second time: the alert lives in the
// escalation, not in the engine's state.
func TestARestartAfterADuressDoesNotRaiseItAgain(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	if n := len(alertsOf(h.cmd(CmdDisarm, duressCode("u_owner")))); n != 1 {
		t.Fatalf("%d duress alerts", n)
	}
	h.now = h.now.Add(time.Second)
	h.e = restart(t, h)
	as := h.e.Boot(h.now, Booted{}, nil)
	as = append(as, h.advance(time.Hour)...)
	if len(alertsOf(as)) != 0 || len(eventsOf[DuressRaised](as)) != 0 {
		t.Fatalf("the restart raises again: %v", as)
	}
	h.mustBe(ModeDisarmed)
}
