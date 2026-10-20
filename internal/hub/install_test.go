package hub

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/engine"
)

const installerCode = "259031"

// withInstaller gives the rig an installer code, and counts the restarts asked for.
func (r *rig) withInstaller(changed bool) *atomic.Int32 {
	r.t.Helper()
	reg, err := OpenInstallerCode(testConfig().Access, r.store)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := reg.Set(InstallerUser, installerCode); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		r.t.Fatal(err)
	}
	restarts := &atomic.Int32{}
	r.hub.deps.DevicesChanged = func() bool { return changed }
	r.hub.deps.RestartWhenQuiet = func() { restarts.Add(1) }
	return restarts
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
	r.withInstaller(false)
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
	r.withInstaller(false)
	r.start()
	r.openInstall()
	r.keypad(contract.ValArm, "")
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Reason == contract.InstallArmed })

	// An older timer does nothing to a later mode.
	r.clock.Advance(30 * time.Minute)
	r.settle()
	if n := len(r.installModes()); n != 3 {
		t.Fatalf("%d install.mode, want start, open, closed", n)
	}
}

func TestTheConsoleClosesTheModeAndTheHubRestartsForNewZones(t *testing.T) {
	for _, changed := range []bool{false, true} {
		r := newRig(t, nil)
		restarts := r.withInstaller(changed)
		r.start()
		r.openInstall()
		r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
		r.waitFor("the mode closed", func() bool { return r.lastInstall().Reason == contract.InstallStopped })
		want := int32(0)
		if changed {
			want = 1
		}
		if got := restarts.Load(); got != want {
			t.Errorf("devices changed %v: %d restart(s), want %d", changed, got, want)
		}
		// Closing what is closed says nothing more.
		r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
		r.settle()
		if n := len(r.installModes()); n != 3 {
			t.Errorf("%d install.mode, want start, open, closed", n)
		}
	}
}

func TestTheInstallerCodeNeverDisarms(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller(false)
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
	r.withInstaller(false)
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
	r.withInstaller(false)
	r.setCode("u_owner", "482915")
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	r.keypad(contract.ValDisarm, "482915")
	r.mustBe(engine.ModeDisarmed)
}

func TestRestartAndDevicesChangedGoTogether(t *testing.T) {
	_, err := New(testConfig(), Deps{Clock: newFakeClock(), Store: newMemStore(), Publisher: &fakeBroker{},
		NewID: contract.NewIDGenerator().New, DevicesChanged: func() bool { return true }})
	if err == nil {
		t.Fatal("DevicesChanged without RestartWhenQuiet was accepted")
	}
}

func TestTamperIsQuietDuringTheInstallationModeButNotFire(t *testing.T) {
	r := newRig(t, nil, func(c *Config) {
		c.Engine.Zones["dev_box"] = engine.Zone{Name: "box", Kind: engine.ZoneTamper}
		c.Engine.Zones["dev_smoke"] = engine.Zone{Name: "smoke", Kind: engine.ZoneFire}
	})
	r.withInstaller(false)
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
	r.withInstaller(false)
	r.start()
	r.openInstall()
	r.send(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{})
	r.waitFor("the mode closed", func() bool { return r.lastInstall().Status == contract.InstallClosed && len(r.installModes()) == 3 })
	r.device("dev_box", contract.CapTamper, contract.ValTriggered)
	r.waitFor("the tamper alert", func() bool { return len(eventsOf(r, contract.TypeAlertRequest)) > 0 })
}
