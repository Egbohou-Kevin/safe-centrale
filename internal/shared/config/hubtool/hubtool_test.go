package hubtool

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"safehome/internal/shared/core/access"
	sitemodel "safehome/internal/shared/core/site"
)

func TestASiteGoesAndComesBackWhole(t *testing.T) {
	s := sitemodel.Site{
		Zones:      map[string]sitemodel.Zone{"dev_door": {Name: "Entree", Kind: "entry", Room: "Hall"}},
		SirenIDs:   []string{"dev_siren1"},
		SirenNames: map[string]string{"dev_siren1": "Salon"},
	}
	if got := SiteOf(s).Model(); !reflect.DeepEqual(got, s) {
		t.Fatalf("%+v became %+v", s, got)
	}
	if ids := SiteOf(sitemodel.Site{}).SirenIDs; ids == nil {
		t.Fatal("no siren must say [] to the hub, not null")
	}
}

func TestTheAccessRuleKeepsItsNumbers(t *testing.T) {
	c := access.Config{MaxUsers: 8, MinDigits: 4, MaxDigits: 8, Iterations: 1000}
	if got := AccessOf(c); got != (Access{8, 4, 8, 1000}) {
		t.Fatalf("%+v", got)
	}
}

func TestLocation(t *testing.T) {
	if loc, err := (Config{}).Location(); loc != nil || err != nil {
		t.Fatalf("no zone is UTC, nil: %v %v", loc, err)
	}
	if loc, err := (Config{Timezone: "Africa/Lome"}).Location(); err != nil || loc.String() != "Africa/Lome" {
		t.Fatalf("%v %v", loc, err)
	}
	if _, err := (Config{Timezone: "Mars/Olympus"}).Location(); err == nil {
		t.Fatal("an unknown zone was taken")
	}
}

// answering is a hub that answers verb with out, and keeps what it was sent.
func answering(verb, out string, sent *[]byte) Hub {
	return Hub{Call: func(command, v string, in []byte) ([]byte, error) {
		if command != Command || v != verb {
			return nil, errors.New("asked " + command + " " + v)
		}
		*sent = in
		return []byte(out), nil
	}}
}

func TestTheHubIsAskedByItsVerbs(t *testing.T) {
	var sent []byte
	c, err := answering(View, `{"site":"tg-lom-0001","state_dir":"/var/lib/safehome","broker_url":"mqtt://127.0.0.1:1883","timezone":"","contacts":[],"access":{"max_users":8,"min_digits":4,"max_digits":8,"iterations":1}}`, &sent).View()
	if err != nil || c.Site != "tg-lom-0001" || c.Access.MaxUsers != 8 {
		t.Fatalf("view %+v %v", c, err)
	}
	s, err := answering(Read, `{"zones":{"dev_door":{"name":"Entree","kind":"entry"}},"siren_ids":[]}`, &sent).Read()
	if err != nil || s.Zones["dev_door"].Name != "Entree" {
		t.Fatalf("read %+v %v", s, err)
	}
	if err := answering(Check, "", &sent).Check(s); err != nil || !strings.Contains(string(sent), `"dev_door"`) {
		t.Fatalf("check sent %s: %v", sent, err)
	}
	if err := answering(Write, "", &sent).Write(s); err != nil || !strings.Contains(string(sent), `"siren_ids":[]`) {
		t.Fatalf("write sent %s: %v", sent, err)
	}
}

func TestAnAnswerIsReadStrictly(t *testing.T) {
	var sent []byte
	if _, err := answering(Read, `{"zones":{},"siren_ids":[],"extra":1}`, &sent).Read(); err == nil {
		t.Fatal("an unknown field was taken")
	}
	refused := Hub{Call: func(string, string, []byte) ([]byte, error) { return nil, errors.New("no") }}
	if err := refused.Write(sitemodel.Site{}); err == nil || err.Error() != "no" {
		t.Fatalf("refusal: %v", err)
	}
}
