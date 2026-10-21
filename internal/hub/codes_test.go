package hub

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"safehome/internal/access"
	"safehome/internal/contract"
	"safehome/internal/engine"
	"safehome/internal/escalation"
)

// --- access codes ----------------------------------------------------------------------

func TestReloadCodesPicksUpWhatAnotherProcessSaved(t *testing.T) {
	r := newRig(t, nil)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)

	// The installer's tool, in another process, opens the same saved codes and adds one.
	cfg := testConfig()
	tool, err := openCodes(cfg.Access, r.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := tool.Set("u_owner", "846029"); err != nil {
		t.Fatal(err)
	}

	// The running hub does not know it yet...
	acks := len(r.broker.ofType(contract.TypeAlarmAck))
	r.keypad(contract.ValDisarm, "846029")
	r.waitAcks(acks + 1)
	r.mustBe(engine.ModeArmed)

	// ...until it is told to look again.
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	r.keypad(contract.ValDisarm, "846029")
	r.waitFor("disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
	r.mustBe(engine.ModeDisarmed)

	// A damaged file is reported and leaves the codes in use alone.
	r.store.Save(keyAccess, []byte(`{"broken": [`))
	if err := r.hub.ReloadCodes(); err == nil {
		t.Fatal("a damaged file must be reported")
	}
	if len(r.hub.codes.Users()) != 1 {
		t.Fatal("the codes in use must survive a failed reload")
	}
}

// A person removed from the contacts (by the cloud's configuration) keeps a code on
// disk until the installer removes it: the hub leaves it aside, at start and at each
// reload, and says so without the code.
func TestTheCodeOfSomeoneNoLongerAContactIsLeftAside(t *testing.T) {
	store := newMemStore()
	tool, err := openCodes(testConfig().Access, store)
	if err != nil {
		t.Fatal(err)
	}
	for user, code := range map[string]string{"u_owner": "482915", "u_gone": "730264"} {
		if err := tool.Set(user, code); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, store)
	r.start()
	r.keypad(contract.ValArm, "")
	r.keypad(contract.ValArmNow, "")
	r.mustBe(engine.ModeArmed)
	acks := len(r.broker.ofType(contract.TypeAlarmAck))
	r.keypad(contract.ValDisarm, "730264")
	r.waitAcks(acks + 1)
	r.mustBe(engine.ModeArmed)

	if err := tool.Set("u_left", "915062"); err != nil {
		t.Fatal(err)
	}
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.codes.Users(); !slices.Equal(got, []string{"u_owner"}) {
		t.Fatalf("the hub uses the codes of %v", got)
	}
	logs := r.logs.String()
	if !strings.Contains(logs, "user=u_gone") || !strings.Contains(logs, "user=u_left") {
		t.Fatalf("the codes left aside are not said:\n%s", logs)
	}
	for _, code := range []string{"730264", "915062"} {
		if strings.Contains(logs, code) {
			t.Fatal("a code shows in the logs")
		}
	}
	if onDisk, err := openCodes(testConfig().Access, store); err != nil || len(onDisk.Users()) != 3 {
		t.Fatalf("the hub must leave the saved codes alone: %v", err)
	}
	r.keypad(contract.ValDisarm, "482915")
	r.waitFor("disarmed", func() bool { return len(eventsOf(r, contract.TypeAlarmDisarmed)) == 1 })
}

// The contacts may change while the hub runs (a configuration from the cloud during
// the installation mode, which does not restart it): a reload follows the contacts as
// saved now, not those of the start.
func TestReloadCodesFollowsTheContactsSavedNow(t *testing.T) {
	store := newMemStore()
	tool, err := openCodes(testConfig().Access, store)
	if err != nil {
		t.Fatal(err)
	}
	for user, code := range map[string]string{"u_owner": "482915", "u_new": "730264"} {
		if err := tool.Set(user, code); err != nil {
			t.Fatal(err)
		}
	}
	r := newRig(t, store)
	contacts := []string{"u_owner"}
	r.hub.deps.Contacts = func() ([]string, error) { return contacts, nil }
	r.start()

	contacts = []string{"u_owner", "u_new"} // u_new just added by the cloud
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.codes.Users(); !slices.Equal(got, []string{"u_new", "u_owner"}) {
		t.Fatalf("the hub uses the codes of %v", got)
	}
	contacts = []string{"u_new"} // the owner removed
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.codes.Users(); !slices.Equal(got, []string{"u_new"}) {
		t.Fatalf("the hub uses the codes of %v", got)
	}
	// Contacts that cannot be read: those of the start.
	r.hub.deps.Contacts = func() ([]string, error) { return nil, errors.New("damaged") }
	if err := r.hub.ReloadCodes(); err != nil {
		t.Fatal(err)
	}
	if got := r.hub.codes.Users(); !slices.Equal(got, []string{"u_owner"}) {
		t.Fatalf("the hub uses the codes of %v", got)
	}
}

// The installer's tools: a code for a contact or the installer only, one code opens
// one thing, and the list says whose codes the hub leaves aside.
func TestSiteCodes(t *testing.T) {
	store := newMemStore()
	cfg := testConfig()
	cfg.Escalation.Contacts = []escalation.Contact{{ID: "u_owner", Name: "Owner"}, {ID: "u_guard", Name: "Guard"}}
	people, err := openCodes(cfg.Access, store)
	if err != nil || people.Set("u_gone", "730264") != nil {
		t.Fatal(err)
	}
	site, err := OpenSiteCodes(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := site.Set("u_owner", "482915"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		user, code string
		want       error
	}{
		{"u_stranger", "615273", ErrNotAContact},
		{InstallerUser, "482915", access.ErrCodeTaken}, // the owner's
		{"u_guard", "1234", access.ErrWeakCode},
	} {
		if err := site.Set(c.user, c.code); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.user, err, c.want)
		}
	}
	if err := site.Set(InstallerUser, "259031"); err != nil {
		t.Fatal(err)
	}
	if err := site.Set("u_guard", "259031"); !errors.Is(err, access.ErrCodeTaken) {
		t.Fatalf("a person's code equal to the installer's: %v", err)
	}
	l := site.List()
	if len(l.Contacts) != 2 || !l.Contacts[0].HasCode || l.Contacts[0].Name != "Owner" || l.Contacts[1].HasCode ||
		!slices.Equal(l.Leftovers, []string{"u_gone"}) || !l.Installer {
		t.Fatalf("%+v", l)
	}
	if err := site.Remove(InstallerUser); err != nil || site.List().Installer {
		t.Fatalf("the installer code stays: %v", err)
	}
	if err := site.Remove("u_gone"); err != nil || len(site.List().Leftovers) != 0 {
		t.Fatalf("the leftover stays: %v", err)
	}
	if err := site.Remove("u_gone"); !errors.Is(err, access.ErrNoSuchUser) {
		t.Fatalf("%v", err)
	}
}
