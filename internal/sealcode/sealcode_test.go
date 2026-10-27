package sealcode

import (
	"strings"
	"testing"
	"time"
)

var at = time.Date(2026, 10, 27, 20, 0, 0, 0, time.UTC)

func TestSealThenOpen(t *testing.T) {
	k, _ := Generate()
	for _, code := range []string{"1234", "12345678", "0000"} {
		s, err := Seal(k.Public(), code, "tg-lom-0001", "alarm.disarm", at)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s, code) {
			t.Errorf("the seal shows the code")
		}
		o, err := k.Open(s, "tg-lom-0001", "alarm.disarm", at.Add(time.Minute))
		if err != nil || o.Code != code || o.Ephemeral == "" {
			t.Errorf("code %q: got %+v, %v", code, o, err)
		}
	}
}

func TestSealsAreAllTheSameSize(t *testing.T) {
	k, _ := Generate()
	a, _ := Seal(k.Public(), "1234", "s", "c", at)
	b, _ := Seal(k.Public(), "12345678", "s", "c", at)
	if len(a) != len(b) {
		t.Errorf("sizes differ: %d, %d", len(a), len(b))
	}
}

func TestWhatDoesNotOpen(t *testing.T) {
	k, _ := Generate()
	other, _ := Generate()
	s, _ := Seal(k.Public(), "1234", "tg-lom-0001", "alarm.disarm", at)
	for name, try := range map[string]func() error{
		"another hub":     func() error { _, e := other.Open(s, "tg-lom-0001", "alarm.disarm", at); return e },
		"another site":    func() error { _, e := k.Open(s, "tg-lom-0002", "alarm.disarm", at); return e },
		"another command": func() error { _, e := k.Open(s, "tg-lom-0001", "alarm.silence", at); return e },
		"too late":        func() error { _, e := k.Open(s, "tg-lom-0001", "alarm.disarm", at.Add(Window+time.Second)); return e },
		"too early":       func() error { _, e := k.Open(s, "tg-lom-0001", "alarm.disarm", at.Add(-Window-time.Second)); return e },
		"not base64":      func() error { _, e := k.Open("%%%", "tg-lom-0001", "alarm.disarm", at); return e },
		"truncated":       func() error { _, e := k.Open(s[:len(s)-8], "tg-lom-0001", "alarm.disarm", at); return e },
		"altered": func() error {
			b := []byte(s)
			b[len(b)-6] ^= 1
			_, e := k.Open(string(b), "tg-lom-0001", "alarm.disarm", at)
			return e
		},
	} {
		if err := try(); err != ErrBad {
			t.Errorf("%s: got %v, want ErrBad", name, err)
		}
	}
}

func TestOnlyACodeIsSealed(t *testing.T) {
	k, _ := Generate()
	for _, bad := range []string{"", "123", "123456789", "12a4", "1234\n"} {
		if _, err := Seal(k.Public(), bad, "s", "c", at); err == nil {
			t.Errorf("sealed %q", bad)
		}
	}
}

func TestEverySealIsNew(t *testing.T) {
	k, _ := Generate()
	a, _ := Seal(k.Public(), "1234", "s", "c", at)
	b, _ := Seal(k.Public(), "1234", "s", "c", at)
	oa, _ := k.Open(a, "s", "c", at)
	ob, _ := k.Open(b, "s", "c", at)
	if oa.Ephemeral == ob.Ephemeral {
		t.Error("two seals share their key")
	}
}

func TestKeyRoundTrip(t *testing.T) {
	k, _ := Generate()
	again, err := Parse(k.Encode())
	if err != nil || again.Public() != k.Public() {
		t.Fatalf("%v", err)
	}
	if _, err := Parse("AAAA"); err == nil {
		t.Error("parsed junk")
	}
}

// A seal made elsewhere (the app's Kotlin) must open here: the private key and the
// seal below were produced by this package once and are fixed, so that a change of
// format fails here and in the app's test, which holds the same two lines.
func TestTheAppsVector(t *testing.T) {
	k, err := Parse(vectorKey)
	if err != nil {
		t.Fatal(err)
	}
	if k.Public() != vectorPublic {
		t.Fatal("the public key of the vector changed")
	}
	o, err := k.Open(vectorSeal, "tg-lom-0001", "alarm.disarm", time.Unix(vectorTime, 0))
	if err != nil || o.Code != "4821" {
		t.Fatalf("%+v, %v", o, err)
	}
}
