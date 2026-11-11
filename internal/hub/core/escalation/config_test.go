package escalation

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

// --- configuration -----------------------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	if _, err := New(DefaultConfig("Shop", contacts())); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Config){
		"no site name": func(c *Config) { c.SiteName = "" },
		"six contacts": func(c *Config) {
			c.Contacts = append(c.Contacts, Contact{ID: "x", Role: RoleOther, Channels: []Channel{ChannelSMS}})
		},
		"no id":             func(c *Config) { c.Contacts[1].ID = "" },
		"duplicate id":      func(c *Config) { c.Contacts[1].ID = "owner" },
		"unknown role":      func(c *Config) { c.Contacts[1].Role = "king" },
		"no channel":        func(c *Config) { c.Contacts[1].Channels = nil },
		"unknown channel":   func(c *Config) { c.Contacts[1].Channels = []Channel{"pigeon"} },
		"channel twice":     func(c *Config) { c.Contacts[1].Channels = []Channel{ChannelSMS, ChannelSMS} },
		"no owner":          func(c *Config) { c.Contacts = c.Contacts[1:] },
		"no confirm delay":  func(c *Config) { c.InternetConfirm = 0 },
		"no gsm delay":      func(c *Config) { c.GSMDelay = 0 },
		"no hourly limit":   func(c *Config) { c.MaxPerHour = 0 },
		"unknown kind":      func(c *Config) { c.Plans["meteor"] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}} },
		"plan without wave": func(c *Config) { c.Plans[KindFire] = Plan{} },
		"waves out of order": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{After: time.Minute, To: []Audience{AudienceOwner}}, {To: []Audience{AudienceOwner}}}}
		},
		"wave tells nobody": func(c *Config) { c.Plans[KindFire] = Plan{Waves: []Wave{{}}} },
		"unknown audience":  func(c *Config) { c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{"all the world"}}}} },
		"reminders no delay": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, Repeat: Repeat{Times: 2}}
		},
		"negative reminders": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, Repeat: Repeat{Times: -1, Every: time.Minute}}
		},
		"gsm after on normal": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, GSMAfter: time.Hour}
		},
		"negative gsm after": func(c *Config) {
			c.Plans[KindFire] = Plan{Waves: []Wave{{To: []Audience{AudienceOwner}}}, InternetOnly: true, GSMAfter: -1}
		},
	}
	for name, mutate := range bad {
		cfg := DefaultConfig("Shop", contacts())
		cfg.Contacts = slices.Clone(cfg.Contacts)
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No contact at all is allowed (the contacts come later).
	cfg := DefaultConfig("Shop", nil)
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestMissingPlansTakeTheDefault(t *testing.T) {
	e := newEsc(t, func(c *Config) { c.Plans = nil })
	if got := sends(raise(e, t0, KindIntrusion)); len(got) == 0 {
		t.Fatal("the default plan must apply")
	}
}

func TestTheHourlyLimitIsExactlyTheConfiguredMaximum(t *testing.T) {
	e := newEsc(t, func(c *Config) {
		c.MaxPerHour = 3
		c.Contacts = []Contact{{ID: "owner", Role: RoleOwner, Channels: []Channel{ChannelApp, ChannelSMS}}}
	})
	want(t, "message 1", sends(raise(e, t0, KindIntrusion)), "owner/app/1")
	want(t, "message 2", sends(tick(e, t0.Add(10*time.Second))), "owner/sms/1")
	want(t, "message 3", sends(tick(e, t0.Add(2*time.Minute))), "owner/app/2")
	out := tick(e, t0.Add(2*time.Minute+10*time.Second))
	if len(sends(out)) != 0 {
		t.Fatalf("a fourth message in the hour must be held back: %v", sends(out))
	}
	throttled := false
	for _, a := range out {
		_, ok := a.(Throttled)
		throttled = throttled || ok
	}
	if !throttled {
		t.Fatal("the held back message must be reported")
	}
}

// Who an intrusion tells and when, by the default plan: the deputy and the guard at
// once, the owner two minutes later, the manager and the others at five; a role a
// plan never names comes last, never told. A plan changed in the site's files is the
// one said: the hub's own, not a copy.
func TestToldSaysWhoIsToldFirstAndWhen(t *testing.T) {
	e := newEsc(t)
	var got []string
	for _, w := range e.Told(KindIntrusion) {
		got = append(got, fmt.Sprintf("%s@%s", w.Contact.ID, w.After))
	}
	want := []string{"deputy@0s", "guard@0s", "owner@2m0s", "manager@5m0s", "other@5m0s"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	only := newEsc(t, func(c *Config) {
		c.Plans = map[AlertKind]Plan{KindIntrusion: {Waves: []Wave{{To: []Audience{AudienceOwner}}}}}
	})
	told := only.Told(KindIntrusion)
	if told[0].Contact.ID != "owner" || told[0].Never || !told[len(told)-1].Never {
		t.Fatalf("the site's own plan: %+v", told)
	}
}
