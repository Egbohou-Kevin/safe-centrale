package hub

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"safehome/internal/hub/core/engine"
	"safehome/internal/hub/core/escalation"
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

// Removing a person's code removes their duress code too: a person whose access is
// removed (a lost phone, a former employee) must no longer disarm in any way.
func TestRemovingAPersonsCodeRemovesTheirDuressCode(t *testing.T) {
	r := newRig(t, nil)
	r.withDuress()
	cfg := testConfig()
	site, err := sitecodes.Open(cfg.Access, CodeContacts(cfg), r.store, driver.SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	if err := site.Remove("u_owner"); err != nil {
		t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if auth, install := r.hub.checkCode(keypadDisarm(duressCode)); install || auth != (engine.Auth{Checked: true}) {
		t.Fatalf("the duress code of a removed person: %+v, install %t", auth, install)
	}
}

// A duress code goes with its person's code, at start and at each reload: one without
// it (left on disk by an older tool, or its person no longer a contact) is left aside,
// and silently: the log says once that a former contact's code is left aside, never
// that a duress code exists.
func TestADuressCodeWithoutItsPersonsCodeIsLeftAsideSilently(t *testing.T) {
	store := drivertest.NewMemStore()
	cfg := testConfig().Access
	people, err := sitecodes.OpenPeople(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	duress, err := sitecodes.OpenDuress(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if people.Set("u_gone", "482915") != nil || duress.Set("u_gone", "730264") != nil || duress.Set("u_owner", duressCode) != nil {
		t.Fatal("the codes on disk")
	}
	r := newRig(t, store)
	if got := r.hub.duress.Users(); len(got) != 0 {
		t.Fatalf("at start, the hub uses the duress codes of %v", got)
	}
	gone := func() int { return strings.Count(r.logs.String(), "user=u_gone") }
	before := gone()
	if before != 1 {
		t.Fatalf("at start, %d lines for the former contact, want 1:\n%s", before, r.logs.String())
	}
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.duress.Users(); len(got) != 0 {
		t.Fatalf("after a reload, the hub uses the duress codes of %v", got)
	}
	if n := gone() - before; n != 1 {
		t.Fatalf("a reload logs %d lines for the former contact, want 1:\n%s", n, r.logs.String())
	}
	if auth, _ := r.hub.checkCode(keypadDisarm(duressCode)); auth != (engine.Auth{Checked: true}) {
		t.Fatalf("a duress code without its person's code: %+v", auth)
	}

	// Its person's code set, the duress code is known.
	if people.Set("u_owner", ownerCode) != nil || r.hub.ReloadCodes() != nil {
		t.Fatal("the owner's code")
	}
	if auth, _ := r.hub.checkCode(keypadDisarm(duressCode)); !auth.Duress {
		t.Fatalf("the duress code beside its person's code: %+v", auth)
	}
	if strings.Contains(strings.ToLower(r.logs.String()), "duress") {
		t.Fatal("the log tells a duress code")
	}
}

// A full site (as many people's and duress codes as places, and the installer's) is
// checked within engine.CodeCheckHold: the three registries are checked at once, so a
// check takes the time of the largest (MaxUsers codes, about a second each on the hub,
// docs/engine.md), not of the three together, which would be more than the hold.
func TestAFullSiteIsCheckedWithinTheHold(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 3 {
		t.Skip("needs three processors to tell the checks run at once")
	}
	cfg := testConfig()
	max := cfg.Access.MaxUsers
	if perCode := time.Second; time.Duration(max)*perCode > engine.CodeCheckHold ||
		time.Duration(2*max+1)*perCode <= engine.CodeCheckHold {
		t.Fatalf("%d codes a registry: the hold of %s no longer tells", max, engine.CodeCheckHold)
	}
	contacts := func(c *Config) {
		for i := len(c.Escalation.Contacts); i < max; i++ {
			c.Escalation.Contacts = append(c.Escalation.Contacts, escalation.Contact{
				ID: fmt.Sprintf("u_%d", i), Name: fmt.Sprintf("Person %d", i), Role: escalation.RoleOther,
				Channels: []escalation.Channel{escalation.ChannelSMS},
			})
		}
	}
	r := newRig(t, nil, contacts)
	contacts(&cfg)
	slow := cfg.Access
	slow.Iterations = 100_000 // slow enough for the time to tell, the hashes of the hub
	site, err := sitecodes.Open(slow, CodeContacts(cfg), r.store, driver.SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	people := []string{"482915", "730264", "915062", "260748", "609317"}
	duress := []string{"846029", "350981", "172604", "938052", "514790"}
	for i, ct := range CodeContacts(cfg) {
		if site.Set(ct.ID, people[i]) != nil || site.SetDuress(ct.ID, duress[i]) != nil {
			t.Fatalf("the codes of %s", ct.ID)
		}
	}
	if site.Set(InstallerUser, installerCode) != nil || r.hub.ReloadCodes() != nil {
		t.Fatal("the installer code")
	}
	if n := len(r.hub.codes.Users()) + len(r.hub.duress.Users()) + len(r.hub.installer.Users()); n != 2*max+1 {
		t.Fatalf("%d codes, want %d", n, 2*max+1)
	}
	const wrong = "615273"
	start := time.Now()
	r.hub.codes.Verify(wrong)
	r.hub.duress.Verify(wrong)
	r.hub.installer.Verify(wrong)
	oneByOne := time.Since(start)
	start = time.Now()
	r.hub.checkCode(keypadDisarm(wrong))
	atOnce := time.Since(start)
	// At once: about max codes' time out of 2*max+1 (5 of 11); one by one, all of them.
	if atOnce*4 > oneByOne*3 {
		t.Fatalf("a check of a full site takes %s, the registries one by one %s: they are not checked at once", atOnce, oneByOne)
	}
}
