package power

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 20, 0, 0, 0, time.UTC)

func newMonitor(t *testing.T) *Monitor {
	t.Helper()
	m, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

func want(t *testing.T, got []Action, w ...Action) {
	t.Helper()
	if len(got) == 0 && len(w) == 0 {
		return
	}
	if !reflect.DeepEqual(got, w) {
		t.Fatalf("actions %+v, want %+v", got, w)
	}
}

func TestTheFirstReadingsAreTold(t *testing.T) {
	m := newMonitor(t)
	want(t, m.Handle(Line{Now: at(0)}), Mains{})
	want(t, m.Handle(Gauge{Now: at(0), MilliVolts: 4100, Percent: 97}), Battery{Percent: 97})
	want(t, m.Handle(Line{Now: at(1)}))
	want(t, m.Handle(Gauge{Now: at(1), MilliVolts: 4100, Percent: 97}))
}

func TestACutCountsOnceItHeld(t *testing.T) {
	m := newMonitor(t)
	m.Handle(Line{Now: at(0)})
	// A flicker: lost for two seconds only.
	want(t, m.Handle(Line{Now: at(1), Lost: true}))
	want(t, m.Handle(Line{Now: at(3), Lost: true}))
	want(t, m.Handle(Line{Now: at(4)}))
	// A real cut.
	want(t, m.Handle(Line{Now: at(10), Lost: true}))
	want(t, m.Handle(Line{Now: at(12), Lost: true}))
	want(t, m.Handle(Line{Now: at(13), Lost: true}), Mains{Lost: true})
	want(t, m.Handle(Line{Now: at(14), Lost: true}))
	// And its end.
	want(t, m.Handle(Line{Now: at(60)}))
	want(t, m.Handle(Line{Now: at(63)}), Mains{})
}

func TestTheChargeIsToldBySteps(t *testing.T) {
	m := newMonitor(t)
	want(t, m.Handle(Gauge{Now: at(0), MilliVolts: 3900, Percent: 60}), Battery{Percent: 60})
	want(t, m.Handle(Gauge{Now: at(1), MilliVolts: 3900, Percent: 59}))
	want(t, m.Handle(Gauge{Now: at(2), MilliVolts: 3900, Percent: 61}))
	want(t, m.Handle(Gauge{Now: at(3), MilliVolts: 3880, Percent: 58}), Battery{Percent: 58})
	// The ends are always told, and a gauge above 100 is full.
	want(t, m.Handle(Gauge{Now: at(4), MilliVolts: 4200, Percent: 99}), Battery{Percent: 99})
	want(t, m.Handle(Gauge{Now: at(5), MilliVolts: 4200, Percent: 104}), Battery{Percent: 100})
	// A gauge that answers zeros is not read.
	want(t, m.Handle(Gauge{Now: at(6)}))
}

// cutOff puts the monitor on battery, the cut told.
func cutOff(t *testing.T, m *Monitor) {
	t.Helper()
	m.Handle(Line{Now: at(0)})
	m.Handle(Gauge{Now: at(0), MilliVolts: 3700, Percent: 20})
	m.Handle(Line{Now: at(1), Lost: true})
	want(t, m.Handle(Line{Now: at(4), Lost: true}), Mains{Lost: true})
}

func TestANearlyEmptyCellShutsTheHubDownOnce(t *testing.T) {
	m := newMonitor(t)
	cutOff(t, m)
	want(t, m.Handle(Gauge{Now: at(100), MilliVolts: 2990, Percent: 2}), Battery{Percent: 2})
	// A reading back above the line starts the wait again.
	want(t, m.Handle(Gauge{Now: at(120), MilliVolts: 3010, Percent: 2}))
	want(t, m.Handle(Gauge{Now: at(125), MilliVolts: 2980, Percent: 2}))
	want(t, m.Handle(Gauge{Now: at(150), MilliVolts: 2970, Percent: 1}))
	want(t, m.Handle(Gauge{Now: at(155), MilliVolts: 2960, Percent: 1}), Cut{MilliVolts: 2960})
	want(t, m.Handle(Gauge{Now: at(160), MilliVolts: 2950, Percent: 1}))
}

func TestALowCellOnTheMainsIsNotShutDown(t *testing.T) {
	m := newMonitor(t)
	m.Handle(Line{Now: at(0)})
	m.Handle(Gauge{Now: at(0), MilliVolts: 2900, Percent: 0})
	want(t, m.Handle(Gauge{Now: at(300), MilliVolts: 2900, Percent: 0}))
}

func TestTheMainsBackGiveTheCutAgain(t *testing.T) {
	m := newMonitor(t)
	cutOff(t, m)
	m.Handle(Gauge{Now: at(10), MilliVolts: 2900, Percent: 0})
	want(t, m.Handle(Gauge{Now: at(40), MilliVolts: 2900, Percent: 0}), Cut{MilliVolts: 2900})
	// The module did not cut; the mains came back, then went again.
	m.Handle(Line{Now: at(50)})
	want(t, m.Handle(Line{Now: at(53)}), Mains{})
	m.Handle(Line{Now: at(60), Lost: true})
	want(t, m.Handle(Line{Now: at(63), Lost: true}), Mains{Lost: true})
	m.Handle(Gauge{Now: at(70), MilliVolts: 2900, Percent: 0})
	want(t, m.Handle(Gauge{Now: at(100), MilliVolts: 2900, Percent: 0}), Cut{MilliVolts: 2900})
}

func TestTheSettingsAreChecked(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"settle":  func(c *Config) { c.MainsSettle = time.Hour },
		"step":    func(c *Config) { c.BatteryStep = 0 },
		"voltage": func(c *Config) { c.CutMilliVolts = 2000 },
		"after":   func(c *Config) { c.CutAfter = 0 },
	} {
		c := DefaultConfig()
		change(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
