package engine

import (
	"slices"
	"testing"
	"time"
)

// endedOf returns the incidents a person ended among the actions.
func endedOf(as []Action) []string {
	var out []string
	for _, a := range as {
		if e, ok := a.(Ended); ok {
			out = append(out, e.Incidents...)
		}
	}
	return out
}

// trialTimings are the settings of the trial on real hardware (24 October 2026).
func trialTimings(c *Config) {
	c.ExitDelay, c.EntryDelay = 30*time.Second, 15*time.Second
	c.SirenCycle, c.SirenPause, c.SirenCycles = 20*time.Second, 10*time.Second, 2
}

// The trial's sequence: armed from the app, the door opens, nobody disarms in time,
// the siren runs out and the system goes back to armed; a minute later the owner
// disarms from the app. The intrusion's escalation must end with that disarm.
func TestADisarmAfterTheSirenRanOutEndsTheIntrusion(t *testing.T) {
	h := newHarness(t, trialTimings)
	h.arm(func(c *Command) { c.Origin = OriginApp })
	h.advance(30 * time.Second)
	h.mustBe(ModeArmed)

	h.advance(28 * time.Second)
	h.open(door)
	h.mustBe(ModeEntryDelay)
	inc := onlyEvent[Triggered](t, h.advance(15*time.Second)).Incident

	// The siren's cycles: 20 s, a 10 s pause, 20 s, then back to armed.
	var cleared []Cleared
	for range 4 {
		as := h.advance(25 * time.Second)
		cleared = append(cleared, eventsOf[Cleared](as)...)
		if got := endedOf(as); len(got) != 0 {
			t.Fatalf("the siren running out ends nothing for the contacts: %v", got)
		}
	}
	h.mustBe(ModeArmed)
	if len(cleared) != 1 || cleared[0].By != ClearedByTimeout {
		t.Fatalf("cleared: %+v", cleared)
	}

	as := h.cmd(CmdDisarm, goodCode("u_kevin"), func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeDisarmed)
	if got := endedOf(as); !slices.Equal(got, []string{inc}) {
		t.Fatalf("the disarm must end the intrusion %s: %v", inc, got)
	}
	// The disarm says it too, for those who carry the alarm's messages.
	if got := onlyEvent[Disarmed](t, as).Incidents; !slices.Equal(got, []string{inc}) {
		t.Fatalf("alarm.disarmed must name the intrusion %s: %v", inc, got)
	}
	if len(eventsOf[Cleared](as)) != 0 {
		t.Fatalf("the alarm already said it was cleared: %v", as)
	}

	// Once ended, it is not ended again by the next disarm.
	h.close(door)
	h.armed()
	if got := endedOf(h.cmd(CmdDisarm, goodCode("u_kevin"))); len(got) != 0 {
		t.Fatalf("a new arming ends nothing of the old one: %v", got)
	}
}

func TestADisarmEndsEveryAlarmOfItsArmingThatRanOut(t *testing.T) {
	h := newHarness(t, trialTimings)
	h.armed()
	first := onlyEvent[Triggered](t, h.breakWindow()).Incident
	h.advance(time.Minute)
	h.mustBe(ModeArmed)
	second := onlyEvent[Triggered](t, h.signal(tamper, SensorTamper, true)).Incident
	h.advance(10 * time.Second)

	// Restarted in the middle, through the saved file: the memory of the first alarm
	// is kept.
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)

	as := h.cmd(CmdDisarm, goodCode("u_owner"))
	h.mustBe(ModeDisarmed)
	if got := endedOf(as); !sameItems(got, []string{first, second}) {
		t.Fatalf("both alarms of the arming end: %v, want %s and %s", got, first, second)
	}
	if got := onlyEvent[Disarmed](t, as).Incidents; !sameItems(got, []string{first, second}) {
		t.Fatalf("the disarm names both alarms: %v, want %s and %s", got, first, second)
	}
}

func TestADisarmLeavesAFireThatRanOutToItsContacts(t *testing.T) {
	h := newHarness(t, trialTimings)
	h.armed()
	h.signal(smoke, SensorSmoke, true)
	h.advance(time.Minute)
	h.mustBe(ModeArmed)
	as := h.cmd(CmdDisarm, goodCode("u_owner"))
	if got := endedOf(as); len(got) != 0 {
		t.Fatalf("a fire runs until a contact answers: %v", got)
	}
	if got := onlyEvent[Disarmed](t, as).Incidents; len(got) != 0 {
		t.Fatalf("the disarm ended no incident: %v", got)
	}
}

func TestStoppingTheSirenWithACodeEndsTheAlarm(t *testing.T) {
	for _, name := range []CommandName{CmdDisarm, CmdSilence} {
		t.Run(string(name), func(t *testing.T) {
			h := newHarness(t)
			h.armed()
			inc := onlyEvent[Triggered](t, h.signal(smoke, SensorSmoke, true)).Incident
			if got := endedOf(h.cmd(name, goodCode("u_owner"))); !slices.Equal(got, []string{inc}) {
				t.Fatalf("ended %v, want %s", got, inc)
			}
		})
	}
}

func sameItems(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// Kevin's decision of 25 October 2026 (the mockup AccueilAlarmeSireneArretee): the
// siren ran out, the system went back to armed, the intrusion is still open. "Terminer
// l'alarme" (alarm.end, with the code) ends it and leaves the shop armed: it is said
// cleared by the person and ended, and the disarm that follows has nothing left to end.
func TestEndingAnAlarmWhoseSirenRanOutLeavesTheShopArmed(t *testing.T) {
	h := newHarness(t, trialTimings)
	h.armed()
	h.open(door)
	inc := onlyEvent[Triggered](t, h.advance(15*time.Second)).Incident
	for range 4 {
		h.advance(25 * time.Second)
	}
	h.mustBe(ModeArmed)

	as := h.cmd(CmdEnd, goodCode("u_kevin"), func(c *Command) { c.Origin = OriginApp })
	h.mustBe(ModeArmed)
	if r := replyOf(t, as); r.Status != StatusDone {
		t.Fatalf("reply %+v", r)
	}
	if got := endedOf(as); !slices.Equal(got, []string{inc}) {
		t.Fatalf("the end must end the intrusion %s: %v", inc, got)
	}
	cl := onlyEvent[Cleared](t, as)
	if cl.Incident != inc || cl.By != ClearedByUser || cl.User != "u_kevin" || !cl.Ended || cl.Duration != 0 {
		t.Fatalf("cleared: %+v", cl)
	}
	if len(eventsOf[Disarmed](as)) != 0 || count[SirenOn](as) != 0 {
		t.Fatalf("the end disarms nothing and sounds nothing: %v", as)
	}
	if got := onlyEvent[Disarmed](t, h.cmd(CmdDisarm, goodCode("u_kevin"))).Incidents; len(got) != 0 {
		t.Fatalf("a later disarm ends nothing more: %v", got)
	}
}

// An alarm that still rings ends as a silence does, the shop still armed and its
// zones watching, said ended rather than its siren cut.
func TestEndingTheAlarmThatRingsLeavesTheShopArmedAndWatching(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()

	as := h.cmd(CmdEnd, goodCode("u_guard"))
	h.mustBe(ModeArmed)
	if cl := onlyEvent[Cleared](t, as); count[SirenOff](as) != 1 || !cl.Ended || cl.By != ClearedByUser {
		t.Fatalf("end: %v", as)
	}
	noActions(t, h.advance(time.Hour))
	h.moves()
	h.mustBe(ModeTriggered)
}

// The end wants the code, as a disarm does: a wrong one ends nothing and counts. With
// nothing under way, it is done and changes nothing.
func TestEndingWantsTheCodeAndEndsNothingWithoutAnAlarm(t *testing.T) {
	h := newHarness(t)
	h.armed()
	h.breakWindow()
	as := h.cmd(CmdEnd, badCode)
	if r := replyOf(t, as); r.Status != StatusRefused || r.Reason != ReasonBadCode || len(endedOf(as)) != 0 {
		t.Fatalf("a wrong code: %v", as)
	}
	h.mustBe(ModeTriggered)

	q := newHarness(t)
	q.armed()
	as = q.cmd(CmdEnd, goodCode("u_guard"))
	if r := replyOf(t, as); r.Status != StatusDone || len(eventsOf[Cleared](as)) != 0 || len(endedOf(as)) != 0 {
		t.Fatalf("nothing under way: %v", as)
	}
	q.mustBe(ModeArmed)
}

// The trial of 26 October 2026: armed at 10:30, an alarm whose siren program ran out
// by itself, the shop still armed. alarm.state said "armed" since the end of the
// alarm (10:33:09): it is armed since the arming, through the alarm, the entry delay
// before it and a restart.
func TestArmedSinceStaysTheArmingThroughAnAlarm(t *testing.T) {
	h := newHarness(t, trialTimings)
	h.arm(func(c *Command) { c.Origin = OriginApp })
	h.advance(30 * time.Second)
	h.mustBe(ModeArmed)
	armedAt := h.now
	h.advance(2 * time.Minute)
	h.open(door)
	h.mustBe(ModeEntryDelay)
	h.advance(15 * time.Second)
	h.mustBe(ModeTriggered)
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)
	for range 4 {
		h.advance(25 * time.Second)
	}
	h.mustBe(ModeArmed)
	if s := h.e.State(h.now); !s.Since.Equal(armedAt) {
		t.Fatalf("armed since %v, want the arming at %v", s.Since, armedAt)
	}
	h.e = restart(t, h)
	h.e.Boot(h.now, Booted{}, nil)
	h.breakWindow()
	h.advance(5 * time.Minute)
	h.mustBe(ModeArmed)
	if s := h.e.State(h.now); !s.Since.Equal(armedAt) {
		t.Fatalf("armed since %v after a second alarm and a restart, want %v", s.Since, armedAt)
	}

	// A new arming starts its own.
	h.cmd(CmdDisarm, goodCode("u_kevin"))
	h.close(door)
	h.armed()
	if s := h.e.State(h.now); !s.Since.Equal(h.now) {
		t.Fatalf("a new arming: since %v, want %v", s.Since, h.now)
	}
}
