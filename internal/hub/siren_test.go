package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/engine"
)

// sirenOrders lists the commands sent to the siren so far.
func sirenOrders(r *rig) []published { return r.broker.On(contract.DeviceCmdTopic(site, devSiren)) }

// sirenAnswers answers a siren command as the Zigbee adapter does.
func sirenAnswers(r *rig, order published, status, reason string) {
	r.t.Helper()
	r.send(contract.DeviceAckTopic(site, devSiren), contract.TypeCommandAck,
		contract.CommandAckData{Ref: order.Env.ID, Status: status, Reason: reason})
}

func TestASirenOrderThatDidNotTakeIsSentAgainForWhatIsLeft(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	if len(sirenOrders(r)) != 1 {
		t.Fatalf("siren orders: %d", len(sirenOrders(r)))
	}
	// The adapter was down: it saw the order after its exp, 40 s into the cycle.
	r.clock.Advance(40 * time.Second)
	sirenAnswers(r, sirenOrders(r)[0], contract.CmdExpired, "")
	drivertest.WaitFor(t, "the order again", func() bool { r.settle(); return len(sirenOrders(r)) == 2 })
	cmd := dataOf[contract.DeviceCommandData](t, sirenOrders(r)[1])
	if cmd.Command != contract.CmdSirenOn || cmd.Args.DurationS != 260 || cmd.Exp == nil ||
		!cmd.Exp.Equal(r.clock.Now().Add(30*time.Second)) {
		t.Fatalf("%+v", cmd)
	}
	// An answer that says it worked changes nothing.
	sirenAnswers(r, sirenOrders(r)[1], contract.CmdAccepted, "")
	sirenAnswers(r, sirenOrders(r)[1], contract.CmdDone, "")
	r.settle()
	if len(sirenOrders(r)) != 2 {
		t.Fatalf("siren orders: %d", len(sirenOrders(r)))
	}
	r.validateAll()
}

// sirenAlerts lists the siren_failed alert requests published so far.
func sirenAlerts(r *rig) []contract.AlertRequestData {
	var out []contract.AlertRequestData
	for _, m := range eventsOf(r, contract.TypeAlertRequest) {
		if d := dataOf[contract.AlertRequestData](r.t, m); d.Kind == contract.AlertSirenFailed {
			out = append(out, d)
		}
	}
	return out
}

// lastSirenCommand is the command of the last order sent to the siren.
func lastSirenCommand(r *rig) string {
	orders := sirenOrders(r)
	return dataOf[contract.DeviceCommandData](r.t, orders[len(orders)-1]).Command
}

// failEveryOrder answers each order of the current sounding failed, retries included,
// until the hub stops sending it again.
func failEveryOrder(r *rig) {
	r.t.Helper()
	for range 1 + sirenRetries + 1 {
		orders := sirenOrders(r)
		sirenAnswers(r, orders[len(orders)-1], contract.CmdFailed, "no_confirmation")
		r.settle()
	}
}

func TestASirenThatNeverConfirmsIsToldAFewTimesThenReported(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	failEveryOrder(r)
	if got := len(sirenOrders(r)); got != 1+3 {
		t.Fatalf("siren orders: %d, want the first and %d more", got, 3)
	}
	if !strings.Contains(r.logs.String(), "level=ERROR") || !strings.Contains(r.logs.String(), "siren") {
		t.Fatalf("the siren's failure must be logged:\n%s", r.logs.String())
	}
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(sirenAlerts(r)) > 0 })
	alerts := sirenAlerts(r)
	if a := alerts[0]; len(alerts) != 1 || a.Urgency != contract.UrgencyHigh || a.Contact != "u_owner" ||
		len(a.Devices) != 1 || a.Devices[0] != devSiren || !strings.Contains(a.Text, "sirene") {
		t.Fatalf("%+v", alerts)
	}
	mode := r.mode()
	if mode != engine.ModeTriggered {
		t.Fatalf("mode %s", mode)
	}
	r.validateAll()
}

func TestASilentSirenIsReportedOncePerAlarm(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r) // at t0+2m: cycle 1 until +7m, pause until +9m, cycle 2 until +14m
	failEveryOrder(r)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(sirenAlerts(r)) > 0 })
	incident := sirenAlerts(r)[0].Incident

	// Cycle 2: a new order, which the siren refuses as well.
	n := len(sirenOrders(r))
	r.clock.Advance(7 * time.Minute)
	drivertest.WaitFor(t, "cycle 2", func() bool {
		r.settle()
		return len(sirenOrders(r)) > n && lastSirenCommand(r) == contract.CmdSirenOn
	})
	n = len(sirenOrders(r))
	failEveryOrder(r)
	if got := len(sirenOrders(r)); got != n+sirenRetries {
		t.Fatalf("siren orders: %d, want %d", got, n+sirenRetries)
	}
	for _, a := range sirenAlerts(r) {
		if a.Incident != incident {
			t.Fatalf("a second siren alert in the same alarm: %+v", a)
		}
	}
	// The history says it once too: the alarm sounded without this siren.
	alarm := dataOf[contract.TriggeredData](t, eventsOf(r, contract.TypeAlarmTriggered)[0]).Incident
	history := eventsOf(r, contract.TypeAlarmSirenFailed)
	if len(history) != 1 || history[0].Topic != contract.AlarmEventTopic(site) ||
		dataOf[contract.SirenFailedData](t, history[0]) != (contract.SirenFailedData{Incident: alarm, DeviceID: devSiren}) {
		t.Fatalf("%+v", history)
	}
	r.validateAll()
}

// alarmOf is the incident of the alarm under way, as its first alert request says it.
func alarmOf(r *rig) string {
	return dataOf[contract.AlertRequestData](r.t, eventsOf(r, contract.TypeAlertRequest)[0]).Incident
}

// sirenAsked lists the siren's alert requests as "contact/channel", after checking
// that each names the alarm, under an incident of its own, and says whether that
// alarm is handled as handled says.
func sirenAsked(r *rig, alarm string, handled bool) []string {
	r.t.Helper()
	var out []string
	for _, a := range sirenAlerts(r) {
		urgency := contract.UrgencyHigh
		if handled {
			urgency = contract.UrgencyLow
		}
		if a.Incident == alarm || a.AlarmIncident != alarm || a.AlarmHandled != handled || a.Urgency != urgency {
			r.t.Fatalf("%+v", a)
		}
		out = append(out, a.Contact+"/"+a.Channel)
	}
	return out
}

// Before anyone answered the alarm, the siren's alert goes as it always did, and the
// alarm's own escalation goes on.
func TestASirenThatFailsBeforeAnyAcknowledgementAlertsAsToday(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	alarm := alarmOf(r)
	failEveryOrder(r)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(sirenAlerts(r)) == 3 })
	if got := sirenAsked(r, alarm, false); !sameSet(got, []string{"u_owner/app", "u_guard/sms", "u_guard/call"}) {
		t.Fatalf("%v", got)
	}
	r.clock.Advance(2 * time.Minute)
	drivertest.WaitFor(t, "the alarm's second wave", func() bool { r.settle(); return contains(requested(t, r), "u_owner/app/2") })
	r.validateAll()
}

// The trial of 24 October 2026: the owner pressed "Je gere", then the siren failed,
// and its alert rang the phones like a new alarm. It is now told without ringing:
// no call, urgency low, the alarm named and said handled, once.
func TestASirenThatFailsAfterTheAlarmWasAcknowledgedDoesNotRingAgain(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	trigger(r)
	alarm := alarmOf(r)
	r.send(contract.AlertAckTopic(site), contract.TypeAlertAck, contract.AlertAckData{Incident: alarm, Contact: "u_owner", Via: "app"})
	r.settle()
	before := len(requested(t, r))
	failEveryOrder(r)
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(sirenAlerts(r)) == 2 })
	if got := sirenAsked(r, alarm, true); !sameSet(got, []string{"u_owner/app", "u_guard/sms"}) {
		t.Fatalf("%v", got)
	}
	// The owner's phone says it got it: nothing more for anyone, never a call.
	app := requestIDs(t, r)["u_owner/app"]
	r.send(contract.AlertResultTopic(site), contract.TypeAlertResult, contract.AlertResultData{Ref: app, Status: contract.ResultDelivered})
	r.settle()
	for range 4 {
		r.clock.Advance(30 * time.Second)
		r.settle()
	}
	if got := requested(t, r); len(got) != before+2 {
		t.Fatalf("%v: only the siren's two messages may follow the acknowledgement", got)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "a call was not placed") || !strings.Contains(logs, "outcome=informed") {
		t.Fatalf("the withheld call and the outcome must be logged:\n%s", logs)
	}
	if len(eventsOf(r, contract.TypeAlarmSirenFailed)) != 1 {
		t.Fatal("the history says it once")
	}
	r.mustBe(engine.ModeTriggered)
	r.validateAll()
}

// Outside an alarm (one day a siren test or an arming beep), a silent siren is a loud
// alert of its own, as before, and nothing goes to the history.
func TestASilentSirenOutsideAnAlarmKeepsItsOwnAlert(t *testing.T) {
	r := newRig(t, nil, withContacts)
	r.start()
	err := r.hub.alarm.loop.Do(context.Background(), func(now time.Time) []engine.Action {
		r.hub.reportSilentSiren(now, devSiren)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	drivertest.WaitFor(t, "the alert", func() bool { r.settle(); return len(sirenAlerts(r)) == 3 })
	if got := sirenAsked(r, "", false); !sameSet(got, []string{"u_owner/app", "u_guard/sms", "u_guard/call"}) {
		t.Fatalf("%v", got)
	}
	if len(eventsOf(r, contract.TypeAlarmSirenFailed)) != 0 {
		t.Fatal("no alarm, no history")
	}
	r.validateAll()
}

// A hub restarted during the alarm forgets that it reported the siren, and the siren
// still refuses: the history may say it again, the phones are not told again.
func TestASilentSirenIsNotToldAgainAfterARestart(t *testing.T) {
	first := newRig(t, nil, withContacts)
	first.start()
	trigger(first) // at t0+2m: cycle 1 until +7m, pause until +9m, cycle 2 until +14m
	failEveryOrder(first)
	drivertest.WaitFor(t, "the alert", func() bool { first.settle(); return len(sirenAlerts(first)) == 3 })
	reported := sirenAlerts(first)[0].Incident
	first.stop()

	second := newRig(t, first.store, withContacts)
	second.clock.Advance(10 * time.Minute) // in cycle 2: the siren is told again
	second.start()
	drivertest.WaitFor(t, "the siren resumed", func() bool { second.settle(); return len(sirenOrders(second)) > 0 })
	failEveryOrder(second)
	drivertest.WaitFor(t, "the history", func() bool {
		second.settle()
		return len(eventsOf(second, contract.TypeAlarmSirenFailed)) == 1
	})
	second.settle()
	for _, a := range sirenAlerts(second) {
		if a.Incident != reported {
			t.Fatalf("the siren is told again after the restart: %+v", a)
		}
	}
}

func TestASirenThatTakesARetryIsNotReported(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	trigger(r)
	for range sirenRetries {
		orders := sirenOrders(r)
		sirenAnswers(r, orders[len(orders)-1], contract.CmdFailed, "no_confirmation")
		r.settle()
	}
	// The last retry works.
	orders := sirenOrders(r)
	if len(orders) != 1+sirenRetries {
		t.Fatalf("siren orders: %d", len(orders))
	}
	sirenAnswers(r, orders[len(orders)-1], contract.CmdDone, "")
	r.settle()
	if got := sirenAlerts(r); len(got) != 0 || strings.Contains(r.logs.String(), "does not take") {
		t.Fatalf("a siren that sounds is not reported: %+v", got)
	}
}

func TestAnAnswerToAnOlderSirenOrderIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.setCode("u_owner", "482915")
	trigger(r)
	on := sirenOrders(r)[0]
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
	r.mustBe(engine.ModeDisarmed)
	r.settle()
	n := len(sirenOrders(r)) // the siren.off
	sirenAnswers(r, on, contract.CmdExpired, "")
	// An answer from another device, or about an order this hub never sent.
	r.send(contract.DeviceAckTopic(site, devDoor), contract.TypeCommandAck, contract.CommandAckData{Ref: on.Env.ID, Status: contract.CmdFailed})
	r.send(contract.DeviceAckTopic(site, devSiren), contract.TypeCommandAck, contract.CommandAckData{Ref: "01NOSUCHORDER", Status: contract.CmdFailed})
	r.settle()
	if got := len(sirenOrders(r)); got != n {
		t.Fatalf("siren orders: %d, want %d: the alarm is over", got, n)
	}
}

// A device event handled while the retained states still arrive already tells the
// siren where the cycle is: the boot that follows must not tell it again.
func TestTheBootDoesNotRepeatASirenOrderOfTheBootWindow(t *testing.T) {
	first := newRig(t, nil)
	first.start()
	trigger(first) // at t0+2m: cycle 1 until +7m, pause until +9m, cycle 2 until +14m
	first.stop()

	second := newRig(t, first.store, func(c *Config) { c.BootWindow = 300 * time.Millisecond })
	second.clock.Advance(10 * time.Minute) // in cycle 2, four minutes left
	second.startWithoutWaiting()
	second.device(devDoor, contract.CapContact, "closed")
	second.clock.PassTimer(t, 300*time.Millisecond)
	drivertest.WaitFor(t, "the boot", func() bool { return len(second.broker.OfType(contract.TypeInstallMode)) > 0 })
	second.settle()
	var on []contract.DeviceCommandData
	for _, o := range sirenOrders(second) {
		if cmd := dataOf[contract.DeviceCommandData](t, o); cmd.Command == contract.CmdSirenOn {
			on = append(on, cmd)
		}
	}
	if len(on) != 1 || on[0].Args.DurationS != 240 {
		t.Fatalf("siren.on orders: %+v, want one for 240 s", on)
	}
}

// --- a fire while a siren is known silent -------------------------------------------

const devSmoke = "dev_smoke"

func withSmoke(c *Config) {
	c.Engine.Zones[devSmoke] = engine.Zone{Name: "kitchen", Kind: engine.ZoneFire}
}

// fireTexts raises a fire and returns the texts of its alert requests.
func fireTexts(r *rig) []string {
	r.t.Helper()
	r.device(devSmoke, contract.CapSmoke, contract.ValAlarm)
	var out []string
	drivertest.WaitFor(r.t, "the fire's alert", func() bool {
		r.settle()
		out = out[:0]
		for _, m := range eventsOf(r, contract.TypeAlertRequest) {
			if d := dataOf[contract.AlertRequestData](r.t, m); d.Kind == contract.AlertFire {
				out = append(out, d.Text)
			}
		}
		return len(out) > 0
	})
	return out
}

// sirenFailsThen starts an alarm whose siren refuses to sound, waits for its report,
// then lets then do what follows before a fire is raised.
func sirenFailsThen(t *testing.T, then func(r *rig)) []string {
	r := newRig(t, nil, withSmoke)
	r.start()
	r.setCode("u_owner", "482915")
	trigger(r) // at t0+2m: cycle 1 until +7m, pause until +9m, cycle 2 until +14m
	failEveryOrder(r)
	drivertest.WaitFor(t, "the siren's alert", func() bool { r.settle(); return len(sirenAlerts(r)) > 0 })
	then(r)
	texts := fireTexts(r)
	r.validateAll()
	return texts
}

// nextCycle lets the siren's next cycle start: its order is the last one sent.
func nextCycle(r *rig) {
	r.t.Helper()
	n := len(sirenOrders(r))
	r.clock.Advance(7 * time.Minute) // to the next cycle, from the start of this one
	drivertest.WaitFor(r.t, "the next cycle", func() bool {
		r.settle()
		return len(sirenOrders(r)) > n && lastSirenCommand(r) == contract.CmdSirenOn
	})
}

// soundsAgain lets the next cycle start, and its order be done.
func soundsAgain(r *rig) {
	nextCycle(r)
	sirenAnswers(r, sirenOrders(r)[len(sirenOrders(r))-1], contract.CmdDone, "")
	r.settle()
}

// doneToTheSirenOff lets the cycle end, and the siren.off of the pause be done.
func doneToTheSirenOff(r *rig) {
	r.t.Helper()
	r.clock.Advance(5 * time.Minute)
	drivertest.WaitFor(r.t, "the pause", func() bool { r.settle(); return lastSirenCommand(r) == contract.CmdSirenOff })
	sirenAnswers(r, sirenOrders(r)[len(sirenOrders(r))-1], contract.CmdDone, "")
	r.settle()
}

// doneToOthers lets the next cycle start, then answers done to an order of the cycle
// before and to a command that is not one of ours, but not to the order of this cycle.
func doneToOthers(r *rig) {
	r.t.Helper()
	older := sirenOrders(r)[0]
	nextCycle(r)
	sirenAnswers(r, older, contract.CmdDone, "")
	r.send(contract.DeviceAckTopic(site, devSiren), contract.TypeCommandAck,
		contract.CommandAckData{Ref: r.ids.New(), Status: contract.CmdDone})
	r.settle()
}

// A fire raised during an alarm whose siren is known silent says so: the people on
// the site are not warned by it. A siren whose order to sound of a later cycle is done
// is no longer known silent, until it refuses again; a done to anything else does not
// bring it back. Neither is one whose alarm is over: a fire then says nothing of it.
func TestAFireSaysTheSirenKnownSilent(t *testing.T) {
	const said = " La sirene (dev_siren) ne repond pas."
	for _, c := range []struct {
		name string
		then func(r *rig)
		say  bool
	}{
		{"during the alarm", func(*rig) {}, true},
		{"in the pause", func(r *rig) { r.clock.Advance(5 * time.Minute) }, true},
		{"after the siren sounded again", soundsAgain, false},
		{"after a done to the siren.off", doneToTheSirenOff, true},
		{"after a done to an older order or to another command", doneToOthers, true},
		{"after the siren sounded, then refused again", func(r *rig) {
			soundsAgain(r)
			nextCycle(r)
			failEveryOrder(r)
		}, true},
		{"after the alarm was disarmed", func(r *rig) {
			r.order(contract.CmdDisarm, contract.CommandArgs{Code: "482915"}, contract.OriginApp, "u_owner")
			r.mustBe(engine.ModeDisarmed)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, text := range sirenFailsThen(t, c.then) {
				if strings.Contains(text, said) != c.say || !strings.Contains(text, "ALARME INCENDIE") {
					t.Fatalf("the silent siren must be said: %v; the fire's text: %q", c.say, text)
				}
			}
		})
	}
}
