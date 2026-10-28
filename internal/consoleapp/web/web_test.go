package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
func (f *fakeConsole) NameSiren(id, name string) error {
	f.calls = append(f.calls, fmt.Sprintf("siren name %s %q", id, name))
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
func (f *fakeConsole) RemoveCode(user string) error {
	f.calls = append(f.calls, "uncode "+user)
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
			{ID: "dev_siren", Model: "heiman.siren", Capabilities: []string{"siren"}, Paired: true, Siren: true, CanSound: true,
				SirenName: "Sir\u00e8ne"},
		},
		Findings: []install.Finding{{Level: install.Warning, Text: "no zone: nothing is watched"}},
		Events:   []consoleapp.Event{{At: until.Add(-time.Minute), DeviceID: "dev_door", Zone: "Porte <A>", Capability: "contact", Value: "open"}},
	}
}

func newHandler(t *testing.T, c Console) *Handler {
	t.Helper()
	h, err := New(c, host, netip.MustParsePrefix("10.42.0.0/24"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// phone is a phone on the hub's own network.
const phone = "10.42.0.23:51234"

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r.Host, r.RemoteAddr = host, phone
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func post(h http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Host, r.RemoteAddr = host, phone
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
		"Sir\u00e8ne \u00ab Sir\u00e8ne \u00bb <span class=\"muted\">(nom par d\u00e9faut)</span>", `action="/siren-name"`,
		"Nommer la sir\u00e8ne", "Ne plus servir de sir\u00e8ne", "Retirer du r\u00e9seau", "Appairer de nouveaux appareils",
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

// A siren with a zone (a smoke detector that sounds) has its zone's name in the
// alerts: the page says so, and offers no name of its own.
func TestASirenWithAZoneShowsNoSirenName(t *testing.T) {
	o := overview()
	o.Devices = []consoleapp.Device{{ID: "dev_smoke", Model: "smoke.siren", Capabilities: []string{"smoke", "siren"}, Paired: true,
		Zone: &install.Zone{Name: "Cuisine", Kind: "24h_fire"}, Siren: true, CanSound: true, SirenName: "Cuisine", Kinds: []string{"24h_fire"}}}
	body := get(newHandler(t, &fakeConsole{o: o}), "/").Body.String()
	if !strings.Contains(body, "Sir\u00e8ne <span class=\"muted\">(nomm\u00e9e comme sa zone dans les alertes)</span>") ||
		strings.Contains(body, `action="/siren-name"`) || strings.Contains(body, "nom par d\u00e9faut") {
		t.Fatalf("%s", body)
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
	loc = post(h, "/siren-name", url.Values{"id": {"dev_siren"}, "name": {" R\u00e9serve "}}).Header().Get("Location")
	if body := get(h, loc).Body.String(); !strings.Contains(body, "dev_siren : nom de sir\u00e8ne enregistr\u00e9.") {
		t.Fatalf("after %s: no confirmation", loc)
	}
	post(h, "/unzone", url.Values{"id": {"dev_door"}})
	post(h, "/remove", url.Values{"id": {"dev_door"}})
	post(h, "/pair", nil)
	post(h, "/finish", nil)
	want := []string{`zone dev_door "Porte" instant`, "siren dev_siren true", "siren name dev_siren \"R\u00e9serve\"",
		"unzone dev_door", "remove dev_door", "pair", "finish"}
	if strings.Join(c.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", c.calls)
	}

	for err, want := range map[error]string{
		consoleapp.ErrBusy:      "Un appairage est en cours",
		consoleapp.ErrClosed:    "retapez le code installateur",
		consoleapp.ErrLastSiren: "garder au moins une sir\u00e8ne",
		fmt.Errorf("zone name %q is %w", "x", consoleapp.ErrNameTooLong): "Nom trop long",
		fmt.Errorf("%w: a fire zone...", consoleapp.ErrCannotServe):      "ne peut pas servir",
		consoleapp.ErrEmptyName:                          "Donnez un nom",
		consoleapp.ErrNoAnswer:                           "n'a pas r\u00e9pondu",
		errors.New("hub: at least one siren"):            "Refus\u00e9 : hub: at least one siren",
		errors.New("something odd <script>"):             "Refus\u00e9 : something odd &lt;script&gt;",
		fmt.Errorf("x: %w", consoleapp.ErrUnknown):       "dev_door n'est pas appair\u00e9",
		fmt.Errorf("%w: dev_door", consoleapp.ErrNoZone): "dev_door n'a pas de zone",
		fmt.Errorf("dev_door %w", consoleapp.ErrNoSiren): "dev_door ne sert pas de sir\u00e8ne",
		fmt.Errorf("dev_door %w", consoleapp.ErrHasZone): "dev_door a une zone : les alertes l'appellent par le nom de sa zone.",
	} {
		c.err = err
		loc := post(h, "/zone", url.Values{"id": {"dev_door"}, "name": {"X"}, "kind": {"entry"}}).Header().Get("Location")
		if body := get(h, loc).Body.String(); !strings.Contains(body, want) {
			t.Errorf("%v: missing %q", err, want)
		}
	}
}

// A link from another page (the installer's phone browses the internet too) cannot have
// the console say a text of its own: what an outcome says comes from the hub alone.
func TestALinkCannotPutItsOwnTextOnThePage(t *testing.T) {
	h := newHandler(t, &fakeConsole{o: overview()})
	fake := "Appelez le 0800 000 000"
	for _, target := range []string{
		"/?err=other&detail=" + url.QueryEscape(fake),
		"/?err=other&ref=x&detail=" + url.QueryEscape(fake),
		"/?err=" + url.QueryEscape(fake),
		"/?ok=" + url.QueryEscape(fake),
		"/?err=unknown&id=" + url.QueryEscape(fake),
	} {
		body := get(h, target).Body.String()
		if strings.Contains(body, "0800") {
			t.Errorf("%s: the link's text is on the page", target)
		}
		if strings.Contains(body, "C'est fait") {
			t.Errorf("%s: an outcome no order gave", target)
		}
	}
	c := &fakeConsole{o: overview(), err: errors.New("hub: odd")}
	h = newHandler(t, c)
	loc := post(h, "/pair", nil).Header().Get("Location")
	if strings.Contains(loc, "odd") {
		t.Fatalf("the refusal's text is in the address: %s", loc)
	}
	if body := get(h, loc).Body.String(); !strings.Contains(body, "Refus\u00e9 : hub: odd") {
		t.Fatalf("the hub's own refusal is said: %s", loc)
	}
}

// A removal sent without an answer from the adapter still takes the device out of the
// site: the page says so, not only that the adapter did not answer. The device may
// still be paired and working, no longer watched.
func TestAnUnconfirmedRemovalSaysTheDeviceLeftTheSite(t *testing.T) {
	c := &fakeConsole{o: overview(), err: fmt.Errorf("%w: %w", install.ErrUnconfirmed, consoleapp.ErrNoAnswer)}
	h := newHandler(t, c)
	loc := post(h, "/remove", url.Values{"id": {"dev_door"}}).Header().Get("Location")
	if loc != "/?err=unconfirmed&id=dev_door" {
		t.Fatalf("redirect %s", loc)
	}
	body := get(h, loc).Body.String()
	if !strings.Contains(body, "dev_door quitte la configuration quand m\u00eame") || strings.Contains(body, "n'a pas r\u00e9pondu") {
		t.Fatalf("%s", body)
	}
}

// The zones file is refused: the hub watches other zones than the installer's, and
// the page does not say that all is in order.
func TestAnUnreadableZonesFileIsNotInOrder(t *testing.T) {
	o := overview()
	o.Findings, o.SiteErr = nil, "site-devices.json: unexpected end of JSON input"
	body := get(newHandler(t, &fakeConsole{o: o}), "/").Body.String()
	if !strings.Contains(body, "La configuration des zones est illisible") || strings.Contains(body, "Tout est en ordre") {
		t.Fatalf("%s", body)
	}
	o.SiteErr = ""
	if body := get(newHandler(t, &fakeConsole{o: o}), "/").Body.String(); !strings.Contains(body, "Tout est en ordre") {
		t.Fatalf("%s", body)
	}
}

func TestOnlyTheHubsAddressAndKnownRoutes(t *testing.T) {
	h := newHandler(t, &fakeConsole{o: overview()})
	r := httptest.NewRequest("GET", "/", nil)
	r.Host, r.RemoteAddr = "evil.example", phone
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

func TestTheCodesOfFormerContacts(t *testing.T) {
	c := &fakeConsole{o: overview()}
	h := newHandler(t, c)
	if body := get(h, "/").Body.String(); strings.Contains(body, "anciens contacts") {
		t.Fatal("a section without leftovers")
	}
	c.o.Leftovers = []string{"u_gone"}
	body := get(h, "/").Body.String()
	for _, want := range []string{"Codes d'anciens contacts", "u_gone", `action="/uncode"`, "Retirer ce code"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	loc := post(h, "/uncode", url.Values{"id": {"u_gone"}}).Header().Get("Location")
	if c.calls[0] != "uncode u_gone" || !strings.Contains(get(h, loc).Body.String(), "Code retir\u00e9 (u_gone)") {
		t.Fatalf("%v %s", c.calls, loc)
	}
	c.err = fmt.Errorf("%w: u_owner", consoleapp.ErrNoSuchCode)
	loc = post(h, "/uncode", url.Values{"id": {"u_owner"}}).Header().Get("Location")
	if body := get(h, loc).Body.String(); !strings.Contains(body, "u_owner n'a pas de code \u00e0 retirer") {
		t.Fatalf("after %s: %s", loc, body)
	}
}

// The installer's phone may browse the internet over its mobile data while on the
// hub's network: a page from elsewhere must not be able to post orders.
// A device on the shop's network may route the hub's address on its own network
// through the hub, and Linux delivers it whatever the interface: the page answers
// only the phones on the hub's network, which the Wi-Fi passphrase lets in.
func TestOnlyThePhonesOnTheHubsNetworkAreAnswered(t *testing.T) {
	c := &fakeConsole{o: overview()}
	h := newHandler(t, c)
	for _, from := range []string{"192.168.1.50:40000", "10.42.1.5:40000", "[::ffff:192.168.1.50]:40000", "nonsense"} {
		r := httptest.NewRequest("POST", "/finish", nil)
		r.Host, r.RemoteAddr = host, from
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || len(c.calls) != 0 {
			t.Fatalf("from %s: %d, %v", from, w.Code, c.calls)
		}
	}
	if w := get(h, "/"); w.Code != http.StatusOK {
		t.Fatalf("a phone on the hub's network: %d", w.Code)
	}
	if _, err := New(c, host, netip.Prefix{}, time.UTC); err == nil {
		t.Fatal("the network is required")
	}
}

func TestOrdersFromAnotherSiteAreRefused(t *testing.T) {
	c := &fakeConsole{o: overview()}
	h := newHandler(t, c)
	order := func(headers map[string]string) int {
		r := httptest.NewRequest("POST", "/finish", nil)
		r.Host, r.RemoteAddr = host, phone
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, headers := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "http://evil.example"},
	} {
		if code := order(headers); code != http.StatusForbidden {
			t.Errorf("%v: %d", headers, code)
		}
	}
	if len(c.calls) != 0 {
		t.Fatalf("a cross-site order went through: %v", c.calls)
	}
	if code := order(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://" + host}); code != http.StatusSeeOther {
		t.Fatalf("the page's own order: %d", code)
	}
}
