package hub

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"safehome/internal/contract"
	"safehome/internal/engine"
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
	tool, err := OpenCodes(cfg.Access, r.store)
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
	tool, err := OpenCodes(testConfig().Access, store)
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
	if onDisk, err := OpenCodes(testConfig().Access, store); err != nil || len(onDisk.Users()) != 3 {
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
	tool, err := OpenCodes(testConfig().Access, store)
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
