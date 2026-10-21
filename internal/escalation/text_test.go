package escalation

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- texts -------------------------------------------------------------------------------

func TestTextsAreShortPlainASCIIAndSayWhatToDo(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Location = time.FixedZone("UTC+1", 3600) })
	out := raise(e, t0, KindIntrusion, "dev_back")
	var text string
	for _, a := range out {
		if s, ok := a.(Send); ok {
			text = s.Text
		}
	}
	for _, part := range []string{"ALARME", "Boutique Lome", "Porte arriere", "22:04", "Repondre 1"} {
		if !strings.Contains(text, part) {
			t.Errorf("%q misses %q", text, part)
		}
	}
	checkText(t, text)
}

func checkText(t *testing.T, text string) {
	t.Helper()
	if len(text) > 160 {
		t.Errorf("%d characters: %q", len(text), text)
	}
	for _, r := range text {
		if r < 0x20 || r > 0x7e {
			t.Errorf("%q has a character outside plain ASCII: %q", text, r)
		}
	}
}

func TestEveryKindHasASuitableText(t *testing.T) {
	for _, kind := range allKinds {
		e := newEsc(t)
		out := e.Handle(Raise{Now: t0, Incident: "x", Kind: kind, Urgency: UrgencyHigh, Percent: 40, Devices: []string{"dev_back", "dev_a"}})
		found := false
		for _, a := range out {
			if s, ok := a.(Send); ok {
				found = true
				checkText(t, s.Text)
				if !strings.Contains(s.Text, "Boutique Lome") {
					t.Errorf("%s: %q does not name the site", kind, s.Text)
				}
				ack := strings.Contains(s.Text, "Repondre 1")
				wantAck := kind != KindMainsRestored && kind != KindDeviceOffline && kind != KindBatteryLow && kind != KindGSMDegraded
				if ack != wantAck {
					t.Errorf("%s: answer hint present=%v, want %v: %q", kind, ack, wantAck, s.Text)
				}
			}
		}
		if !found {
			t.Errorf("%s: nothing sent", kind)
		}
	}
}

func TestALongTextIsCutButKeepsTheWayToAnswer(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.SiteName = strings.Repeat("Grande Boutique ", 5) })
	long := make([]string, 6)
	for i := range long {
		long[i] = fmt.Sprintf("dev_%d", i)
	}
	out := raise(e, t0, KindIntrusion, long...)
	for _, a := range out {
		if s, ok := a.(Send); ok {
			checkText(t, s.Text)
			if !strings.HasSuffix(s.Text, "Repondre 1 si vous gerez.") || !strings.Contains(s.Text, "...") {
				t.Fatalf("%q", s.Text)
			}
		}
	}
}

func TestASCIIRemovesAccentsAndOddCharacters(t *testing.T) {
	if got := ascii("Boutique Lom\u00e9 \u00c9t\u00e9 \u2019 \u4e2d\u6587 \u0153uf"); got != "Boutique Lome Ete ' oeuf" && got != "Boutique Lome Ete '  oeuf" {
		t.Fatalf("%q", got)
	}
}

// The engine bypasses an open zone at the end of an exit delay, whoever started it:
// the automatic arming or a person.
func TestTheBypassTextFitsEveryArming(t *testing.T) {
	e := newEsc(t)
	out := raise(e, t0, KindArmingBypassed, "dev_back")
	for _, a := range out {
		s, ok := a.(Send)
		if !ok {
			continue
		}
		for _, part := range []string{"armee", "zone ouverte", "Porte arriere", "non protegee"} {
			if !strings.Contains(s.Text, part) {
				t.Errorf("%q misses %q", s.Text, part)
			}
		}
		if strings.Contains(s.Text, "automatique") {
			t.Errorf("%q: a person may have armed", s.Text)
		}
		checkText(t, s.Text)
	}
}
