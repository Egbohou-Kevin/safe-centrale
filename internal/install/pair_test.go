package install

import (
	"context"
	"errors"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/schematest"
)

// pairingAdapter opens the network, adds a device, refuses two, ignores another
// session's news, and closes the network.
func pairingAdapter(a *fakeAdapter, m sent) {
	if m.env.Type != contract.TypeRegistryPair {
		return
	}
	d, _ := contract.DecodeData[contract.RegistryPairData](m.env)
	if d.DurationS == 0 {
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), false)
		return
	}
	ref := m.env.ID
	ev := contract.RegistryEventTopic(site)
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), false)
	a.deliver(ev, contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{Ref: "01OTHERORDER0000000000000", DeviceID: "dev_zzzz", Model: "x", Capabilities: []string{"contact"}}, false)
	a.deliver(ev, contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{Ref: ref, DeviceID: "dev_k7p2", Model: "aqara.contact.mccgq11lm", Capabilities: []string{"contact", "battery"}}, false)
	a.deliver(ev, contract.TypeRegistryDeviceRejected, contract.RegistryDeviceRejectedData{Ref: ref, Model: "TS0203", Reason: contract.RejectUnknownModel}, false)
	a.deliver(ev, contract.TypeRegistryDeviceRejected, contract.RegistryDeviceRejectedData{Model: "", Reason: contract.RejectNotPairing}, false)
	a.deliver(ev, contract.TypeRegistryDeviceRejected, contract.RegistryDeviceRejectedData{Ref: "01OTHERORDER0000000000000", Model: "y", Reason: contract.RejectUnknownModel}, false)
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), false)
}

func TestPairReportsWhatJoinsThenEndsWithTheWindow(t *testing.T) {
	s, a := newTestSession(t, pairingAdapter)
	// A status kept by the broker from an old window must not count as a confirmation.
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), true)

	var news []PairNews
	if err := s.Pair(testCtx(t), 2*time.Minute, func(n PairNews) { news = append(news, n) }); err != nil {
		t.Fatal(err)
	}
	if len(news) != 3 || news[0].Added == nil || news[0].Added.DeviceID != "dev_k7p2" ||
		news[1].Rejected == nil || news[1].Rejected.Reason != contract.RejectUnknownModel ||
		news[2].Rejected == nil || news[2].Rejected.Reason != contract.RejectNotPairing {
		t.Fatalf("news: %+v", news)
	}

	orders := a.orders()
	if len(orders) != 1 {
		t.Fatalf("orders: %+v", orders)
	}
	o := orders[0]
	if o.topic != contract.RegistryCmdTopic(site) || o.qos != 1 || o.retain || o.env.Src != Source || o.env.Type != contract.TypeRegistryPair {
		t.Fatalf("order: %+v", o)
	}
	if d, _ := contract.DecodeData[contract.RegistryPairData](o.env); d.DurationS != 120 {
		t.Fatalf("duration %d", d.DurationS)
	}
	if err := schematest.Check("envelope.json", o.raw); err != nil {
		t.Fatal(err)
	}
	if err := schematest.Check("registry-pair.json", o.env.Data); err != nil {
		t.Fatal(err)
	}
}

func TestPairWithoutAnAdapterSaysSo(t *testing.T) {
	s, a := newTestSession(t, nil)
	a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), true) // stale
	err := s.Pair(testCtx(t), time.Minute, func(PairNews) { t.Error("news without an adapter") })
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("err %v, want ErrNoAnswer", err)
	}
}

func TestPairEndsAfterTheWindowEvenWithoutTheLastStatus(t *testing.T) {
	s, _ := newTestSession(t, func(a *fakeAdapter, m sent) {
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), false)
	})
	start := time.Now()
	if err := s.Pair(testCtx(t), time.Second, func(PairNews) {}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < time.Second || took > 3*time.Second {
		t.Fatalf("took %v for a 1 s window and 100 ms of grace", took)
	}
}

func TestInterruptedPairingClosesTheNetwork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, a := newTestSession(t, func(a *fakeAdapter, m sent) {
		if d, _ := contract.DecodeData[contract.RegistryPairData](m.env); d.DurationS > 0 {
			a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), false)
			a.deliver(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceAdded,
				contract.RegistryDeviceAddedData{Ref: m.env.ID, DeviceID: "dev_k7p2", Model: "m", Capabilities: []string{"contact"}}, false)
		}
	})
	err := s.Pair(ctx, 10*time.Minute, func(n PairNews) { cancel() }) // Ctrl-C after the first device
	if err != nil {
		t.Fatal(err)
	}
	orders := a.orders()
	if len(orders) != 2 {
		t.Fatalf("orders: %+v", orders)
	}
	if d, _ := contract.DecodeData[contract.RegistryPairData](orders[1].env); d.DurationS != 0 || orders[1].env.ID == orders[0].env.ID {
		t.Fatalf("closing order: %+v", orders[1])
	}
}

func TestPairRefusesNonsenseAndReportsSendErrors(t *testing.T) {
	s, a := newTestSession(t, nil)
	if err := s.Pair(testCtx(t), 0, func(PairNews) {}); err == nil {
		t.Error("a zero duration was accepted")
	}
	// Under a second would be sent as "pair for 0 s", the order that closes.
	if err := s.Pair(testCtx(t), 500*time.Millisecond, func(PairNews) {}); err == nil || len(a.orders()) != 0 {
		t.Errorf("a sub-second duration: err %v, orders %d", err, len(a.orders()))
	}
	a.err = errors.New("broker down")
	if err := s.Pair(testCtx(t), time.Minute, func(PairNews) {}); err == nil {
		t.Error("a send error was swallowed")
	}
}

func TestPairSkipsAStatusItCannotRead(t *testing.T) {
	s, _ := newTestSession(t, func(a *fakeAdapter, m sent) {
		// No status was ever read: a broken one must not crash the session.
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, map[string]any{"pairing": "yes"}, false)
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(true), false)
		a.deliver(contract.RegistryEventTopic(site), contract.TypeRegistryDeviceAdded,
			contract.RegistryDeviceAddedData{Ref: m.env.ID, DeviceID: "dev_k7p2", Model: "m", Capabilities: []string{"contact"}}, false)
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, map[string]any{"pairing": 0}, false)
		a.deliver(contract.ZigbeeStatusTopic(site), contract.TypeZigbeeStatus, status(false), false)
	})
	var added int
	if err := s.Pair(testCtx(t), time.Minute, func(n PairNews) { added++ }); err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("added %d", added)
	}
}
