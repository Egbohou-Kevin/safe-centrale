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
	h.e.Boot(h.now, nil)

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
