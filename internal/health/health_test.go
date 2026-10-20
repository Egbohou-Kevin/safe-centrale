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
	h.mains(true)
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

// --- GSM backup -----------------------------------------------------------

func (h *harness) gsm(usable bool) []Action { return h.m.Handle(GSM{Now: h.now, Usable: usable}) }

func TestTheOwnerIsToldWhenTheGSMBackupStaysDown(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(true))
	none(t, h.gsm(false))
	if !h.m.NextWake().Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("next wake %v: the alert is due ten minutes after the outage began", h.m.NextWake())
	}
	none(t, h.advance(9*time.Minute))
	none(t, h.gsm(false)) // still down: the outage keeps its start
	al := one(t, h.advance(time.Minute))
	if al.Kind != AlertGSMDegraded || al.Urgency != UrgencyHigh || al.Incident == "" {
		t.Fatalf("%+v", al)
	}
	none(t, h.advance(time.Hour)) // once per outage
	if !h.m.NextWake().Equal(h.m.Snapshot().NextSummary) {
		t.Fatal("nothing else is due")
	}
	// Back, then down again: a new outage, a new alert after its own delay.
	none(t, h.gsm(true))
	none(t, h.gsm(false))
	none(t, h.advance(5*time.Minute))
	one(t, h.advance(5*time.Minute))
}

// keepReporting has the gateway repeat a status every minute for d.
func (h *harness) keepReporting(usable bool, d time.Duration) []Action {
	var out []Action
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(time.Minute)
		out = append(out, h.gsm(usable)...)
	}
	return out
}

func TestAShortGSMDropIsNotWorthAMessage(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(false))
	none(t, h.advance(2*time.Minute))
	none(t, h.gsm(true))
	none(t, h.keepReporting(true, time.Hour))
	if h.m.Status().GSM != LinkOK {
		t.Fatalf("status %q", h.m.Status().GSM)
	}
}

func TestNothingIsKnownOfTheGSMBeforeTheGatewaySpeaks(t *testing.T) {
	h := newHarness(t)
	none(t, h.advance(time.Hour))
	if h.m.Status().GSM != LinkUnknown || !h.m.NextWake().Equal(h.m.Snapshot().NextSummary) {
		t.Fatalf("status %q, next wake %v: a site without a gateway is not watched", h.m.Status().GSM, h.m.NextWake())
	}
}

func TestASilentGatewayCountsAsDown(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(true))
	if !h.m.NextWake().Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("next wake %v: the silence is checked when it becomes too long", h.m.NextWake())
	}
	none(t, h.advance(3*time.Minute-time.Second))
	if h.m.Status().GSM != LinkOK {
		t.Fatalf("status %q before the limit", h.m.Status().GSM)
	}
	none(t, h.advance(time.Second))
	if h.m.Status().GSM != LinkSilent {
		t.Fatalf("status %q: the gateway stopped giving news", h.m.Status().GSM)
	}
	// The outage began when the silence became too long.
	if !h.m.NextWake().Equal(t0.Add(13 * time.Minute)) {
		t.Fatalf("next wake %v", h.m.NextWake())
	}
	none(t, h.advance(10*time.Minute-time.Second))
	al := one(t, h.advance(time.Second))
	if al.Kind != AlertGSMDegraded || al.Urgency != UrgencyHigh {
		t.Fatalf("%+v", al)
	}
	none(t, h.advance(time.Hour))
	// It comes back: working again, and a later silence is a new outage.
	none(t, h.gsm(true))
	if h.m.Status().GSM != LinkOK || h.m.Snapshot().GSM.Alerted {
		t.Fatalf("status %q, %+v", h.m.Status().GSM, h.m.Snapshot())
	}
	none(t, h.advance(12*time.Minute))
	one(t, h.advance(time.Minute))
}

func TestASilentGatewayThatSaidItWasDownKeepsItsOutage(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(false))
	none(t, h.advance(5*time.Minute)) // silent as well now
	if h.m.Status().GSM != LinkSilent || !h.m.Snapshot().GSM.DownSince.Equal(t0) {
		t.Fatalf("status %q, %+v", h.m.Status().GSM, h.m.Snapshot())
	}
	one(t, h.advance(5*time.Minute))
}

func TestAnOldStatusFoundAtStartIsAlreadySilent(t *testing.T) {
	// The retained status of a gateway that died two hours ago.
	h := newHarness(t)
	al := one(t, h.m.Handle(GSM{Now: h.now, Seen: h.now.Add(-2 * time.Hour), Usable: true}))
	if al.Kind != AlertGSMDegraded {
		t.Fatalf("%+v", al)
	}
	if h.m.Status().GSM != LinkSilent {
		t.Fatalf("status %q", h.m.Status().GSM)
	}
}

func TestAStatusFromTheFutureCountsAsWrittenNow(t *testing.T) {
	h := newHarness(t)
	none(t, h.m.Handle(GSM{Now: h.now, Seen: h.now.Add(time.Hour), Usable: true}))
	if !h.m.Snapshot().GSM.Seen.Equal(t0) {
		t.Fatalf("seen %v", h.m.Snapshot().GSM.Seen)
	}
	none(t, h.advance(3*time.Minute))
	if h.m.Status().GSM != LinkSilent {
		t.Fatalf("status %q: a clock set back must not hide a dead gateway for an hour", h.m.Status().GSM)
	}
}

func TestTheGSMAlertSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.gsm(false)
	one(t, h.advance(10*time.Minute))
	raw, _ := json.Marshal(h.m.Snapshot())
	var s Snapshot
	json.Unmarshal(raw, &s)
	again := newHarness(t)
	again.now = h.now
	again.m.Restore(s)
	none(t, again.advance(time.Hour))
}

func TestGSMDelaysMustBePositive(t *testing.T) {
	for _, f := range []func(*Config){
		func(c *Config) { c.GSMDegradedAfter = 0 },
		func(c *Config) { c.GSMSilentAfter = 0 },
		func(c *Config) { c.ZigbeeSilentAfter = 0 },
		func(c *Config) { c.ZigbeeDownAfter = 0 },
	} {
		c := testConfig()
		f(&c)
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	if d := DefaultConfig(); d.GSMDegradedAfter != 10*time.Minute || d.GSMSilentAfter != 3*time.Minute ||
		d.ZigbeeSilentAfter != 3*time.Minute || d.ZigbeeDownAfter != 3*time.Minute {
		t.Fatal("defaults")
	}
}

// --- Zigbee adapter ----------------------------------------------------------

func (h *harness) zigbee(usable bool) []Action {
	return h.m.Handle(Zigbee{Now: h.now, Usable: usable})
}

func TestASilentZigbeeAdapterIsABlindAlarm(t *testing.T) {
	h := newHarness(t)
	none(t, h.zigbee(true))
	if h.m.Status().Zigbee != LinkOK || h.m.Status().GSM != LinkUnknown {
		t.Fatalf("%+v", h.m.Status())
	}
	if !h.m.NextWake().Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("next wake %v", h.m.NextWake())
	}
	none(t, h.advance(3*time.Minute))
	if h.m.Status().Zigbee != LinkSilent {
		t.Fatalf("%+v", h.m.Status())
	}
	none(t, h.advance(3*time.Minute-time.Second))
	al := one(t, h.advance(time.Second))
	if al.Kind != AlertZigbeeDown || al.Urgency != UrgencyHigh {
		t.Fatalf("%+v", al)
	}
	none(t, h.advance(time.Hour)) // once per outage
}

func TestAZigbeeBridgeDownIsToldAfterThreeMinutes(t *testing.T) {
	h := newHarness(t)
	none(t, h.zigbee(false))
	if h.m.Status().Zigbee != LinkDown {
		t.Fatalf("%+v", h.m.Status())
	}
	h.now = h.now.Add(2 * time.Minute)
	none(t, h.zigbee(true)) // Zigbee2MQTT restarted in time: nothing to tell
	h.now = h.now.Add(time.Minute)
	none(t, h.zigbee(false))
	h.now = h.now.Add(time.Minute)
	none(t, h.zigbee(false))
	h.now = h.now.Add(2 * time.Minute)
	if al := one(t, h.zigbee(false)); al.Kind != AlertZigbeeDown {
		t.Fatalf("%+v", al)
	}
}

func TestTheGSMAndZigbeeWatchesAreIndependent(t *testing.T) {
	h := newHarness(t)
	h.gsm(false)
	h.zigbee(false)
	als := alerts(t, h.advance(3*time.Minute))
	if len(als) != 1 || als[0].Kind != AlertZigbeeDown {
		t.Fatalf("%+v", als)
	}
	als = alerts(t, h.advance(7*time.Minute))
	if len(als) != 1 || als[0].Kind != AlertGSMDegraded {
		t.Fatalf("%+v", als)
	}
}

// --- link agent (internet) ---------------------------------------------------

func (h *harness) internet(usable bool) []Action {
	return h.m.Handle(Internet{Now: h.now, Usable: usable})
}

func TestTheInternetIsWatchedButNobodyIsTold(t *testing.T) {
	h := newHarness(t)
	if h.m.Status().Internet != LinkUnknown {
		t.Fatalf("%+v", h.m.Status())
	}
	none(t, h.internet(true))
	if h.m.Status().Internet != LinkOK {
		t.Fatalf("%+v", h.m.Status())
	}
	if !h.m.NextWake().Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("next wake %v", h.m.NextWake())
	}
	none(t, h.advance(3*time.Minute))
	if h.m.Status().Internet != LinkSilent {
		t.Fatalf("a silent agent: %+v", h.m.Status())
	}
	// Once silent, nothing more is due for it: the next wake is the daily summary.
	if wake := h.m.NextWake(); wake.Before(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v: the internet needs no more looks", wake)
	}
	none(t, h.internet(false))
	if h.m.Status().Internet != LinkDown {
		t.Fatalf("%+v", h.m.Status())
	}
	for range 5 {
		for _, a := range h.advance(time.Hour) {
			if al, ok := a.(Alert); ok {
				t.Fatalf("an internet outage was told: %+v", al)
			}
		}
	}
	none(t, h.internet(true))
	if h.m.Status().Internet != LinkOK {
		t.Fatalf("back: %+v", h.m.Status())
	}
}

func TestTheInternetDelayMustBePositive(t *testing.T) {
	c := DefaultConfig()
	c.HubDeviceID = "dev_hubpower"
	if c.InternetSilentAfter != 3*time.Minute {
		t.Fatalf("default %v", c.InternetSilentAfter)
	}
	c.InternetSilentAfter = 0
	if err := c.Validate(); err == nil {
		t.Fatal("a zero internet delay was accepted")
	}
}
