package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/sealcode"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	key    *sealcode.Key
	r      *Relay
	now    time.Time
	ids    *contract.IDGenerator
	disk   map[string]Item // the queue's store, as the application keeps it from the actions
	uplink uint64          // the counter's document, likewise
}

func newHarness(t *testing.T, edit ...func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig(site)
	key, err := sealcode.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Seal = key
	for _, e := range edit {
		e(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, key: key, r: r, now: t0, ids: contract.NewIDGenerator(), disk: map[string]Item{}}
}

// handle hands an input to the relay, carries out its queue actions on the disk, and
// checks that the disk holds the queue, item for item.
func (h *harness) handle(in Input) []Action {
	h.t.Helper()
	acts := h.r.Handle(in)
	h.keep(acts)
	return acts
}

// next is Next, as the application calls it.
func (h *harness) next() (Item, bool) {
	h.t.Helper()
	it, acts, ok := h.r.Next()
	h.keep(acts)
	return it, ok
}

func (h *harness) keep(acts []Action) {
	h.t.Helper()
	h.apply(h.disk, acts)
	stones := 0
	for id, it := range h.disk {
		if it.Sent {
			stones++
			if !h.r.buried[id] {
				h.t.Fatalf("a tombstone on disk not remembered: %s", id)
			}
		}
	}
	if len(h.disk)-stones != len(h.r.queue) || stones != len(h.r.tombs) {
		h.t.Fatalf("%d items and %d tombstones on disk, %d in the queue, %d remembered", len(h.disk)-stones, stones, len(h.r.queue), len(h.r.tombs))
	}
	for _, it := range h.r.queue {
		if !reflect.DeepEqual(h.disk[it.ID], it) {
			h.t.Fatalf("on disk %+v\nin the queue %+v", h.disk[it.ID], it)
		}
	}
}

func (h *harness) apply(disk map[string]Item, acts []Action) {
	for _, a := range acts {
		switch v := a.(type) {
		case QueueSave:
			disk[v.Item.ID] = v.Item
		case QueueDrop:
			for _, id := range v.IDs {
				delete(disk, id)
			}
		case UplinkSave:
			h.uplink = v.Last
		}
	}
}

// restart builds a new relay from what the old one saved: its snapshot and the
// queue's store, both through JSON as on the disk.
func (h *harness) restart() {
	h.t.Helper()
	h.r = h.restored(h.r.Snapshot(), h.disk)
}

func (h *harness) restored(snap Snapshot, disk map[string]Item) *Relay {
	h.t.Helper()
	r, err := New(h.r.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	r.Restore(roundTrip(h.t, snap))
	var stored []Item
	for _, it := range disk {
		stored = append(stored, roundTrip(h.t, it))
	}
	h.apply(disk, r.RestoreQueue(stored, h.uplink))
	return r
}

func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func topic(parts ...string) string { return contract.Topic(site, parts...) }

// msg builds a message of the site; it returns its payload and id.
func (h *harness) msg(typ string, data any) ([]byte, string) {
	h.t.Helper()
	id := h.ids.New()
	env, err := contract.NewEnvelope(site, "hub", 1, id, h.now, typ, data)
	if err != nil {
		h.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b, id
}

func (h *harness) local(tp, typ string, data any) (string, []Action) {
	h.t.Helper()
	b, id := h.msg(typ, data)
	return id, h.handle(Local{Now: h.now, Topic: tp, Payload: b})
}

func (h *harness) cloud(tp, typ string, data any) (string, []byte, []Action) {
	h.t.Helper()
	b, id := h.msg(typ, data)
	return id, b, h.handle(Cloud{Now: h.now, Topic: tp, Payload: b})
}

func (h *harness) advance(d time.Duration) []Action {
	h.now = h.now.Add(d)
	return h.handle(Tick{Now: h.now})
}

func (h *harness) linkUp() { h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"}) }

func (h *harness) queued() []string {
	var out []string
	for _, it := range h.r.queue {
		out = append(out, it.Topic[len(topic()):])
	}
	return out
}

func of[A Action](actions []Action) []A {
	var out []A
	for _, a := range actions {
		if v, ok := a.(A); ok {
			out = append(out, v)
		}
	}
	return out
}

var (
	armed     = contract.ArmedData{}
	triggered = contract.TriggeredData{Incident: "inc1", DeviceID: "dev_door", Capability: contract.CapContact, Zone: "Entree"}
	health    = contract.HealthData{Links: map[string]string{"zigbee": "ok"}}
)

func event(dev, capability, value, code string) contract.DeviceEventData {
	return contract.DeviceEventData{DeviceID: dev, Capability: capability, Value: json.RawMessage(fmt.Sprintf("%q", value)), Code: code}
}

func appAlert(incident string) contract.AlertRequestData {
	return contract.AlertRequestData{Incident: incident, Kind: contract.AlertIntrusion, Urgency: contract.UrgencyCritical,
		Wave: 1, Contact: "u_owner", Channel: contract.ChannelApp, Text: "ALARME"}
}

// sealed is a code as a phone seals it for the relay, at the harness's time.
func (h *harness) sealed(code, command string) string { return h.sealedAt(h.now, code, command) }

func (h *harness) sealedAt(at time.Time, code, command string) string {
	h.t.Helper()
	s, err := sealcode.Seal(h.key.Public(), code, site, command, at)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}
