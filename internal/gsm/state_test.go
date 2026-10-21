package gsm

import (
	"encoding/json"
	"testing"
	"time"
)

// saved returns the state as the driver writes it.
func saved(t *testing.T, g *Gateway) string {
	t.Helper()
	raw, err := json.Marshal(g.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// reload starts a new gateway, with the same configuration, from the saved state.
func reload(t *testing.T, g *Gateway) *Gateway {
	t.Helper()
	var s Snapshot
	if err := json.Unmarshal([]byte(saved(t, g)), &s); err != nil {
		t.Fatal(err)
	}
	b, err := New(g.cfg)
	if err != nil {
		t.Fatal(err)
	}
	b.Restore(s)
	return b
}

func TestALineCheckThatFindsNothingNewDoesNotChangeTheSavedState(t *testing.T) {
	g := newGW(t, 2)
	up(t, g, t0)
	before := saved(t, g)
	for i := 1; i <= 4; i++ {
		now := t0.Add(time.Duration(i) * 30 * time.Second)
		checks := of[CheckLine](g.Handle(Tick{Now: now}))
		if len(checks) != 2 {
			t.Fatalf("check %d: %+v", i, checks)
		}
		if got := saved(t, g); got != before {
			t.Fatalf("starting a check rewrites the state:\n%s\n%s", before, got)
		}
		for _, c := range checks {
			// The signal moves a little inside its band.
			g.Handle(Probe{Now: now, Line: c.Line, Responding: true, Registered: true, Signal: 70 + i, Operator: "togocom"})
		}
		if got := saved(t, g); got != before {
			t.Fatalf("a check that found nothing new rewrites the state:\n%s\n%s", before, got)
		}
	}
}

func TestAfterARestartTheJobInFlightGoesAgainAndABusyLineIsFreed(t *testing.T) {
	g := newGW(t, 2, func(c *Config) { c.MissesToRestart = 1 })
	up(t, g, t0)
	ex := of[Execute](g.Handle(req("r1", "owner", ChannelSMS, PriorityIntrusion, t0)))[0]
	// line2 stops answering and is being restarted.
	now := t0.Add(30 * time.Second)
	for _, c := range of[CheckLine](g.Handle(Tick{Now: now})) {
		if out := g.Handle(Probe{Now: now, Line: c.Line}); len(of[RestartModem](out)) != 1 {
			t.Fatalf("%+v", out)
		}
	}

	b := reload(t, g)
	now = now.Add(time.Second)
	if w := b.NextWake(); w.After(now) {
		t.Fatalf("next wake %v: every line is checked at once", w)
	}
	out := b.Handle(Tick{Now: now})
	again := of[Execute](out) // the line that worked takes the job at once
	checks := of[CheckLine](out)
	if len(checks) != 1 || checks[0].Line != "line2" {
		t.Fatalf("the line that was restarting is checked at once: %+v", out)
	}
	b.Handle(Probe{Now: now, Line: "line2", Responding: true, Registered: true, Signal: 70})
	if len(again) != 1 || again[0].Job != ex.Job || again[0].Text != ex.Text {
		t.Fatalf("the SMS that was being sent goes again: %+v", again)
	}
}
