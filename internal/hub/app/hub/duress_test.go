package hub

import (
	"testing"

	"safehome/internal/hub/core/engine"
	"safehome/internal/shared/contract"
	"safehome/internal/shared/driver"
	"safehome/internal/shared/driver/drivertest"
	"safehome/internal/shared/sitecodes"
)

const (
	ownerCode  = "730264"
	duressCode = "846029"
)

// withDuress gives the owner a code and a duress code, as the installer's tools do,
// and tells the hub (SIGHUP: ReloadCodes).
func (r *rig) withDuress() {
	r.t.Helper()
	cfg := testConfig()
	site, err := sitecodes.Open(cfg.Access, CodeContacts(cfg), r.store, driver.SystemClock())
	if err != nil {
		r.t.Fatal(err)
	}
	if err := site.Set("u_owner", ownerCode); err != nil {
		r.t.Fatal(err)
	}
	if err := site.SetDuress("u_owner", duressCode); err != nil {
		r.t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		r.t.Fatal(err)
	}
}

func keypadDisarm(code string) contract.AlarmCommandData {
	return contract.AlarmCommandData{Command: contract.CmdDisarm, Origin: contract.OriginKeypad, Args: contract.CommandArgs{Code: code}}
}

// A duress code set by the installer's tools is known once the hub is told, tells the
// engine so, and disarms as the person's own code: no refusal, no auth_failed.
func TestADuressCodeDisarmsAsAPersonsCode(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.armedNow()
	r.withDuress()

	if auth, install := r.hub.checkCode(keypadDisarm(duressCode)); install ||
		auth != (engine.Auth{Checked: true, OK: true, User: "u_owner", Duress: true}) {
		t.Fatalf("the duress code: %+v, install %t", auth, install)
	}
	if auth, _ := r.hub.checkCode(keypadDisarm(ownerCode)); auth != (engine.Auth{Checked: true, OK: true, User: "u_owner"}) {
		t.Fatalf("the owner's code: %+v", auth)
	}
	if auth, _ := r.hub.checkCode(keypadDisarm("615273")); auth != (engine.Auth{Checked: true}) {
		t.Fatalf("a wrong code: %+v", auth)
	}

	r.keypad(contract.ValDisarm, duressCode)
	drivertest.WaitFor(t, "disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)
	if n := len(eventsOf(r, contract.TypeAlarmAuthFailed)); n != 0 {
		t.Fatalf("a duress code refused %d times", n)
	}
}

// The time of a check must not tell a duress code: every order hashes the same saved
// codes, whether its code is a person's, a duress one, the installer's or nobody's.
// Hashed counts them (the PBKDF2 hashes are the time a check takes).
func TestEveryCodeTakesTheSameChecks(t *testing.T) {
	r := newRig(t, nil)
	r.withInstaller()
	r.withDuress()
	hashed := func() int64 { return r.hub.codes.Hashed() + r.hub.duress.Hashed() + r.hub.installer.Hashed() }
	want := int64(len(r.hub.codes.Users()) + len(r.hub.duress.Users()) + len(r.hub.installer.Users()))
	if want != 3 {
		t.Fatalf("%d codes saved, want 3", want)
	}
	for _, c := range []struct{ name, code string }{
		{"a person's", ownerCode},
		{"a duress", duressCode},
		{"the installer's", installerCode},
		{"nobody's", "615273"},
	} {
		for _, cmd := range []contract.AlarmCommandData{
			keypadDisarm(c.code),
			{Command: contract.CmdDisarm, Origin: contract.OriginApp, Args: contract.CommandArgs{Code: c.code}},
		} {
			before := hashed()
			r.hub.checkCode(cmd)
			if got := hashed() - before; got != want {
				t.Errorf("%s code from the %s: %d codes hashed, want %d", c.name, cmd.Origin, got, want)
			}
		}
	}
}
