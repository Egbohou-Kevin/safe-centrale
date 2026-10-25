package hub

import (
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
// ends, it is known silent: a fire raised meanwhile says so (silentSirens).

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

// sirenReport is a siren reported silent: the alarm it was reported for, and whether
// it took an order to sound since, in that alarm.
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
			h.alarm.siren = sirenOrder{}
		default:
			continue
		}
		for _, id := range h.cfg.SirenIDs {
			h.sendSiren(now, id, a)
		}
	}
}

// sendSiren sends one siren action to one siren, and remembers the command while
// the siren must sound.
func (h *Hub) sendSiren(now time.Time, sirenID string, a engine.Action) {
	out, err := engwire.Translate([]engine.Action{a})
	if err != nil || len(out.Siren) != 1 {
		h.log.Error("building a siren command failed", "err", err)
		return
	}
	data := out.Siren[0].Data(sirenID, now.Add(h.cfg.SirenCommandTTL))
	ref := h.out.Send(srcEngine, contract.DeviceCmdTopic(h.cfg.Site, sirenID), contract.TypeCommand, data, false, now)
	if ref != "" && h.alarm.siren.on {
		h.alarm.siren.sent[ref] = sirenID
	}
}

// sirenAckMessage handles a siren's answer to a command: a failure, or a siren that
// carried out the order (done).
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
			h.sirenSounds(sirenID, d)
		} else {
			h.retrySiren(now, sirenID, d)
		}
		return nil
	})
	h.submitFailed(err)
}

// sirenSounds records a siren that carried out an order to sound of the current
// order: one reported silent in this alarm is no longer known silent. It runs on the
// engine's loop.
func (h *Hub) sirenSounds(sirenID string, ack contract.CommandAckData) {
	if h.alarm.siren.sent[ack.Ref] != sirenID {
		return // an older order, a siren.off, or not one of ours
	}
	if r, ok := h.alarm.sirenFailed[sirenID]; ok && !r.back && r.alarm == h.ringingAlarm() {
		r.back = true
		h.alarm.sirenFailed[sirenID] = r
		h.log.Info("the siren reported silent sounds again", "siren", sirenID, "alarm", r.alarm)
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
// silent in it, and not heard carrying out an order to sound since. Once that alarm
// is over, nothing is known: the next alarm tells each siren again. The guard is in
// memory: after a restart, a siren is known silent again once it fails again. It
// runs on the engine's loop.
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

// retrySiren sends again a siren.on of the current order that did not take, for what
// is left of the sounding. It runs on the engine's loop. A failure at the very end of
// the sounding is not retried and does not count: the next cycle tells the siren
// again, with its own retries.
func (h *Hub) retrySiren(now time.Time, sirenID string, ack contract.CommandAckData) {
	if h.alarm.siren.sent[ack.Ref] != sirenID {
		return // an older order, or not one of ours
	}
	delete(h.alarm.siren.sent, ack.Ref)
	left := h.alarm.siren.until.Sub(now)
	if left < time.Second {
		return // the sounding is over anyway
	}
	if h.alarm.siren.retries[sirenID] >= sirenRetries {
		h.log.Error("the siren does not take the order to sound", "siren", sirenID,
			"status", ack.Status, "reason", ack.Reason, "attempts", 1+sirenRetries)
		h.reportSilentSiren(now, sirenID)
		return
	}
	h.alarm.siren.retries[sirenID]++
	h.log.Warn("the siren did not take the order to sound, sending it again", "siren", sirenID,
		"status", ack.Status, "reason", ack.Reason, "left", left.Round(time.Second))
	h.sendSiren(now, sirenID, engine.SirenOn{Duration: left})
}

// reportSilentSiren tells the contacts that a siren does not sound, once per siren
// and per alarm: failing again in the next cycles tells them nothing new, but a siren
// that sounded since is known silent again. It runs on the engine's loop.
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
