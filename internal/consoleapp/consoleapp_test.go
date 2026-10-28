package consoleapp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/install"
)

func TestTheConsoleLivesOnlyWhileTheInstallationModeLasts(t *testing.T) {
	r := newRig(t)
	if r.isServing() || r.app.Overview().Open {
		t.Fatal("the console runs before the installation mode")
	}
	if err := r.app.Pair(); !errors.Is(err, ErrClosed) {
		t.Fatalf("pair while closed: %v", err)
	}
	r.open()
	if up, _ := r.hotspot.counts(); up != 1 || !r.app.Overview().Open {
		t.Fatal("the network is not up")
	}
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallStopped})
	drivertest.WaitFor(t, "the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
	if r.isServing() || r.app.Overview().Open {
		t.Fatal("the page outlives the mode")
	}
	if err := r.app.SetZone("dev_motion", "Salle", "interior"); !errors.Is(err, ErrClosed) {
		t.Fatalf("a change while closed: %v", err)
	}
	if err := r.app.NameSiren("dev_siren", "Reserve"); !errors.Is(err, ErrClosed) {
		t.Fatalf("a siren named while closed: %v", err)
	}
}

func TestTheConsoleStopsOnItsOwnWhenTheTimeIsUp(t *testing.T) {
	r := newRig(t)
	r.open()
	r.clock.Advance(30 * time.Minute)
	drivertest.WaitFor(t, "the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
	if r.isServing() {
		t.Fatal("the page outlives the mode, though the hub said nothing")
	}
	// A retained message from before is no new mode.
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(10 * time.Minute))})
	if err := r.app.Alive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if up, _ := r.hotspot.counts(); up != 1 {
		t.Fatal("an old open mode started the network again")
	}
}

func TestANetworkThatDoesNotStartIsSaid(t *testing.T) {
	r := newRig(t)
	r.hotspot.fail = errors.New("no Wi-Fi device")
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(30 * time.Minute))})
	drivertest.WaitFor(t, "the failure", func() bool { return r.app.Overview().Hotspot != "" })
	if r.isServing() {
		t.Fatal("a page without network")
	}
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallTimeout})
	drivertest.WaitFor(t, "the end", func() bool { return !r.app.Overview().Open })
	// A start that failed (cut short by its deadline, say) may still have left a
	// network that NetworkManager brings up: it is taken down at the end of the mode.
	drivertest.WaitFor(t, "the network down", func() bool { _, down := r.hotspot.counts(); return down == 1 })
}

// The console died during a mode (a crash, the watchdog) and comes back once it is
// over: the network it left is taken down at its start, before any new mode's.
func TestANetworkLeftByAConsoleThatDiedIsTakenDown(t *testing.T) {
	hotspot := &fakeHotspot{}
	app, err := New(DefaultConfig(site), Deps{Clock: drivertest.NewFakeClock(t0), Publisher: &rig{}, NewID: contract.NewIDGenerator().New,
		Hotspot: hotspot, Sites: &fakeSites{}, Codes: &fakeCodes{codes: map[string]string{}}, Serve: func(ctx context.Context) error { <-ctx.Done(); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	drivertest.WaitFor(t, "the network taken down", func() bool { up, down := hotspot.counts(); return up == 0 && down == 1 })
}

// The network did not start (the Wi-Fi chip busy with the box's network): the
// installer, seeing no network, types the code again. The hub starts a new half
// hour, and the console tries the network once more.
func TestANetworkThatDidNotStartIsTriedAgainWhenTheCodeIsTypedAgain(t *testing.T) {
	r := newRig(t)
	r.hotspot.mu.Lock()
	r.hotspot.fail = errors.New("device busy")
	r.hotspot.mu.Unlock()
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(30 * time.Minute))})
	drivertest.WaitFor(t, "the failure", func() bool { return r.app.Overview().Hotspot != "" })
	r.hotspot.mu.Lock()
	r.hotspot.fail = nil
	r.hotspot.mu.Unlock()
	r.clock.Advance(5 * time.Minute)
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(r.clock.Now().Add(30 * time.Minute))})
	drivertest.WaitFor(t, "the page", func() bool { return r.isServing() })
	if o := r.app.Overview(); !o.Open || o.Hotspot != "" || !o.Until.Equal(t0.Add(35*time.Minute)) {
		t.Fatalf("%+v", o)
	}
	if up, down := r.hotspot.counts(); up != 1 || down != 1 {
		t.Fatalf("up %d, down %d: the failed start's network taken down first", up, down)
	}
}

func TestPairingShowsTheDevicesAsTheyJoin(t *testing.T) {
	r := newRig(t)
	r.open()
	r.mu.Lock()
	r.join = []contract.RegistryDevice{dev("dev_k7p2", "contact", "battery")}
	r.mu.Unlock()
	if err := r.app.Pair(); err != nil {
		t.Fatal(err)
	}
	drivertest.WaitFor(t, "the new device", func() bool {
		p := r.app.Overview().Pairing
		return p != nil && len(p.Added) == 1 && p.Added[0].DeviceID == "dev_k7p2"
	})
	drivertest.WaitFor(t, "the end of the window", func() bool { p := r.app.Overview().Pairing; return !p.Active && !r.app.Overview().Busy })
	o := r.app.Overview()
	if i := slices.IndexFunc(o.Devices, func(d Device) bool { return d.ID == "dev_k7p2" }); i < 0 || o.Devices[i].Zone != nil {
		t.Fatalf("devices %+v", o.Devices)
	}
	if !slices.ContainsFunc(o.Findings, func(f install.Finding) bool { return f.DeviceID == "dev_k7p2" }) {
		t.Fatalf("a device without use is not pointed out: %+v", o.Findings)
	}
}

// During a pairing window the page goes on: the device that joined can be given its
// zone at once, and a sensor triggered right away shows on the Tests page.
func TestThePageFollowsTheDevicesDuringAPairing(t *testing.T) {
	r := newRig(t)
	r.open()
	r.mu.Lock()
	r.keepOpen = true
	r.join = []contract.RegistryDevice{dev("dev_k7p2", "contact", "battery")}
	r.mu.Unlock()
	if err := r.app.Pair(); err != nil {
		t.Fatal(err)
	}
	drivertest.WaitFor(t, "the new device", func() bool { p := r.app.Overview().Pairing; return p != nil && p.Active && len(p.Added) == 1 })
	if !slices.ContainsFunc(r.app.Overview().Devices, func(d Device) bool { return d.ID == "dev_k7p2" }) {
		t.Fatalf("the device list without the new device: %+v", r.app.Overview().Devices)
	}
	if err := r.app.SetZone("dev_k7p2", "Reserve", "instant"); err != nil {
		t.Fatalf("a zone for the new device during the window: %v", err)
	}
	r.deliver(contract.DeviceEventTopic(site, "dev_k7p2"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_k7p2", Capability: "contact", Value: json.RawMessage(`"open"`)}, false)
	drivertest.WaitFor(t, "the event", func() bool { return len(r.app.Overview().Events) == 1 })
	if !r.app.Overview().Pairing.Active {
		t.Fatal("the window ended before the checks")
	}
}

func TestZonesAndSirensFromThePage(t *testing.T) {
	r := newRig(t)
	r.open()
	cases := []struct {
		name string
		err  func() error
		want error
	}{
		{"unknown device", func() error { return r.app.SetZone("dev_nope", "X", "entry") }, ErrUnknown},
		{"empty name", func() error { return r.app.SetZone("dev_motion", " ", "interior") }, ErrEmptyName},
		{"long name", func() error {
			return r.app.SetZone("dev_motion", strings.Repeat("a", install.MaxNameLength+1), "interior")
		}, ErrNameTooLong},
		{"wrong kind", func() error { return r.app.SetZone("dev_motion", "Salle", "24h_fire") }, ErrCannotServe},
		{"a contact as a siren", func() error { return r.app.Siren("dev_door", true) }, ErrNotASiren},
		{"the last siren", func() error { return r.app.Siren("dev_siren", false) }, ErrLastSiren},
		{"no zone to remove", func() error { return r.app.Unzone("dev_motion") }, ErrNoZone},
		{"a zone named as a siren", func() error { return r.app.NameSiren("dev_door", "Porte") }, ErrNoSiren},
		{"an empty siren name", func() error { return r.app.NameSiren("dev_siren", " ") }, ErrEmptyName},
	}
	for _, c := range cases {
		err := c.err()
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: got %v", c.name, err)
		}
	}
	// A room laid by safehome-install outlives an edit from the console.
	site, _ := r.sites.Read()
	site.Zones["dev_motion"] = install.Zone{Name: "Avant", Kind: "interior", Room: "Salon"}
	if err := r.sites.Write(site); err != nil {
		t.Fatal(err)
	}
	if err := r.app.SetZone("dev_motion", "Salle", "interior"); err != nil {
		t.Fatal(err)
	}
	if err := r.app.Unzone("dev_door"); err != nil {
		t.Fatal(err)
	}
	site, _ = r.sites.Read()
	if site.Zones["dev_motion"] != (install.Zone{Name: "Salle", Kind: "interior", Room: "Salon"}) || len(site.Zones) != 1 {
		t.Fatalf("zones %+v", site.Zones)
	}
	// The siren shows the name the alerts call it by: the default, then its own.
	sirenOf := func() Device {
		o := r.app.Overview()
		return o.Devices[slices.IndexFunc(o.Devices, func(d Device) bool { return d.ID == "dev_siren" })]
	}
	if d := sirenOf(); d.SirenName != "Sir\u00e8ne" || d.SirenNamed {
		t.Fatalf("an unnamed siren: %+v", d)
	}
	if err := r.app.NameSiren("dev_siren", "Reserve"); err != nil {
		t.Fatal(err)
	}
	if d := sirenOf(); d.SirenName != "Reserve" || !d.SirenNamed {
		t.Fatalf("a named siren: %+v", d)
	}
	// A siren with a zone has its zone's name, and takes no other.
	r.sites.Write(install.Site{Zones: map[string]install.Zone{"dev_siren": {Name: "Cuisine", Kind: "24h_fire"}}, SirenIDs: []string{"dev_siren"}})
	if d := sirenOf(); d.SirenName != "Cuisine" || d.SirenNamed {
		t.Fatalf("a siren with a zone: %+v", d)
	}
	if err := r.app.NameSiren("dev_siren", "Reserve"); !errors.Is(err, ErrHasZone) {
		t.Fatalf("a siren with a zone named: %v", err)
	}
	// The zoned device that is not paired shows, as a mistake to undo.
	r.sites.Write(install.Site{Zones: map[string]install.Zone{"dev_ghost": {Name: "G", Kind: "entry"}}, SirenIDs: []string{"dev_siren"}})
	o := r.app.Overview()
	if i := slices.IndexFunc(o.Devices, func(d Device) bool { return d.ID == "dev_ghost" }); i < 0 || o.Devices[i].Paired {
		t.Fatalf("devices %+v", o.Devices)
	}
}

func TestRemovingADevice(t *testing.T) {
	r := newRig(t)
	r.open()
	if err := r.app.Remove(context.Background(), "dev_siren"); !errors.Is(err, ErrLastSiren) {
		t.Fatalf("the last siren: %v", err)
	}
	if n := len(r.sentOn(contract.RegistryCmdTopic(site))); n != 0 {
		t.Fatalf("%d order(s) sent for a refused removal", n)
	}
	if err := r.app.Remove(context.Background(), "dev_door"); err != nil {
		t.Fatal(err)
	}
	site, _ := r.sites.Read()
	if _, ok := site.Zones["dev_door"]; ok {
		t.Fatal("the removed device keeps its zone")
	}
	drivertest.WaitFor(t, "the registry without it", func() bool {
		return !slices.ContainsFunc(r.app.Overview().Devices, func(d Device) bool { return d.ID == "dev_door" && d.Paired })
	})
}

// The order went out, the answer did not come: the adapter may carry it out a moment
// later, and the site must not keep a device the network lost.
func TestAnUnconfirmedRemovalStillLeavesTheSite(t *testing.T) {
	r := newRig(t)
	r.open()
	r.mu.Lock()
	r.mute = true
	r.mu.Unlock()
	if err := r.app.Remove(context.Background(), "dev_door"); !errors.Is(err, install.ErrUnconfirmed) {
		t.Fatalf("the page is told: %v", err)
	}
	if site, _ := r.sites.Read(); site.Zones["dev_door"] != (install.Zone{}) {
		t.Fatalf("the site keeps the device: %+v", site)
	}
}

func TestTheTestsPageShowsEventsWithZonesAndNoCode(t *testing.T) {
	r := newRig(t)
	r.deliver(contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"open"`)}, false)
	r.clock.Advance(time.Second)
	r.open()
	r.deliver(contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"closed"`)}, false)
	r.deliver(contract.DeviceEventTopic(site, "dev_keypad"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_keypad", Capability: "keypad", Value: json.RawMessage(`"disarm"`), Code: "123456"}, false)
	drivertest.WaitFor(t, "the events", func() bool { return len(r.app.Overview().Events) == 2 })
	ev := r.app.Overview().Events
	if ev[1].DeviceID != "dev_door" || ev[1].Zone != "Entree" || ev[1].Value != "closed" || ev[0].DeviceID != "dev_keypad" {
		t.Fatalf("events %+v: the latest first, only since the mode opened", ev)
	}
	if strings.Contains(ev[0].Value, "123456") {
		t.Fatal("the keypad's code shows")
	}
}

func TestFinishAsksTheHubToClose(t *testing.T) {
	r := newRig(t)
	r.open()
	if err := r.app.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := r.sentOn(contract.InstallCmdTopic(site))
	if len(sent) != 1 || sent[0].Type != contract.TypeInstallClose || sent[0].Src != Source {
		t.Fatalf("%+v", sent)
	}
}

func TestNewChecks(t *testing.T) {
	good := Deps{Clock: drivertest.NewFakeClock(t0), Publisher: &rig{}, NewID: contract.NewIDGenerator().New, Hotspot: &fakeHotspot{}, Sites: &fakeSites{},
		Codes: &fakeCodes{}, Serve: func(context.Context) error { return nil }}
	if _, err := New(DefaultConfig(site), good); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.Serve = nil
	if _, err := New(DefaultConfig(site), bad); err == nil {
		t.Error("no page accepted")
	}
	if _, err := New(DefaultConfig("X"), good); err == nil {
		t.Error("a bad site accepted")
	}
	cfg := DefaultConfig(site)
	cfg.PairFor = 0
	if _, err := New(cfg, good); err == nil {
		t.Error("no pairing time accepted")
	}
}

func TestCodesAreSetOnlyWhileTheModeIsOpenAndForAContact(t *testing.T) {
	r := newRig(t)
	if err := r.app.SetCode("u_owner", "2580", "2580"); !errors.Is(err, ErrClosed) {
		t.Fatalf("mode closed: %v", err)
	}
	r.open()
	if p := r.app.Overview().People; len(p) != 1 || p[0].HasCode {
		t.Fatalf("%+v", p)
	}
	for _, c := range []struct {
		user, code, again string
		want              error
	}{
		{"u_owner", "2580", "2581", ErrCodeMismatch},
		{"u_stranger", "2580", "2580", ErrNotAContact},
		{"u_owner", "1234", "1234", ErrCodeWeak},
	} {
		if err := r.app.SetCode(c.user, c.code, c.again); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v", c, err)
		}
	}
	if len(r.codes.codes) != 0 {
		t.Fatalf("a refused code was kept: %v", r.codes.codes)
	}
	if err := r.app.SetCode("u_owner", "2580", "2580"); err != nil {
		t.Fatal(err)
	}
	if p := r.app.Overview().People; !p[0].HasCode || r.codes.codes["u_owner"] != "2580" {
		t.Fatalf("%+v", p)
	}
}

// A code left by someone no longer a contact is shown apart, and removed from the
// page, only while the mode is open.
func TestTheCodeOfAFormerContactIsShownAndRemoved(t *testing.T) {
	r := newRig(t)
	r.codes.codes["u_gone"] = "7391"
	if err := r.app.RemoveCode("u_gone"); !errors.Is(err, ErrClosed) {
		t.Fatalf("mode closed: %v", err)
	}
	r.open()
	if o := r.app.Overview(); !slices.Equal(o.Leftovers, []string{"u_gone"}) || len(o.People) != 1 {
		t.Fatalf("%+v %+v", o.People, o.Leftovers)
	}
	r.codes.codes["u_owner"] = "2580"
	if err := r.app.RemoveCode("u_owner"); !errors.Is(err, ErrNoSuchCode) {
		t.Fatalf("a contact's code is not removed here: %v", err)
	}
	if err := r.app.RemoveCode("u_gone"); err != nil {
		t.Fatal(err)
	}
	if o := r.app.Overview(); len(o.Leftovers) != 0 || r.codes.codes["u_owner"] != "2580" {
		t.Fatalf("%+v %v", o.Leftovers, r.codes.codes)
	}
}

// A removal the adapter confirms after the mode ended still leaves the site: the
// site must not keep a device the network lost.
// A second tap on "remove", or another tab, while the removal is under way: refused,
// and the first removal keeps its hold: the other siren cannot be taken off, and the
// site never ends with the removed device as its only siren.
func TestASecondRemovalOfTheSameDeviceIsRefused(t *testing.T) {
	r := newRig(t)
	r.mu.Lock()
	r.devices = append(r.devices, dev("dev_siren2", "siren", "mains"))
	r.mu.Unlock()
	r.registry()
	r.open()
	if err := r.app.Siren("dev_siren2", true); err != nil {
		t.Fatal(err)
	}
	sent, release := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.hold = func(env contract.Envelope) {
		if env.Type == contract.TypeRegistryRemove {
			close(sent)
			<-release
		}
	}
	r.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- r.app.Remove(context.Background(), "dev_siren") }()
	<-sent
	if err := r.app.Remove(context.Background(), "dev_siren"); !errors.Is(err, ErrRemoving) {
		t.Fatalf("the same removal again: %v", err)
	}
	if err := r.app.Siren("dev_siren2", false); !errors.Is(err, ErrLastSiren) {
		t.Fatalf("the other siren taken off during the removal: %v", err)
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if site, _ := r.sites.Read(); !slices.Equal(site.SirenIDs, []string{"dev_siren2"}) {
		t.Fatalf("sirens %v", site.SirenIDs)
	}
}

func TestARemovalEndingAfterTheModeStillLeavesTheSite(t *testing.T) {
	r := newRig(t)
	r.open()
	sent, release := make(chan struct{}), make(chan struct{})
	r.mu.Lock()
	r.hold = func(env contract.Envelope) {
		if env.Type == contract.TypeRegistryRemove {
			close(sent)
			<-release
		}
	}
	r.mu.Unlock()
	result := make(chan error, 1)
	go func() { result <- r.app.Remove(context.Background(), "dev_door") }()
	<-sent
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallTimeout})
	drivertest.WaitFor(t, "the mode closed", func() bool { return !r.app.Overview().Open })
	close(release)
	if err := <-result; !errors.Is(err, ErrClosed) {
		t.Fatalf("the page is told the mode closed: %v", err)
	}
	drivertest.WaitFor(t, "the site without the device", func() bool {
		site, _ := r.sites.Read()
		_, zoned := site.Zones["dev_door"]
		return !zoned
	})
}

// nmcli may take long to start the network: the console's loop must keep answering
// meanwhile, or systemd's watchdog kills it in the middle of an installation.
func TestTheConsoleAnswersWhileTheNetworkStarts(t *testing.T) {
	r := newRig(t)
	slow := make(chan struct{})
	var once sync.Once
	start := func() { once.Do(func() { close(slow) }) }
	t.Cleanup(start) // before the rig's own cleanup, which waits for the network
	r.hotspot.slow = slow
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(t0.Add(30 * time.Minute))})
	drivertest.WaitFor(t, "the mode", func() bool { return r.app.Overview().Open })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.app.Alive(ctx); err != nil {
		t.Fatalf("the loop does not answer while the network starts: %v", err)
	}
	start()
	drivertest.WaitFor(t, "the page", func() bool { return r.isServing() })
}

// A pairing cut short by the end of its mode finishes after the next mode opened:
// it must neither crash the console nor show in the new mode.
func TestAPairingOfAClosedModeStaysOutOfTheNextOne(t *testing.T) {
	r := newRig(t)
	r.open()
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // never leave the worker stuck
	r.mu.Lock()
	r.keepOpen = true
	r.hold = func(env contract.Envelope) {
		if d, err := contract.DecodeData[contract.RegistryPairData](env); err == nil && env.Type == contract.TypeRegistryPair && d.DurationS == 0 {
			<-release // the network is closed only once the next mode is open
		}
	}
	r.mu.Unlock()
	if err := r.app.Pair(); err != nil {
		t.Fatal(err)
	}
	drivertest.WaitFor(t, "the window", func() bool { p := r.app.Overview().Pairing; return p != nil && p.Active })
	r.mode(contract.InstallModeData{Status: contract.InstallClosed, Reason: contract.InstallStopped})
	drivertest.WaitFor(t, "the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
	r.open()
	once.Do(func() { close(release) })
	drivertest.WaitFor(t, "the order's end", func() bool { return !r.app.Overview().Busy })
	if o := r.app.Overview(); !o.Open || o.Pairing != nil {
		t.Fatalf("open %v, pairing %+v: the old window shows in the new mode", o.Open, o.Pairing)
	}
}

// Typing the installer code again while the mode is open starts a new half hour at
// the hub: the console follows it instead of stopping at the first end.
func TestTheConsoleFollowsAModeOpenedAgain(t *testing.T) {
	r := newRig(t)
	r.open()
	r.clock.Advance(20 * time.Minute)
	r.mode(contract.InstallModeData{Status: contract.InstallOpen, Until: contract.TSPtr(r.clock.Now().Add(30 * time.Minute))})
	drivertest.WaitFor(t, "the new end", func() bool { return r.app.Overview().Until.Equal(t0.Add(50 * time.Minute)) })
	r.clock.Advance(10 * time.Minute) // the first end
	if err := r.app.Alive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, down := r.hotspot.counts(); down != 0 || !r.app.Overview().Open {
		t.Fatal("the console stopped at the first end of a mode opened again")
	}
	r.clock.Advance(20 * time.Minute)
	drivertest.WaitFor(t, "the end", func() bool { _, down := r.hotspot.counts(); return down == 1 })
}

// Two orders of the page at once (two taps, two phones) each change the site: none
// is lost.
func TestChangesOfTheSiteAtOnceAreAllKept(t *testing.T) {
	r := newRig(t)
	r.open()
	r.sites.slowRead = 5 * time.Millisecond
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Go(func() { errs <- r.app.SetZone("dev_motion", "Salle", "interior") })
	wg.Go(func() { errs <- r.app.SetZone("dev_door", "Porte", "instant") })
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	site, _ := r.sites.Read()
	if len(site.Zones) != 2 || site.Zones["dev_motion"].Name != "Salle" || site.Zones["dev_door"].Name != "Porte" {
		t.Fatalf("a change was lost: %+v", site.Zones)
	}
}

// A removal whose order is queued as the console stops gives up instead of waiting
// for a worker that is gone.
func TestARemovalDoesNotOutliveTheConsole(t *testing.T) {
	r := newRig(t)
	r.open()
	checking, release := make(chan struct{}), make(chan struct{})
	r.sites.onCheck = func() { close(checking); <-release }
	result := make(chan error, 1)
	go func() { result <- r.app.Remove(context.Background(), "dev_door") }()
	<-checking
	r.stop()
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("err %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the removal still waits for a console that stopped")
	}
}
