package hub

import (
	"context"
	"slices"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/inbox"
)

// The installation mode (docs/security-install.md): the installer opens it on site by
// typing the installer code then "disarm" at the keypad, while the alarm is disarmed.
// The local console serves its Wi-Fi and its page while it lasts. It is owned by the
// engine's loop, which knows whether the alarm is disarmed.

// InstallerUser is the user of the installer code. That code is apart from the
// people's codes: it takes none of their places, and never disarms.
const InstallerUser = "installer"

// installLength is how long the mode lasts.
const installLength = contract.InstallLength * time.Second

const keyInstaller = "installer"

// installMode is the state of the mode. Only the engine's loop touches it.
type installMode struct {
	open    bool
	until   time.Time
	session int // counts the openings: the timer of an older one does nothing
	// endTimer lets the timer of the open mode go once the mode closes earlier.
	endTimer context.CancelFunc
}

// installerCode tells whether a code that is nobody's is the installer's. Only a
// "disarm" from a keypad may open the mode: never an order from the app.
func (h *Hub) installerCode(cmd contract.AlarmCommandData) bool {
	if cmd.Command != contract.CmdDisarm || cmd.Origin != contract.OriginKeypad {
		return false
	}
	user, ok := h.installer.Verify(cmd.Args.Code)
	return ok && user == InstallerUser
}

// openInstall opens the mode, if the alarm is disarmed and the keypad not locked.
// Otherwise the code counts as a wrong one: it is nobody's, and must not help guess.
func (h *Hub) openInstall(o order) {
	o.cmd.Args.Code = ""
	err := h.alarm.loop.Post(h.life, func(now time.Time) []engine.Action {
		st := h.alarm.eng.State(now)
		if st.Mode != engine.ModeDisarmed || !st.LockedUntil.IsZero() {
			h.log.Warn("installer code refused: the alarm is not disarmed, or the keypad is locked", "alarm", st.Mode)
			return h.alarm.eng.Handle(h.engineCommand(o, now, engine.Auth{Checked: true}))
		}
		h.startInstall(now)
		h.log.Warn("installation mode opened at the keypad", "until", h.alarm.inst.until.Format(time.RFC3339))
		h.out.Send(srcEngine, contract.AlarmAckTopic(h.cfg.Site), contract.TypeAlarmAck, contract.AlarmAckData{
			Ref: o.ref, Status: contract.AckDone, Detail: map[string]any{"install": contract.InstallOpen},
		}, false, now)
		h.publishInstall(now, "")
		return nil
	})
	h.submitFailed(err)
}

// startInstall opens the mode (again: a new opening starts a new half hour) and
// starts its timer. The timer is started here, on the engine's loop, so that it ends
// exactly at until. It runs on the engine's loop.
func (h *Hub) startInstall(now time.Time) {
	h.stopInstallTimer()
	h.alarm.inst.session++
	h.alarm.inst.open, h.alarm.inst.until = true, now.Add(installLength)
	fired, stop := h.deps.Clock.Timer(installLength)
	ctx, cancel := context.WithCancel(h.life)
	h.alarm.inst.endTimer = cancel
	go h.installTimer(ctx, h.alarm.inst.session, fired, stop)
}

func (h *Hub) stopInstallTimer() {
	if h.alarm.inst.endTimer != nil {
		h.alarm.inst.endTimer()
		h.alarm.inst.endTimer = nil
	}
}

// installTimer closes the mode when its time is up, unless ctx ends first.
func (h *Hub) installTimer(ctx context.Context, session int, fired <-chan time.Time, stop func()) {
	defer stop()
	select {
	case <-ctx.Done():
	case <-fired:
		err := h.alarm.loop.Post(h.life, func(now time.Time) []engine.Action {
			if h.alarm.inst.session == session {
				h.closeInstall(now, contract.InstallTimeout)
			}
			return nil
		})
		h.submitFailed(err)
	}
}

// installFollowsAlarm closes the mode once the alarm leaves the disarmed state (an
// arming, an alarm of a 24-hour zone). It runs on the engine's loop after each change.
func (h *Hub) installFollowsAlarm(now time.Time) {
	if h.alarm.inst.open && h.alarm.eng.Mode() != engine.ModeDisarmed {
		h.closeInstall(now, contract.InstallArmed)
	}
}

// closeInstall ends the mode. If the installer changed the zones or the sirens, the
// configuration's application (internal/configapp), which follows the mode, has the
// hub restart to load them at a quiet moment. It runs on the engine's loop.
func (h *Hub) closeInstall(now time.Time, reason string) {
	if !h.alarm.inst.open {
		return
	}
	h.alarm.inst.open = false
	h.stopInstallTimer()
	h.log.Info("installation mode closed", "reason", reason)
	h.publishInstall(now, reason)
}

// quietTamper drops the tamper alerts while the installation mode is open: the
// installer opens the devices (a battery, a fixing) and nobody must be woken for it.
// The engine still sees the signals, so what it knows of the devices stays right;
// fire and panic are never dropped. It runs on the engine's loop.
func (h *Hub) quietTamper(actions []engine.Action) []engine.Action {
	if !h.alarm.inst.open {
		return actions
	}
	return slices.DeleteFunc(actions, func(a engine.Action) bool {
		if al, ok := a.(engine.Alert); ok && al.Kind == engine.AlertTamper {
			h.log.Info("tamper during the installation mode: not reported", "device", deviceOf(al))
			return true
		}
		return false
	})
}

func deviceOf(a engine.Alert) string {
	if a.Cause == nil {
		return ""
	}
	return a.Cause.DeviceID
}

// publishInstall publishes the mode, retained: open when reason is empty.
func (h *Hub) publishInstall(now time.Time, reason string) {
	d := contract.InstallModeData{Status: contract.InstallClosed, Reason: reason}
	if reason == "" {
		d = contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(h.alarm.inst.until)}
	}
	h.out.Send(srcInstall, contract.InstallModeTopic(h.cfg.Site), contract.TypeInstallMode, d, true, now)
}

// installMessage takes an order of the local console: install.close.
func (h *Hub) installMessage(m inbox.Message) {
	if _, _, ok := decode[contract.InstallCloseData](h, m, contract.TypeInstallClose, "end of installation"); !ok {
		return
	}
	err := h.alarm.loop.Post(h.life, func(now time.Time) []engine.Action {
		h.closeInstall(now, contract.InstallStopped)
		return nil
	})
	h.submitFailed(err)
}
