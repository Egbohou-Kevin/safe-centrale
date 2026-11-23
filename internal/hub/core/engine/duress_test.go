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
			if !reflect.DeepEqual(plain.e.State(duress.now), duress.e.State(duress.now)) {
				t.Fatalf("a duress shows in the published state:\n%+v\n%+v", duress.e.State(duress.now), plain.e.State(plain.now))
			}
			// The state saved keeps the alert until the escalation holds it, and nothing else.
			kept := duress.e.Snapshot()
			if len(kept.Duress) != 1 || kept.Duress[0] != (DuressGuard{Incident: alertsOf(got)[0].Incident, Origin: OriginKeypad, User: "u_owner"}) {
				t.Fatalf("the duress kept: %+v", kept.Duress)
			}
			if plain.e.Snapshot().Duress != nil {
				t.Fatalf("a person's code keeps a duress: %+v", plain.e.Snapshot().Duress)
			}
			if as := duress.e.Handle(DuressHeld{Now: duress.now, Incident: kept.Duress[0].Incident}); len(as) != 0 {
				t.Fatalf("the escalation holding it acts: %v", as)
			}
			if !reflect.DeepEqual(plain.e.Snapshot(), duress.e.Snapshot()) {
				t.Fatalf("a duress the escalation holds leaves a trace in the state:\n%+v\n%+v", duress.e.Snapshot(), plain.e.Snapshot())
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
// the siren or ends the alarm acts on site as the person's code and raises one duress
// alert too, since both clear the alarm at the centre.
func TestOnlyADuressCodeRaisesItAndSilenceOrEndToo(t *testing.T) {
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
		if !reflect.DeepEqual(onSite(got), want) {
			t.Fatalf("%s with a duress code shows on site:\n got %v\nwant %v", name, onSite(got), want)
		}
		var raised []Alert
		for _, al := range alertsOf(got) {
			if al.Kind == AlertDuress {
				raised = append(raised, al)
			}
		}
		if len(raised) != 1 || raised[0].User != "u_owner" || raised[0].Urgency != UrgencyCritical ||
			slices.Contains(endedOf(got), raised[0].Incident) {
			t.Fatalf("%s with a duress code: want one open duress alert, got %v", name, got)
		}
		if ev := onlyEvent[DuressRaised](t, got); ev.Incident != raised[0].Incident {
			t.Fatalf("%s: the duress event %+v", name, ev)
		}
	}
}

// A restart before the escalation holds the duress raises it once more, with the same
// incident, person and origin: the effects leave before the state is saved, so a power
// cut right after the code may have lost them. It stays kept, and raised at each
// restart, until the escalation holds it (DuressHeld); then a restart raises nothing.
func TestARestartBeforeTheEscalationHoldsADuressRaisesItAgain(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	live := h.cmd(CmdDisarm, duressCode("u_owner"))
	h.now = h.now.Add(time.Second)
	h.e = restart(t, h)
	as := h.e.Boot(h.now, Booted{}, nil)
	as = append(as, h.advance(time.Hour)...)
	if !reflect.DeepEqual(alertsOf(as), alertsOf(live)) {
		t.Fatalf("the restart raises %v, the code raised %v", alertsOf(as), alertsOf(live))
	}
	if !reflect.DeepEqual(eventsOf[DuressRaised](as), eventsOf[DuressRaised](live)) {
		t.Fatalf("the restart tells %v, the code told %v", eventsOf[DuressRaised](as), eventsOf[DuressRaised](live))
	}
	if count[SirenOn](as) != 0 || len(eventsOf[Triggered](as)) != 0 {
		t.Fatalf("the duress raised again rings: %v", as)
	}
	h.mustBe(ModeDisarmed)

	h.e.Handle(DuressHeld{Now: h.now, Incident: alertsOf(live)[0].Incident})
	h.e = restart(t, h)
	as = h.e.Boot(h.now, Booted{}, nil)
	as = append(as, h.advance(time.Hour)...)
	if len(alertsOf(as)) != 0 || len(eventsOf[DuressRaised](as)) != 0 {
		t.Fatalf("a duress the escalation holds is raised again: %v", as)
	}
}

// Only a duress code keeps an alert: a person's code that disarms, silences or ends
// keeps none, and another incident held changes nothing.
func TestAPersonsCodeKeepsNoDuress(t *testing.T) {
	for _, name := range []CommandName{CmdDisarm, CmdSilence, CmdEnd} {
		h := newHarness(t)
		h.armed()
		h.breakWindow()
		h.cmd(name, goodCode("u_owner"))
		if s := h.e.Snapshot(); s.Duress != nil {
			t.Fatalf("%s with a person's code keeps %+v", name, s.Duress)
		}
		h.e = restart(t, h)
		if as := h.e.Boot(h.now, Booted{}, nil); len(eventsOf[DuressRaised](as)) != 0 {
			t.Fatalf("%s with a person's code: the restart raises %v", name, as)
		}
	}
	h := newHarness(t)
	h.cmd(CmdDisarm, duressCode("u_owner"))
	h.e.Handle(DuressHeld{Now: h.now, Incident: "another"})
	if n := len(h.e.Snapshot().Duress); n != 1 {
		t.Fatalf("another incident held leaves %d duress kept", n)
	}
}
