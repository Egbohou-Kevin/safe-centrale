package escalation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- texts -------------------------------------------------------------------------------

func TestTextsAreShortPlainASCIIAndSayWhatToDo(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Location = time.FixedZone("UTC+1", 3600) })
	out := raise(e, t0, KindIntrusion, "dev_back")
	text := textOf(t, out, ChannelSMS)
	for _, part := range []string{"ALARME", "Boutique Lome", "Porte arriere", "22:04", "Repondre 1"} {
		if !strings.Contains(text, part) {
			t.Errorf("%q misses %q", text, part)
		}
	}
	checkText(t, text)
}

// textOf returns the text of the first message sent through a channel.
func textOf(t *testing.T, out []Action, ch Channel) string {
	t.Helper()
	for _, a := range out {
		if s, ok := a.(Send); ok && s.Channel == ch {
			return s.Text
		}
	}
	t.Fatalf("nothing sent by %s: %v", ch, sends(out))
	return ""
}

// The trial on real hardware: the application showed "Repondre 1 si vous gerez.", an
// SMS instruction, next to its own "Je gere" button. Only an SMS says how to answer.
func TestOnlyAnSMSSaysToAnswerBySMS(t *testing.T) {
	e := newEsc(t)
	in := IncidentSnap{ID: "x", Kind: KindIntrusion, RaisedAt: t0, Devices: []string{"dev_back"}}
	want := "ALARME Boutique Lome: intrusion (Porte arriere) a 21:04."
	for ch, text := range map[Channel]string{
		ChannelSMS: want + " Repondre 1 si vous gerez.", ChannelApp: want, ChannelWhatsApp: want, ChannelCall: want,
	} {
		if got := e.text(in, ch); got != text {
			t.Errorf("%s: %q, want %q", ch, got, text)
		}
	}
	// Through the escalation too: a fire tells by the application and SMS at once.
	out := e.Handle(Raise{Now: t0, Incident: "f", Kind: KindFire, Urgency: UrgencyCritical, Devices: []string{"dev_back"}})
	if app := textOf(t, out, ChannelApp); strings.Contains(app, "Repondre") {
		t.Errorf("app: %q", app)
	}
	if sms := textOf(t, out, ChannelSMS); !strings.HasSuffix(sms, "Repondre 1 si vous gerez.") {
		t.Errorf("sms: %q", sms)
	}
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
				wantAck := s.Channel == ChannelSMS &&
					kind != KindMainsRestored && kind != KindDeviceOffline && kind != KindBatteryLow && kind != KindGSMDegraded
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
	sms := textOf(t, out, ChannelSMS)
	checkText(t, sms)
	if !strings.HasSuffix(sms, "Repondre 1 si vous gerez.") || !strings.Contains(sms, "...") {
		t.Fatalf("%q", sms)
	}
	e = newEsc(t, func(c *Config) { c.SiteName = strings.Repeat("Grande Boutique ", 8) })
	app := textOf(t, raise(e, t0, KindIntrusion, long...), ChannelApp)
	checkText(t, app)
	if strings.Contains(app, "Repondre") || !strings.HasSuffix(app, "...") {
		t.Fatalf("%q", app)
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

func TestTheSirenTextNamesTheSirenAndSaysTheAlarmIsSilent(t *testing.T) {
	for siren, name := range map[string]string{"dev_back": "Porte arriere", "dev_siren1": "dev_siren1"} {
		e := newEsc(t)
		out := raise(e, t0, KindSirenFailed, siren)
		if len(sends(out)) == 0 {
			t.Fatal("nothing sent")
		}
		for _, a := range out {
			s, ok := a.(Send)
			if !ok {
				continue
			}
			for _, part := range []string{"ALERTE", "sirene", name, "silencieuse"} {
				if !strings.Contains(s.Text, part) {
					t.Errorf("%q misses %q", s.Text, part)
				}
			}
			if s.Channel == ChannelSMS && !strings.Contains(s.Text, "Repondre 1") {
				t.Errorf("%q: an SMS says how to answer", s.Text)
			}
			checkText(t, s.Text)
		}
	}
}

// Once the alarm is handled, the siren's alert says what happened and that the alarm
// is handled (or was stopped), and asks for nothing: no "Repondre 1", even by SMS.
// That sentence is kept whole when the text is cut.
func TestTheQuietSirenTextSaysTheAlarmIsHandled(t *testing.T) {
	for _, long := range []bool{false, true} {
		for ending, handle := range map[string]Input{
			"L'alarme est deja prise en charge.": Ack{Now: t0, Incident: "x", Contact: "owner"},
			"L'alarme a ete arretee.":            Close{Now: t0, Incident: "x"},
		} {
			e := newEsc(t, func(c *Config) {
				if long {
					c.SiteName = strings.Repeat("Grande Boutique ", 8)
				}
			})
			e.Handle(handle)
			out := e.Handle(Raise{Now: t0, Incident: "y", Kind: KindSirenFailed, Urgency: UrgencyHigh, Devices: []string{"dev_siren1"}, Alarm: "x"})
			out = append(out, tick(e, t0.Add(time.Hour))...)
			for _, ch := range []Channel{ChannelApp, ChannelSMS} {
				text := textOf(t, out, ch)
				checkText(t, text)
				if !strings.HasSuffix(text, " "+ending) || strings.Contains(text, "Repondre") || strings.Contains(text, "ALERTE") {
					t.Errorf("%s: %q", ch, text)
				}
				if !long && !strings.Contains(text, "la sirene (dev_siren1) n'a pas sonne a 21:04.") {
					t.Errorf("%s: %q does not name the siren", ch, text)
				}
			}
		}
	}
}

// A siren is named as the installer named it, or by its default name. A name that
// already says "sirene" is the subject itself; another goes between brackets.
func TestTheSirenTextReadsWellWhateverItsName(t *testing.T) {
	for name, want := range map[string]string{
		"Sir\u00e8ne":            "Boutique Lome: la sirene ne repond pas a 21:04",
		"Sir\u00e8ne 2":          "Boutique Lome: la sirene 2 ne repond pas a 21:04",
		"SIRENE ext\u00e9rieure": "Boutique Lome: la sirene exterieure ne repond pas a 21:04",
		"R\u00e9serve":           "Boutique Lome: la sirene (Reserve) ne repond pas a 21:04",
		"Sir\u00e8nes du fond":   "Boutique Lome: la sirene (Sirenes du fond) ne repond pas a 21:04",
	} {
		e := newEsc(t, func(c *Config) { c.DeviceNames["dev_siren1"] = name })
		if text := textOf(t, raise(e, t0, KindSirenFailed, "dev_siren1"), ChannelApp); !strings.Contains(text, want) {
			t.Errorf("%s: %q, want %q", name, text, want)
		}
	}
	e := newEsc(t, func(c *Config) { c.DeviceNames["dev_siren1"] = "Sir\u00e8ne 2" })
	e.Handle(Ack{Now: t0, Incident: "x", Contact: "owner"})
	out := e.Handle(Raise{Now: t0, Incident: "y", Kind: KindSirenFailed, Urgency: UrgencyHigh, Devices: []string{"dev_siren1"}, Alarm: "x"})
	if text := textOf(t, out, ChannelApp); !strings.Contains(text, "Boutique Lome: la sirene 2 n'a pas sonne a 21:04.") {
		t.Errorf("quiet: %q", text)
	}
}

// fireWith raises a fire while the sirens are known silent, and returns its texts by
// the application and by SMS.
func fireWith(t *testing.T, e *Escalation, silent ...string) (app, sms string) {
	t.Helper()
	out := e.Handle(Raise{Now: t0, Incident: "f", Kind: KindFire, Urgency: UrgencyCritical, Devices: []string{"dev_back"}, Silent: silent})
	app, sms = textOf(t, out, ChannelApp), textOf(t, out, ChannelSMS)
	checkText(t, app)
	checkText(t, sms)
	return app, sms
}

// A fire raised while a siren is known silent says so: the people on the site are not
// warned by it. The sentence comes before the way to answer, and stays whole when the
// text is cut; names too long to fit are left out.
func TestAFireSaysTheSirensKnownSilent(t *testing.T) {
	names := func(c *Config) {
		c.DeviceNames["dev_siren1"], c.DeviceNames["dev_siren2"] = "Sir\u00e8ne 1", "R\u00e9serve"
		c.DeviceNames["dev_long1"], c.DeviceNames["dev_long2"] = strings.Repeat("Grande sirene ", 2), strings.Repeat("Petite sirene ", 2)
	}
	const fire = "ALARME INCENDIE Boutique Lome: fumee detectee (Porte arriere) a 21:04."
	for _, c := range []struct {
		silent []string
		want   string
	}{
		{nil, ""},
		{[]string{"dev_siren1"}, " La sirene 1 ne repond pas."},
		{[]string{"dev_siren2"}, " La sirene (Reserve) ne repond pas."},
		{[]string{"dev_siren1", "dev_siren2"}, " Les sirenes (Sirene 1, Reserve) ne repondent pas."},
		{[]string{"dev_long1", "dev_long2"}, " Les sirenes ne repondent pas."},
	} {
		app, sms := fireWith(t, newEsc(t, names), c.silent...)
		if app != fire+c.want || sms != fire+c.want+ackHint {
			t.Errorf("%v:\n%q\n%q", c.silent, app, sms)
		}
	}
	long := func(c *Config) { names(c); c.SiteName = strings.Repeat("Grande Boutique ", 8) }
	if _, sms := fireWith(t, newEsc(t, long), "dev_siren2"); !strings.HasSuffix(sms, "... La sirene (Reserve) ne repond pas."+ackHint) {
		t.Errorf("cut: %q", sms)
	}
}

// The reminders of a fire still say it, after a restart too: the silent sirens are
// saved with the incident.
func TestAFiresRemindersStillSayTheSilentSirens(t *testing.T) {
	a := newEsc(t)
	a.Handle(Raise{Now: t0, Incident: "f", Kind: KindFire, Urgency: UrgencyCritical, Devices: []string{"dev_back"}, Silent: []string{"dev_siren1"}})
	raw, err := json.Marshal(a.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatal(err)
	}
	b := newEsc(t)
	b.Restore(snap)
	if text := textOf(t, tick(b, t0.Add(10*time.Minute)), ChannelApp); !strings.HasSuffix(text, " La sirene (dev_siren1) ne repond pas.") {
		t.Fatalf("a reminder after a restart: %q", text)
	}
}
