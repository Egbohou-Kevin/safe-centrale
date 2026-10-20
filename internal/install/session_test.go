package install

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// sent is one message published by the session.
type sent struct {
	topic  string
	env    contract.Envelope
	raw    []byte
	qos    byte
	retain bool
}

// fakeAdapter plays the Zigbee adapter: it records what the session publishes and
// answers through respond, which may deliver messages back to the session.
type fakeAdapter struct {
	mu      sync.Mutex
	sent    []sent
	s       *Session
	respond func(a *fakeAdapter, m sent)
	err     error
}

func (a *fakeAdapter) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
	if a.err != nil {
		return a.err
	}
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		panic(err)
	}
	m := sent{topic: topic, env: env, raw: payload, qos: qos, retain: retain}
	a.mu.Lock()
	a.sent = append(a.sent, m)
	a.mu.Unlock()
	if a.respond != nil {
		a.respond(a, m)
	}
	return nil
}

func (a *fakeAdapter) orders() []sent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sent(nil), a.sent...)
}

// deliver gives the session a message from the adapter.
func (a *fakeAdapter) deliver(topic, typ string, data any, retain bool) {
	a.s.Deliver(msg(topic, site, typ, data, retain))
}

var msgSeq uint64

func msg(topic, siteID, typ string, data any, retain bool) Message {
	msgSeq++
	env, err := contract.NewEnvelope(siteID, contract.AdapterZigbee, msgSeq, contract.NewIDGenerator().New(), t0, typ, data)
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(env)
	return Message{Topic: topic, Payload: b, Retain: retain}
}

func newTestSession(t *testing.T, respond func(a *fakeAdapter, m sent)) (*Session, *fakeAdapter) {
	t.Helper()
	a := &fakeAdapter{respond: respond}
	ids := contract.NewIDGenerator()
	s, err := NewSession(site, Deps{Publisher: a, Now: func() time.Time { return t0 }, NewID: ids.New},
		Timing{Confirm: 200 * time.Millisecond, Grace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	a.s = s
	return s, a
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var (
	door  = contract.RegistryDevice{DeviceID: "dev_door", Model: "aqara.contact.mccgq11lm", Capabilities: []string{"contact", "battery", "link"}, Adapter: contract.AdapterZigbee}
	siren = contract.RegistryDevice{DeviceID: "dev_siren1", Model: "heiman.siren.hs2wd", Capabilities: []string{"siren", "mains"}, Adapter: contract.AdapterZigbee}
)

func status(pairing bool) contract.ZigbeeStatusData {
	return contract.ZigbeeStatusData{Bridge: contract.BridgeOnline, Devices: 2, Offline: []string{}, Pairing: pairing}
}

func TestNewSessionNeedsASiteAndItsDependencies(t *testing.T) {
	d := Deps{Publisher: &fakeAdapter{}, Now: time.Now, NewID: contract.NewIDGenerator().New}
	if _, err := NewSession("Not A Site", d, Timing{}); err == nil {
		t.Error("an invalid site was accepted")
	}
	if _, err := NewSession(site, Deps{Now: time.Now, NewID: d.NewID}, Timing{}); err == nil {
		t.Error("a session without publisher was accepted")
	}
	s, err := NewSession(site, d, Timing{})
	if err != nil {
		t.Fatal(err)
	}
	if s.timing.Confirm != 5*time.Second || s.timing.Grace != 5*time.Second {
		t.Errorf("default timing %+v", s.timing)
	}
	want := []string{
		"safehome/tg-lom-0001/registry/devices", "safehome/tg-lom-0001/registry/event",
		"safehome/tg-lom-0001/zigbee/status", "safehome/tg-lom-0001/device/+/event",
	}
	var got []string
	for _, sub := range s.Subscriptions() {
		got = append(got, sub.Filter)
		if sub.QoS != 1 {
			t.Errorf("%s: QoS %d", sub.Filter, sub.QoS)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("subscriptions %v", got)
	}
}

func TestSnapshotReadsTheRetainedRegistryAndStatus(t *testing.T) {
	s, a := newTestSession(t, nil)
	s.Deliver(Message{Topic: "safehome/tg-lom-0001/registry/devices", Payload: []byte("not json")})
	s.Deliver(msg(contract.RegistryDevicesTopic("tg-lom-0002"), "tg-lom-0002", contract.TypeRegistrySnapshot,
		contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{siren}}, true)) // another site
	a.deliver(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{door}}, true)
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), true)

	v, err := s.Snapshot(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v.Devices, []contract.RegistryDevice{door}) || v.Status == nil || v.Status.Bridge != contract.BridgeOnline {
		t.Fatalf("%+v", v)
	}
}

func TestSnapshotWithoutStatusOrRegistry(t *testing.T) {
	s, a := newTestSession(t, nil)
	a.deliver(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: []contract.RegistryDevice{}}, true)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	v, err := s.Snapshot(ctx)
	if err != nil || v.Status != nil || len(v.Devices) != 0 {
		t.Fatalf("view %+v, err %v", v, err)
	}

	empty, _ := newTestSession(t, nil)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := empty.Snapshot(ctx2); !errors.Is(err, ErrNoRegistry) {
		t.Fatalf("err %v, want ErrNoRegistry", err)
	}
}

// removingAdapter confirms removals, after a confirmation of another order.
func removingAdapter(a *fakeAdapter, m sent) {
	if m.env.Type != contract.TypeRegistryRemove {
		return
	}
	d, _ := contract.DecodeData[contract.RegistryRemoveData](m.env)
	ev := contract.RegistryEventTopic(site)
	a.deliver(ev, contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{Ref: "01OTHERORDER0000000000000", DeviceID: d.DeviceID}, false)
	a.deliver(ev, contract.TypeRegistryDeviceRemoved, contract.RegistryDeviceRemovedData{Ref: m.env.ID, DeviceID: d.DeviceID}, false)
}

func registryOf(a *fakeAdapter, devices ...contract.RegistryDevice) {
	a.deliver(contract.RegistryDevicesTopic(site), contract.TypeRegistrySnapshot, contract.RegistrySnapshotData{Devices: devices}, true)
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), true)
}

func TestRemoveWaitsForTheConfirmation(t *testing.T) {
	s, a := newTestSession(t, removingAdapter)
	registryOf(a, door, siren)
	if err := s.Remove(testCtx(t), "dev_door"); err != nil {
		t.Fatal(err)
	}
	o := a.orders()
	if len(o) != 1 || o[0].env.Type != contract.TypeRegistryRemove {
		t.Fatalf("orders %+v", o)
	}
	if err := schematest.Check("registry-remove.json", o[0].env.Data); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveOfAnUnknownDeviceSendsNothing(t *testing.T) {
	s, a := newTestSession(t, removingAdapter)
	registryOf(a, door)
	if err := s.Remove(testCtx(t), "dev_nope"); !errors.Is(err, ErrNotPaired) {
		t.Fatalf("err %v, want ErrNotPaired", err)
	}
	if len(a.orders()) != 0 {
		t.Fatal("an order was sent for an unknown device")
	}
}

func TestRemoveWithoutAnswerOrRegistry(t *testing.T) {
	s, a := newTestSession(t, nil)
	registryOf(a, door)
	if err := s.Remove(testCtx(t), "dev_door"); !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrNoAnswer", err)
	}
	empty, _ := newTestSession(t, nil)
	if err := empty.Remove(testCtx(t), "dev_door"); !errors.Is(err, ErrNoRegistry) {
		t.Fatalf("err %v, want ErrNoRegistry", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	interrupted, b := newTestSession(t, func(*fakeAdapter, sent) { cancel() })
	registryOf(b, door)
	if err := interrupted.Remove(ctx, "dev_door"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context.Canceled", err)
	}
}

func TestWatchReportsDeviceEventsWithoutCodes(t *testing.T) {
	s, a := newTestSession(t, nil)
	ev := func(id, capability, value, code string) {
		a.deliver(contract.DeviceEventTopic(site, id), contract.TypeDeviceEvent,
			contract.DeviceEventData{DeviceID: id, Capability: capability, Value: json.RawMessage(value), Code: code}, false)
	}
	ev("dev_door", "contact", `"open"`, "")
	ev("dev_motion", "motion", `true`, "")
	ev("dev_keypad", "keypad", `"disarm"`, "123456")
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), false)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var all []contract.DeviceEventData
	if err := s.Watch(ctx, "", func(at time.Time, e contract.DeviceEventData) {
		if !at.Equal(t0) {
			t.Errorf("time %v", at)
		}
		all = append(all, e)
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[2].DeviceID != "dev_keypad" || all[2].Code != "" {
		t.Fatalf("events %+v", all)
	}

	ev("dev_door", "contact", `"closed"`, "")
	ev("dev_motion", "motion", `false`, "")
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	var one []string
	s.Watch(ctx2, "dev_motion", func(_ time.Time, e contract.DeviceEventData) { one = append(one, e.DeviceID) })
	if !reflect.DeepEqual(one, []string{"dev_motion"}) {
		t.Fatalf("filtered events %v", one)
	}
}

func TestDeliverNeverBlocks(t *testing.T) {
	s, _ := newTestSession(t, nil)
	for range cap(s.in) + 10 {
		s.Deliver(Message{Topic: "x"})
	}
	if s.Dropped() != 10 {
		t.Fatalf("dropped %d, want 10", s.Dropped())
	}
}

func TestFollowKeepsTheViewAndTheEvents(t *testing.T) {
	s, a := newTestSession(t, nil)
	if _, ok := s.Current(); ok {
		t.Fatal("a view before the registry")
	}
	a.deliver(contract.DeviceEventTopic(site, "dev_door"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: "contact", Value: json.RawMessage(`"open"`)}, false)
	registryOf(a, contract.RegistryDevice{DeviceID: "dev_door", Model: "m", Capabilities: []string{"contact"}, Adapter: contract.AdapterZigbee})
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), true)
	a.deliver(contract.DeviceEventTopic(site, "dev_keypad"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_keypad", Capability: "keypad", Value: json.RawMessage(`"disarm"`), Code: "123456"}, false)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var views []View
	var events []contract.DeviceEventData
	if err := s.Follow(ctx, func(v View) { views = append(views, v) }, func(_ time.Time, e contract.DeviceEventData) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Code != "" {
		t.Fatalf("events %+v: want both, without the code", events)
	}
	// registryOf sends the registry and a status; then the second status, the event.
	if len(views) != 4 || views[0].Status != nil || !views[3].Status.Pairing || len(views[3].Devices) != 1 {
		t.Fatalf("views %+v: one from the registry on, after each message", views)
	}
	if v, ok := s.Current(); !ok || len(v.Devices) != 1 {
		t.Fatalf("current %+v %v", v, ok)
	}
}
