package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"safehome/internal/consoleapp"
	"safehome/internal/contract"
	"safehome/internal/install"
)

const host = "10.42.0.1"

type fakeConsole struct {
	o     consoleapp.Overview
	err   error
	calls []string
}

func (f *fakeConsole) Overview() consoleapp.Overview { return f.o }
func (f *fakeConsole) Pair() error                   { f.calls = append(f.calls, "pair"); return f.err }
func (f *fakeConsole) SetZone(id, name, kind string) error {
	f.calls = append(f.calls, fmt.Sprintf("zone %s %q %s", id, name, kind))
	return f.err
}
func (f *fakeConsole) Unzone(id string) error { f.calls = append(f.calls, "unzone "+id); return f.err }
func (f *fakeConsole) Siren(id string, on bool) error {
	f.calls = append(f.calls, fmt.Sprintf("siren %s %v", id, on))
	return f.err
}
func (f *fakeConsole) Remove(_ context.Context, id string) error {
	f.calls = append(f.calls, "remove "+id)
	return f.err
}
func (f *fakeConsole) SetCode(user, code, again string) error {
	f.calls = append(f.calls, fmt.Sprintf("code %s %s %s", user, code, again))
	return f.err
}
func (f *fakeConsole) Finish(context.Context) error {
	f.calls = append(f.calls, "finish")
	return f.err
}

var until = time.Date(2026, 10, 20, 14, 30, 0, 0, time.UTC)

func overview() consoleapp.Overview {
	return consoleapp.Overview{
		Open: true, Until: until, Known: true,
		Status: &contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 2, Offline: []string{}},
		Devices: []consoleapp.Device{
			{ID: "dev_door", Model: "aqara.contact", Capabilities: []string{"contact", "battery"}, Paired: true,
				Zone: &install.Zone{Name: "Porte <A>", Kind: "entry"}, Kinds: []string{"entry", "instant", "interior"}},
			{ID: "dev_siren", Model: "heiman.siren", Capabilities: []string{"siren"}, Paired: true, Siren: true, CanSound: true},
		},
		Findings: []install.Finding{{Level: install.Warning, Text: "no zone: nothing is watched"}},
		Events:   []consoleapp.Event{{At: until.Add(-time.Minute), DeviceID: "dev_door", Zone: "Porte <A>", Capability: "contact", Value: "open"}},
	}
}

func newHandler(t *testing.T, c Console) *Handler {
	t.Helper()
	h, err := New(c, host, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func post(h http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestThePageShowsTheDevicesInFrench(t *testing.T) {
	h := newHandler(t, &fakeConsole{o: overview()})
	w := get(h, "/")
	body := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		"Mode installation ouvert jusqu'\u00e0 14:30", "Pont en ligne, 2 appareil(s)", "dev_door",
		"Zone \u00ab Porte &lt;A&gt; \u00bb \u2014 Entr\u00e9e (avec d\u00e9lai)", `<option value="entry" selected>`,
		"Sir\u00e8ne", "Ne plus servir de sir\u00e8ne", "Retirer du r\u00e9seau", "Appairer de nouveaux appareils",
		"no zone: nothing is watched", "Terminer l'installation",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "<A>") {
		t.Error("a zone name is not escaped")
	}
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'", "X-Frame-Options": "DENY", "Cache-Control": "no-store",
	} {
		if !strings.Contains(w.Header().Get(k), want) {
			t.Errorf("header %s = %q", k, w.Header().Get(k))
		}
	}
	if strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Error("the page refreshes while nothing pairs: typing a zone name would be lost")
	}
}

func TestThePageRefreshesWhilePairing(t *testing.T) {
	o := overview()
	o.Pairing = &consoleapp.Pairing{Active: true, Until: until,
		Added:    []contract.RegistryDeviceAddedData{{DeviceID: "dev_k7p2", Model: "aqara.motion", Capabilities: []string{"motion"}}},
		Rejected: []contract.RegistryDeviceRejectedData{{Model: "TS0203", Reason: contract.RejectUnknownModel}}}
	o.Busy = true
	body := get(newHandler(t, &fakeConsole{o: o}), "/").Body.String()
	for _, want := range []string{`content="3"`, "Appairage ouvert jusqu'\u00e0 14:30:00", "dev_k7p2", "TS0203 : mod\u00e8le inconnu"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `action="/pair"`) {
		t.Error("a second pairing can be asked during one")
	}
}

func TestTheTestsPage(t *testing.T) {
	body := get(newHandler(t, &fakeConsole{o: overview()}), "/tests").Body.String()
	for _, want := range []string{`content="2"`, "14:29:00", "Porte &lt;A&gt;", "contact", "open"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestOrdersAndTheirOutcome(t *testing.T) {
	c := &fakeConsole{o: overview()}
	h := newHandler(t, c)
	w := post(h, "/zone", url.Values{"id": {"dev_door"}, "name": {"  Porte  "}, "kind": {"instant"}})
	if w.Code != http.StatusSeeOther || c.calls[0] != `zone dev_door "Porte" instant` {
		t.Fatalf("%d %v", w.Code, c.calls)
	}
	loc := w.Header().Get("Location")
	if body := get(h, loc).Body.String(); !strings.Contains(body, "dev_door : zone enregistr\u00e9e.") {
		t.Fatalf("after %s: no confirmation", loc)
	}
	post(h, "/siren", url.Values{"id": {"dev_siren"}, "on": {"1"}})
	post(h, "/unzone", url.Values{"id": {"dev_door"}})
	post(h, "/remove", url.Values{"id": {"dev_door"}})
	post(h, "/pair", nil)
	post(h, "/finish", nil)
	want := []string{`zone dev_door "Porte" instant`, "siren dev_siren true", "unzone dev_door", "remove dev_door", "pair", "finish"}
	if strings.Join(c.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", c.calls)
	}

	for err, want := range map[error]string{
		consoleapp.ErrBusy:                         "Un appairage est en cours",
		consoleapp.ErrClosed:                       "retapez le code installateur",
		errors.New("hub: at least one siren"):      "garder au moins une sir\u00e8ne",
		errors.New("something odd <script>"):       "Refus\u00e9 : something odd &lt;script&gt;",
		fmt.Errorf("x: %w", consoleapp.ErrUnknown): "dev_door n'est pas appair\u00e9",
	} {
		c.err = err
		loc := post(h, "/zone", url.Values{"id": {"dev_door"}, "name": {"X"}, "kind": {"entry"}}).Header().Get("Location")
		if body := get(h, loc).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%v: missing %q", err, want)
		}
	}
}

func TestOnlyTheHubsAddressAndKnownRoutes(t *testing.T) {
	h := newHandler(t, &fakeConsole{o: overview()})
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("another host: %d", w.Code)
	}
	if w := get(h, "/nothing"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown page: %d", w.Code)
	}
	if w := get(h, "/finish"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("an order by GET: %d", w.Code)
	}
}

func TestTheCodesSection(t *testing.T) {
	o := overview()
	o.People = []consoleapp.Person{{ID: "u_owner", Name: "Kofi", Role: "owner", HasCode: false}, {ID: "u_mgr", Name: "Ama", Role: "manager", HasCode: true}}
	c := &fakeConsole{o: o}
	h := newHandler(t, c)
	body := get(h, "/").Body.String()
	for _, want := range []string{
		"Codes d'acc\u00e8s", "Kofi", "propri\u00e9taire", "pas de code", "Donner un code",
		"Ama", "g\u00e9rant", "a un code", "Changer le code", `type="password" name="code" inputmode="numeric"`, `name="again"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}

	w := post(h, "/code", url.Values{"id": {"u_owner"}, "code": {"2580"}, "again": {"2580"}})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || c.calls[0] != "code u_owner 2580 2580" {
		t.Fatalf("%d %v", w.Code, c.calls)
	}
	if strings.Contains(loc, "2580") {
		t.Fatalf("the code is in the address: %s", loc)
	}
	if body := get(h, loc).Body.String(); !strings.Contains(body, "Code enregistr\u00e9 (u_owner)") || strings.Contains(body, "2580") {
		t.Fatalf("after %s: %s", loc, body)
	}

	for err, want := range map[error]string{
		consoleapp.ErrCodeMismatch:                       "Les deux codes tap\u00e9s diff\u00e8rent",
		consoleapp.ErrCodeInvalid:                        "4 \u00e0 8 chiffres",
		consoleapp.ErrCodeWeak:                           "trop facile \u00e0 deviner",
		consoleapp.ErrCodeTaken:                          "d\u00e9j\u00e0 pris",
		consoleapp.ErrCodeFull:                           "nombre maximal de codes",
		fmt.Errorf("%w: u_x", consoleapp.ErrNotAContact): "u_owner n'est pas un contact",
	} {
		c.err = err
		loc := post(h, "/code", url.Values{"id": {"u_owner"}, "code": {"1357"}, "again": {"1357"}}).Header().Get("Location")
		if body := get(h, loc).Body.String(); !strings.Contains(body, want) || strings.Contains(loc+body, "1357") {
			t.Errorf("%v: missing %q, or the code shows", err, want)
		}
	}
}
