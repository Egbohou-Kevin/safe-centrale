package agent

import (
	"slices"
	"testing"
	"time"

	"safehome/internal/contract"
)

func (h *harness) photo(id string) []Action {
	h.t.Helper()
	_, acts := h.local(topic("photo", "taken"), contract.TypePhotoTaken,
		contract.PhotoTakenData{Photo: id, Incident: "inc1", Camera: "cam_caisse", Bytes: 40_000, Width: 640})
	return acts
}

func (h *harness) address(id string, valid time.Duration) []Action {
	h.t.Helper()
	_, _, acts := h.cloud(topic("photo", "cmd"), contract.TypePhotoUpload, contract.PhotoUploadData{
		Photo: id, URL: "https://api.safehome.example/v1/photos/" + id, Token: "tok-" + id, Exp: contract.TS(h.now.Add(valid)),
	})
	return acts
}

// sendQueue acknowledges everything queued, as the cloud's broker would.
func (h *harness) sendQueue() []Action {
	var out []Action
	for _, it := range h.r.Snapshot().Queue {
		out = append(out, h.r.Handle(Sent{Now: h.now, ID: it.ID})...)
	}
	return out
}

func reports(acts []Action) []PhotoReport { return of[PhotoReport](acts) }

func TestAPhotoGoesUpThenIsSentAfterTheAlarm(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, contract.TriggeredData{Incident: "inc1", DeviceID: "d", Capability: contract.CapContact, Zone: "z"})
	h.photo("p1")
	if q := h.queued(); !slices.Equal(q, []string{"alarm/event", "photo/taken"}) {
		t.Fatalf("%v", q)
	}
	if it, _ := h.r.Next(); it.Topic != topic("alarm", "event") {
		t.Fatal("the alarm first")
	}
	// The address comes while the alarm still waits: no upload yet.
	acts := h.address("p1", 10*time.Minute)
	if len(of[PublishLocal](acts)) != 0 || len(of[Upload](acts)) != 0 {
		t.Fatalf("%+v: the address stays with the agent, and the alarm goes first", acts)
	}
	ups := of[Upload](h.sendQueue())
	if len(ups) != 1 || ups[0] != (Upload{Photo: "p1", URL: "https://api.safehome.example/v1/photos/p1", Token: "tok-p1"}) {
		t.Fatalf("%+v", ups)
	}
	if acts := h.advance(time.Second); len(of[Upload](acts)) != 0 {
		t.Fatal("one upload at a time, once")
	}
	acts = h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadOK})
	if r := reports(acts); len(r) != 1 || r[0].Status != contract.PhotoSent || len(of[RemovePhoto](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	if len(h.r.Snapshot().Photos) != 0 {
		t.Fatal("forgotten once sent")
	}
}

func TestAFailedUploadIsTriedAgainLater(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.photo("p1")
	h.sendQueue()
	if len(of[Upload](h.address("p1", time.Hour))) != 1 {
		t.Fatal("upload at once")
	}
	h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadRetry})
	if acts := h.advance(29 * time.Second); len(of[Upload](acts)) != 0 {
		t.Fatal("too soon")
	}
	if w := h.r.NextWake(); !w.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("wake %v", w)
	}
	if acts := h.advance(time.Second); len(of[Upload](acts)) != 1 {
		t.Fatal("after 30 s")
	}
	h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadRetry})
	if acts := h.advance(59 * time.Second); len(of[Upload](acts)) != 0 {
		t.Fatal("twice longer")
	}
	// The link drops when the retry is due: no upload, and no wake-up in the past.
	h.r.Handle(Link{Now: h.now, Up: false})
	h.advance(time.Second)
	if w := h.r.NextWake(); !w.After(h.now) {
		t.Fatalf("wake %v at %v: the loop would spin", w, h.now)
	}
	if acts := h.r.Handle(Link{Now: h.now, Up: true, Transport: "mqtts"}); len(of[Upload](acts)) != 1 {
		t.Fatalf("%+v: back up, the photo goes", acts)
	}
}

func TestAnAddressThatExpiresDuringItsUploadDoesNotSpinTheLoop(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.photo("p1")
	h.sendQueue()
	if len(of[Upload](h.address("p1", time.Minute))) != 1 {
		t.Fatal("upload at once")
	}
	// The upload is slow: the address expires while it is under way. The relay waits
	// for its result; a wake-up in the past would tick the loop without a pause.
	h.advance(2 * time.Minute)
	if w := h.r.NextWake(); !w.After(h.now) {
		t.Fatalf("wake %v at %v: the loop would spin until the upload ends", w, h.now)
	}
	acts := h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadRetry})
	acts = append(acts, h.advance(time.Second)...)
	if r := reports(acts); len(r) != 1 || r[0].Reason != contract.PhotoURLExpired {
		t.Fatalf("%+v: once the upload ended, the stale address is dropped", acts)
	}
}

func TestAnAddressThatExpires(t *testing.T) {
	h := newHarness(t)
	h.photo("p1")
	h.sendQueue()
	h.address("p1", 5*time.Minute) // the link is down: kept
	acts := h.advance(5 * time.Minute)
	if r := reports(acts); len(r) != 1 || r[0].Reason != contract.PhotoURLExpired {
		t.Fatalf("%+v", acts)
	}
	h.linkUp()
	if acts := h.address("p1", 5*time.Minute); len(of[Upload](acts)) != 1 {
		t.Fatalf("a new address: %+v", acts)
	}
	acts = h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadExpired})
	if r := reports(acts); len(r) != 1 || r[0].Reason != contract.PhotoURLExpired || len(of[RemovePhoto](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
	if p := h.r.Snapshot().Photos["p1"]; p.URL != "" || p.Token != "" {
		t.Fatal("forgotten address")
	}
}

func TestPhotosThatAreDropped(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	for _, id := range []string{"p1", "p2", "p3"} {
		h.photo(id)
	}
	h.photo("p1") // QoS 1: again
	h.sendQueue()
	h.address("p1", time.Hour)
	acts := h.r.Handle(Uploaded{Now: h.now, Photo: "p1", Result: UploadMissing})
	if r := reports(acts); len(r) != 1 || r[0].Reason != contract.PhotoMissing {
		t.Fatalf("%+v", acts)
	}
	acts = h.address("p2", time.Hour)
	if len(of[Upload](acts)) != 1 {
		t.Fatal(acts)
	}
	acts = h.r.Handle(Uploaded{Now: h.now, Photo: "p2", Result: UploadRefused})
	if r := reports(acts); len(r) != 1 || r[0].Reason != contract.PhotoRefused || len(of[RemovePhoto](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	// Never an address: dropped after a day.
	acts = h.advance(24 * time.Hour)
	if r := reports(acts); len(r) != 1 || r[0].Photo != "p3" || r[0].Reason != contract.PhotoTooOld {
		t.Fatalf("%+v", acts)
	}
	if h.r.Handle(Uploaded{Now: h.now, Photo: "p3", Result: UploadOK}) != nil {
		t.Fatal("a photo no longer followed")
	}
}

func TestOnlyAFewPhotosAreFollowed(t *testing.T) {
	h := newHarness(t)
	var dropped []PhotoReport
	for range maxPhotos + 2 {
		h.now = h.now.Add(time.Second)
		dropped = append(dropped, reports(h.photo(h.ids.New()))...)
	}
	if len(dropped) != 2 || len(h.r.Snapshot().Photos) != maxPhotos {
		t.Fatalf("%d dropped, %d followed", len(dropped), len(h.r.Snapshot().Photos))
	}
}

func TestUploadOrdersAreChecked(t *testing.T) {
	h := newHarness(t)
	h.photo("p1")
	for name, d := range map[string]contract.PhotoUploadData{
		"unknown photo": {Photo: "p9", URL: "https://x.example/p", Token: "t", Exp: contract.TS(h.now.Add(time.Hour))},
		"http":          {Photo: "p1", URL: "http://x.example/p", Token: "t", Exp: contract.TS(h.now.Add(time.Hour))},
		"expired":       {Photo: "p1", URL: "https://x.example/p", Token: "t", Exp: contract.TS(h.now)},
	} {
		_, _, acts := h.cloud(topic("photo", "cmd"), contract.TypePhotoUpload, d)
		if len(of[Refused](acts)) != 1 {
			t.Errorf("%s: %+v", name, acts)
		}
	}
	// A photo announcement that breaks the contract is not followed.
	h.local(topic("photo", "taken"), contract.TypePhotoTaken, contract.PhotoTakenData{Photo: "px"})
	if _, ok := h.r.Snapshot().Photos["px"]; ok {
		t.Fatal("followed")
	}
	// What the agent says goes up too.
	h.local(topic("photo", "sent"), contract.TypePhotoSent, contract.PhotoSentData{Photo: "p1", Status: contract.PhotoSent})
	if !slices.Contains(h.queued(), "photo/sent") {
		t.Fatal("photo.sent goes up")
	}
}

func TestTheUploadIsForgottenAtARestart(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.photo("p1")
	h.sendQueue()
	h.address("p1", time.Hour)
	r2, _ := New(DefaultConfig(site))
	r2.Restore(h.r.Snapshot())
	if acts := r2.Handle(Link{Now: h.now, Up: true, Transport: "mqtts"}); len(of[Upload](acts)) != 1 {
		t.Fatalf("%+v: the upload under way died with the process; it starts again", acts)
	}
}

func TestMaintenanceOrdersPassWhenValid(t *testing.T) {
	h := newHarness(t)
	_, _, acts := h.cloud(topic("maint", "cmd"), contract.TypeMaintOpen, contract.MaintOpenData{
		Session: "m1", User: "kevin", Reason: "modem", DurationS: 3600, Exp: contract.TS(h.now.Add(time.Minute))})
	if len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	_, _, acts = h.cloud(topic("maint", "cmd"), contract.TypeMaintClose, contract.MaintCloseData{Session: "m1"})
	if len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	for name, d := range map[string]any{
		"two hours": contract.MaintOpenData{Session: "m2", User: "k", Reason: "r", DurationS: 7200, Exp: contract.TS(h.now.Add(time.Minute))},
		"expired":   contract.MaintOpenData{Session: "m3", User: "k", Reason: "r", DurationS: 600, Exp: contract.TS(h.now)},
	} {
		if _, _, acts := h.cloud(topic("maint", "cmd"), contract.TypeMaintOpen, d); len(of[Refused](acts)) != 1 {
			t.Errorf("%s: %+v", name, acts)
		}
	}
	if _, _, acts := h.cloud(topic("maint", "cmd"), contract.TypeMaintClose, contract.MaintCloseData{}); len(of[Refused](acts)) != 1 {
		t.Errorf("close without session: %+v", acts)
	}
	h.local(topic("maint", "session"), contract.TypeMaintSession, contract.MaintSessionData{Session: "m1", Status: contract.MaintOpen})
	if !slices.Contains(h.queued(), "maint/session") {
		t.Fatal("the owner sees it: it goes up")
	}
}
