package health

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"
)

// t0 is Saturday 2026-10-17 21:00 UTC; the daily summary is at 09:00.
var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

const hub = "dev_hub"

func testConfig() Config {
	c := DefaultConfig()
	c.HubDeviceID = hub
	return c
}

type harness struct {
	t   *testing.T
	m   *Monitor
	now time.Time
	ids int
	cfg Config
}

func newHarness(t *testing.T, mutate ...func(*Config)) *harness {
	t.Helper()
	h := &harness{t: t, now: t0, cfg: testConfig()}
	for _, f := range mutate {
		f(&h.cfg)
	}
	m, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("alert-%d", h.ids) }, t0)
	if err != nil {
		t.Fatal(err)
	}
	h.m = m
	return h
}

func (h *harness) advance(d time.Duration) []Action {
	h.now = h.now.Add(d)
	return h.m.Handle(Tick{Now: h.now})
}
func (h *harness) mains(lost bool) []Action { return h.m.Handle(Mains{Now: h.now, Lost: lost}) }
func (h *harness) hubBattery(p int) []Action {
	return h.m.Handle(Battery{Now: h.now, DeviceID: hub, Percent: p})
}
func (h *harness) battery(dev string, p int) []Action {
	return h.m.Handle(Battery{Now: h.now, DeviceID: dev, Percent: p})
}
func (h *harness) online(dev string, up bool) []Action {
	return h.m.Handle(Availability{Now: h.now, DeviceID: dev, Online: up})
}

func alerts(t *testing.T, as []Action) []Alert {
	t.Helper()
	var out []Alert
	for _, a := range as {
		al, ok := a.(Alert)
		if !ok {
			t.Fatalf("unexpected action %T", a)
		}
		out = append(out, al)
	}
	return out
}

func none(t *testing.T, as []Action) {
	t.Helper()
	if len(as) != 0 {
		t.Fatalf("want no alert, got %v", as)
	}
}

func one(t *testing.T, as []Action) Alert {
	t.Helper()
	al := alerts(t, as)
	if len(al) != 1 {
		t.Fatalf("want 1 alert, got %v", as)
	}
	return al[0]
}

// --- hub: power cuts ------------------------------------------------------

func TestPowerCutAloneIsNotAnAlert(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	none(t, h.mains(true))
	none(t, h.hubBattery(95))
	none(t, h.hubBattery(60))
	none(t, h.mains(false)) // nothing was sent, so nothing is announced
	if st := h.m.Status(); st.MainsLost || st.HubBattery != 60 {
		t.Fatalf("%+v", st)
	}
}

func TestHubBatteryAlertsAtFortyThenTen(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	h.mains(true)
	none(t, h.hubBattery(41))

	a := one(t, h.hubBattery(40)) // at the threshold
	if a.Kind != AlertHubBatteryLow || a.Urgency != UrgencyHigh || a.Percent != 40 || a.Incident == "" {
		t.Fatalf("%+v", a)
	}
	none(t, h.hubBattery(39)) // only one alert per threshold
	none(t, h.hubBattery(11))

	a = one(t, h.hubBattery(10))
	if a.Kind != AlertHubBatteryLow || a.Urgency != UrgencyCritical || a.Percent != 10 {
		t.Fatalf("%+v", a)
	}
	none(t, h.hubBattery(5))
	none(t, h.hubBattery(0))
}

func TestHubBatteryJumpingBelowBothThresholdsAlertsOnce(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(80)
	h.mains(true)
	a := one(t, h.hubBattery(8))
	if a.Urgency != UrgencyCritical || a.Percent != 8 {
		t.Fatalf("%+v", a)
	}
	none(t, h.hubBattery(7))
	none(t, h.hubBattery(9))
}

func TestHubBatteryWithMainsPresentNeverAlerts(t *testing.T) {
	h := newHarness(t)
	none(t, h.hubBattery(30)) // charging after an outage, say
	none(t, h.hubBattery(5))
}

func TestPowerCutWhenTheBatteryIsAlreadyLowAlertsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(35)
	a := one(t, h.mains(true))
	if a.Kind != AlertHubBatteryLow || a.Percent != 35 {
		t.Fatalf("%+v", a)
	}
}

func TestPowerCutBeforeAnyBatteryReading(t *testing.T) {
	h := newHarness(t)
	none(t, h.mains(true))
	a := one(t, h.hubBattery(38))
	if a.Percent != 38 {
		t.Fatalf("%+v", a)
	}
}

func TestMainsLostRepeatedDoesNotRepeatTheAlert(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(30)
	one(t, h.mains(true))
	none(t, h.mains(true))
}

func TestThresholdsRearmAboveSixtyPercent(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	h.mains(true)
	one(t, h.hubBattery(40))

	// The power comes back and the battery recharges a little: not rearmed yet.
	h.mains(false)
	h.hubBattery(55)
	h.mains(true)
	none(t, h.hubBattery(35)) // same warning as before: no second alert

	// Fully recharged, then a new outage: the alerts work again.
	h.mains(false)
	h.hubBattery(61)
	h.mains(true)
	a := one(t, h.hubBattery(40))
	if a.Urgency != UrgencyHigh {
		t.Fatalf("%+v", a)
	}
}

func TestMainsBackIsAnnouncedOnlyAfterABatteryAlert(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	h.mains(true)
	one(t, h.hubBattery(12))
	h.hubBattery(9) // urgent alert too

	a := one(t, h.mains(false))
	if a.Kind != AlertMainsRestored || a.Urgency != UrgencyLow || a.Percent != 9 || a.Incident == "" {
		t.Fatalf("%+v", a)
	}
	none(t, h.mains(false)) // said once
	h.hubBattery(30)
	none(t, h.mains(true))  // the first alert stays used until a recharge, the urgent level is not reached
	none(t, h.mains(false)) // no new battery alert since: nothing owed
}

func TestMainsPresentAtBootIsNotARestoration(t *testing.T) {
	h := newHarness(t)
	none(t, h.mains(false))
}

func TestOutOfRangeBatteryReadingsAreIgnored(t *testing.T) {
	h := newHarness(t)
	h.mains(true)
	none(t, h.hubBattery(-1))
	none(t, h.hubBattery(101))
	if h.m.Status().HubBatteryKnown {
		t.Fatal("a reading outside 0..100 is not a reading")
	}
	h.battery("dev_door", -5)
	if len(h.m.Status().LowBattery) != 0 {
		t.Fatal("a reading outside 0..100 is not a reading")
	}
}

// --- sensors ---------------------------------------------------------------

func TestSensorBatteryIsRecordedNotAlertedAtOnce(t *testing.T) {
	h := newHarness(t)
	none(t, h.battery("dev_door", 80))
	none(t, h.battery("dev_door", 15)) // at the threshold
	none(t, h.battery("dev_motion", 9))
	if got := h.m.Status().LowBattery; !slices.Equal(got, []string{"dev_door", "dev_motion"}) {
		t.Fatalf("%v", got)
	}
	h.battery("dev_door", 20) // between low and rearm: stays low
	if got := h.m.Status().LowBattery; !slices.Equal(got, []string{"dev_door", "dev_motion"}) {
		t.Fatalf("%v", got)
	}
	h.battery("dev_door", 31) // a fresh battery
	if got := h.m.Status().LowBattery; !slices.Equal(got, []string{"dev_motion"}) {
		t.Fatalf("%v", got)
	}
}

func TestHubBatteryIsNeverASensorFault(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(5)
	if len(h.m.Status().LowBattery) != 0 {
		t.Fatal("the hub battery is not a sensor battery")
	}
}

func TestOfflineAndBackOnline(t *testing.T) {
	h := newHarness(t)
	none(t, h.online("dev_door", false))
	none(t, h.online("dev_back", false))
	if got := h.m.Status().Offline; !slices.Equal(got, []string{"dev_back", "dev_door"}) {
		t.Fatalf("%v", got)
	}
	h.online("dev_door", true)
	if got := h.m.Status().Offline; !slices.Equal(got, []string{"dev_back"}) {
		t.Fatalf("%v", got)
	}
	h.online(hub, false) // the hub reports its own faults another way
	if got := h.m.Status().Offline; !slices.Equal(got, []string{"dev_back"}) {
		t.Fatalf("%v", got)
	}
}

func TestForgetClearsADeviceThatLeftTheRegistry(t *testing.T) {
	h := newHarness(t)
	h.online("dev_door", false)
	h.battery("dev_door", 5)
	h.m.Handle(Forget{Now: h.now, DeviceID: "dev_door"})
	if st := h.m.Status(); len(st.Offline) != 0 || len(st.LowBattery) != 0 {
		t.Fatalf("%+v", st)
	}
}

// --- daily summary -----------------------------------------------------------

func TestDailySummaryListsFaultsAtNineInTheMorning(t *testing.T) {
	h := newHarness(t)
	want := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	if got := h.m.NextWake(); !got.Equal(want) {
		t.Fatalf("NextWake %v, want %v", got, want)
	}
	h.online("dev_motion", false)
	h.online("dev_back", false)
	h.battery("dev_door", 12)

	none(t, h.advance(11*time.Hour+59*time.Minute)) // 08:59
	as := alerts(t, h.advance(time.Minute))         // 09:00
	if len(as) != 2 {
		t.Fatalf("want one summary per kind, got %v", as)
	}
	off, low := as[0], as[1]
	if off.Kind != AlertDeviceOffline || off.Urgency != UrgencyLow || !slices.Equal(off.Devices, []string{"dev_back", "dev_motion"}) {
		t.Fatalf("%+v", off)
	}
	if low.Kind != AlertBatteryLow || low.Urgency != UrgencyLow || !slices.Equal(low.Devices, []string{"dev_door"}) {
		t.Fatalf("%+v", low)
	}
	if off.Incident == "" || off.Incident == low.Incident {
		t.Fatalf("each alert needs its own identifier: %q %q", off.Incident, low.Incident)
	}
	if got := h.m.NextWake(); !got.Equal(want.Add(24 * time.Hour)) {
		t.Fatalf("NextWake %v", got)
	}
}

func TestSummaryRepeatsEachDayWhileTheFaultLasts(t *testing.T) {
	h := newHarness(t)
	h.online("dev_back", false)
	h.advance(12 * time.Hour)
	one(t, h.advance(24*time.Hour)) // the next day still reminds
	h.online("dev_back", true)
	none(t, h.advance(24*time.Hour)) // fixed: silence
}

func TestNoSummaryWithoutFaults(t *testing.T) {
	h := newHarness(t)
	none(t, h.advance(12*time.Hour))
}

func TestLateTickSendsASingleSummary(t *testing.T) {
	h := newHarness(t)
	h.online("dev_back", false)
	as := h.advance(5 * 24 * time.Hour) // the hub was off for five days
	one(t, as)
	if got, want := h.m.NextWake(), time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("NextWake %v, want %v", got, want)
	}
}

func TestSummaryDescribesTheStateBeforeTheInputThatTriggeredIt(t *testing.T) {
	h := newHarness(t)
	h.online("dev_back", false)
	h.now = time.Date(2026, 10, 18, 9, 0, 1, 0, time.UTC)
	as := h.m.Handle(Availability{Now: h.now, DeviceID: "dev_back", Online: true})
	if a := one(t, as); a.Kind != AlertDeviceOffline {
		t.Fatalf("%+v", a)
	}
}

func TestSummaryTimeZone(t *testing.T) {
	zone := time.FixedZone("UTC+2", 2*3600)
	h := newHarness(t, func(c *Config) { c.Location = zone })
	// 09:00 at UTC+2 is 07:00 UTC.
	if got, want := h.m.NextWake(), time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("NextWake %v, want %v", got, want)
	}
	h2 := newHarness(t)
	h2.now = time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	m, _ := New(h2.cfg, func() string { return "x" }, h2.now)
	if got, want := m.NextWake(), time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("a summary time still ahead today is used today: %v", got)
	}
}

// --- persistence -------------------------------------------------------------

func TestRestartDoesNotRepeatAnAlertAlreadySent(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	h.mains(true)
	one(t, h.hubBattery(39))

	b, err := json.Marshal(h.m.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		t.Fatal(err)
	}
	m, err := New(h.cfg, func() string { h.ids++; return fmt.Sprintf("alert-%d", h.ids) }, h.now)
	if err != nil {
		t.Fatal(err)
	}
	m.Restore(snap)
	h.m = m

	none(t, h.hubBattery(38))
	if got := one(t, h.mains(false)); got.Kind != AlertMainsRestored {
		t.Fatalf("the restoration message is still owed after a restart: %+v", got)
	}
}

func TestSnapshotRoundTripAndCopy(t *testing.T) {
	h := newHarness(t)
	h.online("dev_back", false)
	h.battery("dev_door", 5)
	h.mains(true)
	want := h.m.Snapshot()
	b, _ := json.Marshal(want)
	var got Snapshot
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	want.Offline["dev_ghost"] = true
	if h.m.Status().Offline[0] != "dev_back" || len(h.m.Status().Offline) != 1 {
		t.Fatal("changing a copy must not change the monitor")
	}

	m, _ := New(h.cfg, func() string { return "x" }, t0)
	m.Restore(Snapshot{}) // an empty snapshot must still be usable
	m.Handle(Availability{Now: t0, DeviceID: "d", Online: false})
	m.Handle(Battery{Now: t0, DeviceID: "d", Percent: 1})
}

// --- configuration ---------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no hub":                 func(c *Config) { c.HubDeviceID = "" },
		"zero critical":          func(c *Config) { c.HubBatteryCritical = 0 },
		"critical above warn":    func(c *Config) { c.HubBatteryCritical = 50 },
		"warn above rearm":       func(c *Config) { c.HubBatteryWarn = 70 },
		"rearm above 100":        func(c *Config) { c.HubBatteryRearm = 101 },
		"zero device low":        func(c *Config) { c.DeviceBatteryLow = 0 },
		"device low above rearm": func(c *Config) { c.DeviceBatteryLow = 40 },
		"hour 24":                func(c *Config) { c.SummaryAt = TimeOfDay{24, 0} },
		"minute 60":              func(c *Config) { c.SummaryAt = TimeOfDay{9, 60} },
	}
	for name, mutate := range bad {
		c := testConfig()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := New(c, func() string { return "x" }, t0); err == nil {
			t.Errorf("%s: New must refuse", name)
		}
	}
	if _, err := New(testConfig(), nil, t0); err == nil {
		t.Error("newID is required")
	}
}

func TestDefaultsMatchSpecification(t *testing.T) {
	c := DefaultConfig()
	if c.HubBatteryWarn != 40 || c.HubBatteryCritical != 10 || c.HubBatteryRearm != 60 ||
		c.DeviceBatteryLow != 15 || c.DeviceBatteryRearm != 30 || c.SummaryAt != (TimeOfDay{9, 0}) {
		t.Fatalf("defaults differ from docs/escalation.md: %+v", c)
	}
}

// A second cut before the battery recharged above 60 %: the first alert stays used
// (once per recharge), but the urgent one is due again, the hub may go dark during
// this cut, and the return of the power is announced again.
func TestTheUrgentBatteryAlertIsDueAgainAtEachCut(t *testing.T) {
	h := newHarness(t)
	h.hubBattery(100)
	h.mains(true)
	one(t, h.hubBattery(40))
	one(t, h.hubBattery(9))
	one(t, h.mains(false))
	h.hubBattery(35)
	none(t, h.mains(true))
	none(t, h.hubBattery(20))
	if a := one(t, h.hubBattery(10)); a.Urgency != UrgencyCritical {
		t.Fatalf("%+v", a)
	}
	if a := one(t, h.mains(false)); a.Kind != AlertMainsRestored {
		t.Fatalf("%+v", a)
	}
	// Already at the urgent level when the power goes again: told at once.
	if a := one(t, h.mains(true)); a.Urgency != UrgencyCritical {
		t.Fatalf("%+v", a)
	}
}
