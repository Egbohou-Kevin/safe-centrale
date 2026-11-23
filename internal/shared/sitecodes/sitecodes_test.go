package sitecodes

import (
	"errors"
	"slices"
	"testing"
	"time"

	"safehome/internal/shared/core/access"
	"safehome/internal/shared/driver"
	"safehome/internal/shared/driver/drivertest"
)

var t0 = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

// testConfig is the codes' settings at the lowest cost: fast tests.
func testConfig() access.Config {
	c := access.DefaultConfig()
	c.Iterations = 10_000
	return c
}

// The installer's tools: a code for a contact or the installer only, one code opens
// one thing, and the list says whose codes the hub leaves aside.
func TestCodes(t *testing.T) {
	store := drivertest.NewMemStore()
	cfg := testConfig()
	contacts := []Contact{{ID: "u_owner", Name: "Owner"}, {ID: "u_guard", Name: "Guard"}}
	people, err := OpenPeople(cfg, store)
	if err != nil || people.Set("u_gone", "730264") != nil {
		t.Fatal(err)
	}
	site, err := Open(cfg, contacts, store, driver.SystemClock())
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

// A duress code opens one thing too: it is in one registry only, whichever is set
// first, in the three ways (person's, installer's, duress). It is a contact's only,
// and the list shows it apart.
func TestDuressCodeOpensOneThing(t *testing.T) {
	cfg := testConfig()
	contacts := []Contact{{ID: "u_owner", Name: "Owner"}, {ID: "u_guard", Name: "Guard"}}
	open := func() *Codes {
		site, err := Open(cfg, contacts, drivertest.NewMemStore(), driver.SystemClock())
		if err != nil {
			t.Fatal(err)
		}
		return site
	}
	const code = "482915"
	person := func(s *Codes) error { return s.Set("u_owner", code) }
	guard := func(s *Codes) error { return s.Set("u_guard", code) }
	installer := func(s *Codes) error { return s.Set(InstallerUser, code) }
	duress := func(s *Codes) error { return s.SetDuress("u_guard", code) }
	ownDuress := func(s *Codes) error { return s.SetDuress("u_owner", code) }
	for _, c := range []struct {
		name          string
		first, second func(*Codes) error
	}{
		{"person then duress", person, duress},
		{"person then own duress", person, ownDuress},
		{"duress then person", duress, person},
		{"duress then own person code", duress, guard},
		{"installer then duress", installer, duress},
		{"duress then installer", duress, installer},
		{"person then installer", person, installer},
		{"installer then person", installer, person},
	} {
		site := open()
		if err := c.first(site); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if err := c.second(site); !errors.Is(err, access.ErrCodeTaken) {
			t.Errorf("%s: %v, want %v", c.name, err, access.ErrCodeTaken)
		}
	}

	site := open()
	if err := site.SetDuress(InstallerUser, "259031"); !errors.Is(err, ErrNotAContact) {
		t.Fatalf("a duress code for the installer: %v", err)
	}
	if err := site.SetDuress("u_stranger", "259031"); !errors.Is(err, ErrNotAContact) {
		t.Fatalf("a duress code for a stranger: %v", err)
	}
	if site.Set("u_owner", "730264") != nil || site.SetDuress("u_owner", "259031") != nil {
		t.Fatal("a person's code and their duress code")
	}
	l := site.List()
	if !l.Contacts[0].HasCode || !l.Contacts[0].HasDuress || l.Contacts[1].HasDuress || len(l.DuressLeftovers) != 0 {
		t.Fatalf("%+v", l)
	}
	if err := site.RemoveDuress("u_owner"); err != nil || site.List().Contacts[0].HasDuress || !site.List().Contacts[0].HasCode {
		t.Fatalf("removing the duress code: %v %+v", err, site.List())
	}
	if err := site.RemoveDuress("u_owner"); !errors.Is(err, access.ErrNoSuchUser) {
		t.Fatalf("%v", err)
	}

	// A duress code whose user is no longer a contact is listed apart.
	store := drivertest.NewMemStore()
	d, err := OpenDuress(cfg, store)
	if err != nil || d.Set("u_gone", "615273") != nil {
		t.Fatal(err)
	}
	site, err = Open(cfg, contacts, store, driver.SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	if l := site.List(); !slices.Equal(l.DuressLeftovers, []string{"u_gone"}) || len(l.Leftovers) != 0 {
		t.Fatalf("%+v", l)
	}
}

// A code refused as someone else's tells that it opens the site: three of them within
// ten minutes stop the codes from being set, so that the installer's tools cannot be
// used to try the codes one after another. The refusals are kept on disk: the
// command-line tool opens the codes afresh at every call.
func TestCodesRefusedAsTakenAreLimited(t *testing.T) {
	store := drivertest.NewMemStore()
	cfg := testConfig()
	contacts := []Contact{{ID: "u_owner", Name: "Owner"}, {ID: "u_guard", Name: "Guard"}}
	now := t0
	open := func() *Codes {
		site, err := Open(cfg, contacts, store, driver.SystemClock())
		if err != nil {
			t.Fatal(err)
		}
		site.now = func() time.Time { return now }
		return site
	}
	if err := open().Set("u_owner", "482915"); err != nil {
		t.Fatal(err)
	}
	for range takenLimit {
		if err := open().Set("u_guard", "482915"); !errors.Is(err, access.ErrCodeTaken) {
			t.Fatalf("want taken, got %v", err)
		}
	}
	if err := open().Set("u_guard", "730518"); !errors.Is(err, ErrTooManyTaken) {
		t.Fatalf("want the wait, got %v", err)
	}
	if err := open().Set("u_guard", "482915"); !errors.Is(err, ErrTooManyTaken) {
		t.Fatalf("the code of another is no longer told: %v", err)
	}
	now = now.Add(takenWindow)
	if err := open().Set("u_guard", "730518"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if user, ok := open().people.Verify("482915"); !ok || user != "u_owner" {
		t.Fatal("the owner's code is untouched")
	}
}
