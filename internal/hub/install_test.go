package hub

import (
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

const installerCode = "259031"

// withInstaller gives the rig an installer code.
func (r *rig) withInstaller() {
	r.t.Helper()
	reg, err := openInstallerCode(testConfig().Access, r.store)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := reg.Set(InstallerUser, installerCode); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		r.t.Fatal(err)
	}
}

// installModes returns the installation modes published, in order.
func (r *rig) installModes() []contract.InstallModeData {
	var out []contract.InstallModeData
	for _, p := range r.broker.on(contract.InstallModeTopic(site)) {
		if !p.Retain {
			r.t.Error("install.mode not retained")
		}
		out = append(out, dataOf[contract.InstallModeData](r.t, p))
	}
	return out
}

func (r *rig) lastInstall() contract.InstallModeData {
	m := r.installModes()
	if len(m) == 0 {
		return contract.InstallModeData{}
	}
	return m[len(m)-1]
}

func (r *rig) openInstall() {
	r.t.Helper()
	r.keypad(contract.ValDisarm, installerCode)
	r.waitFor("the installation mode", func() bool { return r.lastInstall().Status == contract.InstallOpen })
}

func TestTheInstallerCodeOpensTheInstallationModeForHalfAnHour(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.settle()
	if m := r.installModes(); len(m) != 1 || m[0] != (contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallRestart}) {
		t.Fatalf("at start: %+v, want closed (restart)", m)
	}

	r.openInstall()
	if got := r.lastInstall().Until; got == nil || !got.Time.Equal(t0.Add(30*time.Minute)) {
		t.Fatalf("open until %v, want %v", got, t0.Add(30*time.Minute))
	}
	r.waitAcks(1)
	ack := dataOf[contract.AlarmAckData](t, eventsOf(r, contract.TypeAlarmAck)[0])
	if ack.Status != contract.AckDone || ack.Detail["install"] != contract.InstallOpen {
		t.Fatalf("the keypad's answer: %+v", ack)
	}
	if len(eventsOf(r, contract.TypeAlarmAuthFailed)) != 0 || r.mode() != engine.ModeDisarmed {
		t.Fatal("the installer code counted as a wrong code, or moved the alarm")
	}

	r.clock.Advance(29 * time.Minute)
	r.settle()
	if r.lastInstall().Status != contract.InstallOpen {
		t.Fatal("closed before its time")
	}
	r.clock.Advance(time.Minute)
	r.waitFor("the end of the mode", func() bool { return r.lastInstall().Reason == contract.InstallTimeout })
	r.validateAll()
	if strings.Contains(r.logs.String(), installerCode) {
		t.Fatal("the installer code shows in the logs")
	}
}

func TestArmingClosesTheInstallationMode(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.openInstall()
	r.keypad(contract.ValArm, "")
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Reason == contract.InstallArmed })

	// The arming's state leaves before the mode's end: the configuration's application,
	// which follows both, must know the exit delay has begun when the mode ends.
	var order []string
	for _, p := range r.broker.all() {
		switch {
		case p.Topic == contract.AlarmStateTopic(site):
			order = append(order, dataOf[contract.AlarmStateData](t, p).State)
		case p.Topic == contract.InstallModeTopic(site) && dataOf[contract.InstallModeData](t, p).Reason == contract.InstallArmed:
			order = append(order, "install closed")
		}
	}
	if i := slices.Index(order, "install closed"); i < 1 || order[i-1] != contract.StateArming {
		t.Fatalf("published in this order: %v", order)
	}

	// An older timer does nothing to a later mode.
	r.clock.Advance(30 * time.Minute)
	r.settle()
	if n := len(r.installModes()); n != 3 {
		t.Fatalf("%d install.mode, want start, open, closed", n)
	}
}

func TestTheConsoleClosesTheMode(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.openInstall()
	r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Reason == contract.InstallStopped })
	// Closing what is closed says nothing more.
	r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
	r.settle()
	if n := len(r.installModes()); n != 3 {
		t.Errorf("%d install.mode, want start, open, closed", n)
	}
}

func TestTheInstallerCodeNeverDisarms(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	r.keypad(contract.ValDisarm, installerCode)
	r.waitFor("the refusal", func() bool { return len(eventsOf(r, contract.TypeAlarmAuthFailed)) == 1 })
	r.mustBe(engine.ModeArmed)
	if r.lastInstall().Status != contract.InstallClosed {
		t.Fatal("the mode opened on an armed alarm")
	}
}

func TestOnlyTheKeypadOpensTheInstallationMode(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.order(contract.CmdDisarm, contract.CommandArgs{Code: installerCode}, contract.OriginApp, "u_owner")
	r.waitAcks(1)
	r.settle()
	if r.lastInstall().Status != contract.InstallClosed {
		t.Fatal("an order from the app opened the mode")
	}
	// Another key with the code does not either.
	r.keypad(contract.ValSilence, installerCode)
	r.waitAcks(2)
	r.settle()
	if r.lastInstall().Status != contract.InstallClosed {
		t.Fatal("silence with the installer code opened the mode")
	}
}

func TestAPersonsCodeStillDisarmsWithAnInstallerCode(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.setCode("u_owner", "482915")
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	r.keypad(contract.ValDisarm, "482915")
	r.mustBe(engine.ModeDisarmed)
}

func TestTamperIsQuietDuringTheInstallationModeButNotFire(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.Zones["dev_box"] = engine.Zone{Name: "box", Kind: engine.ZoneTamper}
		c.Engine.Zones["dev_smoke"] = engine.Zone{Name: "smoke", Kind: engine.ZoneFire}
	})
	r.withInstaller()
	r.start()
	r.openInstall()
	r.device("dev_box", contract.CapTamper, contract.ValTriggered)
	r.settle()
	if n := len(eventsOf(r, contract.TypeAlertRequest)); n != 0 {
		t.Fatalf("%d alert request(s) for a tamper during the installation mode", n)
	}
	if !strings.Contains(r.logs.String(), "not reported") {
		t.Error("the quiet tamper is not in the log")
	}
	r.device("dev_smoke", contract.CapSmoke, contract.ValAlarm)
	r.waitFor("the fire alert", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) > 0 })
	r.waitFor("the mode closed by the alarm", func() bool { return r.lastInstall().Reason == contract.InstallArmed })
}

func TestTamperAlertsAgainOnceTheModeIsClosed(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.Zones["dev_box"] = engine.Zone{Name: "box", Kind: engine.ZoneTamper}
	})
	r.withInstaller()
	r.start()
	r.openInstall()
	r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Status == contract.InstallClosed && len(r.installModes()) == 3 })
	r.device("dev_box", contract.CapTamper, contract.ValTriggered)
	r.waitFor("the tamper alert", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) > 0 })
}

// The timer of a mode that closed early goes with it, and the timer of a new mode
// starts with it: the mode lasts exactly its half hour.
func TestTheInstallTimerFollowsTheMode(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.start()
	r.openInstall()
	end := t0.Add(installLength)
	if !r.clock.pending(end) {
		t.Fatal("the mode is open without its timer")
	}
	r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Reason == contract.InstallStopped })
	r.waitFor("the timer to go", func() bool { return !r.clock.pending(end) })

	r.openInstall()
	r.clock.Advance(installLength) // at once: the timer must already run
	r.waitFor("the end of the mode", func() bool { return r.lastInstall().Reason == contract.InstallTimeout })
}
