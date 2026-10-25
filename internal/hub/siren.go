package hub

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	engwire "safehome/internal/engine/wire"
	"safehome/internal/escalation"
	"safehome/internal/inbox"
)

// The Zigbee adapter answers each siren command (command.ack). A siren.on that did not
// take, because the adapter saw it after its exp (it was down or stuck) or the siren
// never confirmed it, would leave the siren silent for the whole cycle: the hub sends
// it again for what is left of the sounding, a few times at most per order. A siren
// that still does not take it is reported to the contacts (siren_failed): the shop is
// silent during an alarm. The alert names that alarm, and the history says it too
// (alarm.siren_failed). Until the siren takes an order to sound again, or that alarm
// ends, it is known silent: a fire raised meanwhile says so (silentSirens). An order
// done is all the hub knows: the siren reported the state asked for when its profile
// has a confirm, and without one (the shipped siren) the order only left the adapter.
//
// A siren.off that did not take (the adapter restarting when a disarm went out) would
// leave the siren sounding for the rest of its siren.on, up to a cycle, while the
// alarm says disarmed and nobody can silence it any more: it is sent again too, a few
// times, and a siren that still does not take it is logged as an error.

// sirenRetries is how many times one siren is sent the same order again.
const sirenRetries = 3

// sirenOrder is the last order the engine gave the sirens. It is owned by the
// engine's loop.
type sirenOrder struct {
	on      bool
	until   time.Time         // end of the sounding, when on
	sent    map[string]string // command id -> siren, for the commands of this order
	retries map[string]int    // siren -> commands sent again for this order
}

// sirenMemory is what the hub knows of the sirens' failures. It is owned by the
// engine's loop.
type sirenMemory struct {
	// sirenFailed is, for each siren reported silent, the alarm it was reported for
	// and whether it took an order to sound since.
	sirenFailed map[string]sirenReport
	// sirenLogged is, for each siren whose orders do not take, what was logged about
	// it and for which failure (see logSiren).
	sirenLogged map[string]map[string]string
}

// sirenReport is a siren reported silent: the alarm it was reported for, and whether
// an order to sound was done since, in that alarm.
type sirenReport struct {
	alarm string
	back  bool
}

// orderSirens sends the engine's siren orders to every siren. A siren.on that ends
// when the current one does is the same order told again (the boot resumes the
// siren, which an input of the boot window may already have done): it is not sent.
func (h *Hub) orderSirens(now time.Time, actions []engine.Action) {
	for _, a := range actions {
		switch v := a.(type) {
		case engine.SirenOn:
			until := now.Add(v.Duration)
			if h.alarm.siren.on && h.alarm.siren.until.Equal(until) {
				continue
			}
			h.alarm.siren = sirenOrder{on: true, until: until, sent: map[string]string{}, retries: map[string]int{}}
		case engine.SirenOff:
			h.alarm.siren = sirenOrder{sent: map[string]string{}, retries: map[string]int{}}
		default:
			continue
		}
		for _, id := range h.cfg.SirenIDs {
			h.sendSiren(now, id, a)
		}
	}
}

// sendSiren sends one siren action to one siren, and remembers the command of the
// current order, to send it again if it does not take.
func (h *Hub) sendSiren(now time.Time, sirenID string, a engine.Action) {
	out, err := engwire.Translate([]engine.Action{a})
	if err != nil || len(out.Siren) != 1 {
		h.log.Error("building a siren command failed", "err", err)
		return
	}
	data := out.Siren[0].Data(sirenID, now.Add(h.cfg.SirenCommandTTL))
	ref := h.out.Send(srcEngine, contract.DeviceCmdTopic(h.cfg.Site, sirenID), contract.TypeCommand, data, false, now)
	if ref != "" && h.alarm.siren.sent != nil {
		h.alarm.siren.sent[ref] = sirenID
	}
}

// sirenAckMessage handles a siren's answer to a command: a failure, or an order done.
func (h *Hub) sirenAckMessage(sirenID string, m inbox.Message) {
	if !slices.Contains(h.cfg.SirenIDs, sirenID) {
		return
	}
	_, d, ok := decode[contract.CommandAckData](h, m, contract.TypeCommandAck, "command acknowledgement")
	if !ok || (d.Status != contract.CmdFailed && d.Status != contract.CmdExpired && d.Status != contract.CmdDone) {
		return
	}
	err := h.alarm.loop.Post(h.life, func(now time.Time) []engine.Action {
		if d.Status == contract.CmdDone {
			h.sirenTakesOrders(sirenID)
			h.sirenOrderDone(sirenID, d)
		} else {
			h.retrySiren(now, sirenID, d)
		}
		return nil
	})
	h.submitFailed(err)
}

// sirenOrderDone records a siren whose order to sound, of the current order, is done:
// one reported silent in this alarm is no longer known silent. Done is not proof that
// it sounds (see above). It runs on the engine's loop.
func (h *Hub) sirenOrderDone(sirenID string, ack contract.CommandAckData) {
	if !h.alarm.siren.on || h.alarm.siren.sent[ack.Ref] != sirenID {
		return // an older order, a siren.off, or not one of ours
	}
	if r, ok := h.alarm.sirenFailed[sirenID]; ok && !r.back && r.alarm == h.ringingAlarm() {
		r.back = true
		h.alarm.sirenFailed[sirenID] = r
		h.log.Info("the siren reported silent took an order to sound", "siren", sirenID, "alarm", r.alarm)
	}
}

// ringingAlarm is the incident the engine rings for, "" when none. It runs on the
// engine's loop.
func (h *Hub) ringingAlarm() string {
	if inc := h.alarm.eng.Snapshot().Incident; inc != nil {
		return inc.ID
	}
	return ""
}

// silentSirens lists the sirens known not to sound in the alarm under way: reported
// silent in it, and no order to sound of theirs done since. Once that alarm is over,
// nothing is known: the next alarm tells each siren again. The guard is in memory:
// after a restart, a siren is known silent again once it fails again. It runs on the
// engine's loop.
func (h *Hub) silentSirens() []string {
	alarm := h.ringingAlarm()
	var out []string
	for id, r := range h.alarm.sirenFailed {
		if alarm != "" && r.alarm == alarm && !r.back {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// retrySiren sends again a siren order of the current order that did not take: a
// siren.on for what is left of the sounding, a siren.off as it was. It runs on the
// engine's loop. A siren.on failing at the very end of the sounding is not retried and
// does not count: the next cycle tells the siren again, with its own retries.
func (h *Hub) retrySiren(now time.Time, sirenID string, ack contract.CommandAckData) {
	if h.alarm.siren.sent[ack.Ref] != sirenID {
		return // an older order, or not one of ours
	}
	delete(h.alarm.siren.sent, ack.Ref)
	if !h.alarm.siren.on {
		h.retrySirenOff(now, sirenID, ack)
		return
	}
	// The engine's time follows the clock, corrected backwards too: a sounding is never
	// longer than a cycle.
	left := min(h.alarm.siren.until.Sub(now), h.cfg.Engine.SirenCycle)
	if left < time.Second {
		return // the sounding is over anyway
	}
	if h.alarm.siren.retries[sirenID] >= sirenRetries {
		h.logSiren(slog.LevelError, sirenID, "the siren does not take the order to sound", ack, "attempts", 1+sirenRetries)
		h.reportSilentSiren(now, sirenID)
		return
	}
	h.alarm.siren.retries[sirenID]++
	h.logSiren(slog.LevelWarn, sirenID, "the siren did not take the order to sound, sending it again", ack, "left", left.Round(time.Second))
	h.sendSiren(now, sirenID, engine.SirenOn{Duration: left})
}

// retrySirenOff sends again a siren.off that did not take, a few times. It runs on
// the engine's loop.
func (h *Hub) retrySirenOff(now time.Time, sirenID string, ack contract.CommandAckData) {
	if h.alarm.siren.retries[sirenID] >= sirenRetries {
		h.logSiren(slog.LevelError, sirenID, "the siren does not take the order to stop: it sounds until its own end", ack, "attempts", 1+sirenRetries)
		return
	}
	h.alarm.siren.retries[sirenID]++
	h.logSiren(slog.LevelWarn, sirenID, "the siren did not take the order to stop, sending it again", ack)
	h.sendSiren(now, sirenID, engine.SirenOff{})
}

// reportSilentSiren tells the contacts that a siren does not sound, once per siren
// and per alarm: failing again in the next cycles tells them nothing new, but a siren
// that took an order since is known silent again. It runs on the engine's loop.
//
// The alert is an incident of its own, like a tamper, not a part of the alarm's: an
// acknowledgement of the alarm must not end it, the GSM gateway drops the messages
// of an incident already over, the cloud must not take it for a resend of the
// alarm's first wave, and its incident_at is its own. It names the alarm it belongs
// to, so that a phone already ringing for that alarm adds it there instead of
// ringing again, and the escalation tells it without ringing once that alarm is
// handled (docs/escalation.md, "Sirene muette pendant une alarme").
//
// The guard here is in memory: after a restart, the history's event may leave once
// more, but the alert does not (the escalation keeps its own guard, saved).
func (h *Hub) reportSilentSiren(now time.Time, sirenID string) {
	alarm := h.ringingAlarm()
	reported, ok := h.alarm.sirenFailed[sirenID]
	if h.alarm.sirenFailed == nil {
		h.alarm.sirenFailed = map[string]sirenReport{}
	}
	h.alarm.sirenFailed[sirenID] = sirenReport{alarm: alarm}
	if ok && reported.alarm == alarm {
		return
	}
	if alarm != "" {
		h.out.Send(srcEngine, contract.AlarmEventTopic(h.cfg.Site), contract.TypeAlarmSirenFailed,
			contract.SirenFailedData{Incident: alarm, DeviceID: sirenID}, false, now)
	}
	h.escalate(escalation.Raise{
		Now: now, Incident: h.deps.NewID(), Kind: escalation.KindSirenFailed,
		Urgency: escalation.UrgencyHigh, Devices: []string{sirenID}, Alarm: alarm,
	})
}

// logSiren logs what became of a siren order that did not take, once per siren and
// message until its failure changes or the siren takes an order again: a siren
// configured but not paired fails every order with unknown_device, its retries
// included, and the same lines every cycle drowned the log of the first real hub (25
// October 2026). The siren still counts as failed: only the log is spared. It runs on
// the engine's loop.
func (h *Hub) logSiren(level slog.Level, sirenID, msg string, ack contract.CommandAckData, args ...any) {
	why := ack.Status + "/" + ack.Reason
	if h.alarm.sirenLogged[sirenID][msg] == why {
		return
	}
	if h.alarm.sirenLogged == nil {
		h.alarm.sirenLogged = map[string]map[string]string{}
	}
	if h.alarm.sirenLogged[sirenID] == nil {
		h.alarm.sirenLogged[sirenID] = map[string]string{}
	}
	h.alarm.sirenLogged[sirenID][msg] = why
	args = append([]any{"siren", sirenID, "status", ack.Status, "reason", ack.Reason}, args...)
	h.log.Log(context.Background(), level, msg+" (logged again when this changes)", args...)
}

// sirenTakesOrders records a siren that took an order: what logSiren kept quiet about
// it is logged again the next time. It runs on the engine's loop.
func (h *Hub) sirenTakesOrders(sirenID string) {
	if _, ok := h.alarm.sirenLogged[sirenID]; ok {
		delete(h.alarm.sirenLogged, sirenID)
		h.log.Info("the siren takes orders again", "siren", sirenID)
	}
}
