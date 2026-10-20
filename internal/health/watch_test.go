package health

import (
	"encoding/json"
	"testing"
	"time"
)

// --- GSM backup -----------------------------------------------------------

func (h *harness) gsm(usable bool) []Action { return h.m.Handle(GSM{Now: h.now, Usable: usable}) }

func TestTheOwnerIsToldWhenTheGSMBackupStaysDown(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(true))
	none(t, h.gsm(false))
	if !h.m.NextWake().Equal(t0.Add(3 * time.Minute)) {
		t.Fatalf("next wake %v: the gateway turns silent three minutes after its last status", h.m.NextWake())
	}
	none(t, h.advance(3*time.Minute))
	if h.m.Status().GSM != LinkSilent {
		t.Fatalf("status %q: a gateway that said it was down and then went quiet is silent", h.m.Status().GSM)
	}
	if !h.m.NextWake().Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("next wake %v: the alert is due ten minutes after the outage began", h.m.NextWake())
	}
	none(t, h.advance(6*time.Minute))
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

func TestAStatusOlderThanTheLastOneIsIgnored(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(true))
	h.now = h.now.Add(time.Minute)
	// A retained "down" written before the last status, delivered late.
	none(t, h.m.Handle(GSM{Now: h.now, Seen: t0.Add(-time.Minute), Usable: false}))
	if s := h.m.Snapshot().GSM; !s.Usable || !s.Seen.Equal(t0) {
		t.Fatalf("the late status must not replace the newer one: %+v", s)
	}
}

func TestStatusesAreAcceptedAgainAfterTheClockWentBack(t *testing.T) {
	h := newHarness(t)
	none(t, h.gsm(true))
	h.now = t0.Add(-time.Hour) // the clock was set back an hour
	none(t, h.gsm(false))
	if s := h.m.Snapshot().GSM; s.Usable || !s.Seen.Equal(h.now) {
		t.Fatalf("a status written after the clock went back must count: %+v", s)
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
