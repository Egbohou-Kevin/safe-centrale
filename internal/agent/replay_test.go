package agent

import (
	"math"
	"reflect"
	"testing"
	"time"

	"safehome/internal/contract"
)

// send sends the queue as the application would: Next, then Sent once the cloud's
// broker took it. It returns what left, in order.
func (h *harness) send() []Item {
	h.t.Helper()
	var out []Item
	for {
		it, ok := h.next()
		if !ok {
			return out
		}
		out = append(out, it)
		h.handle(Sent{Now: h.now, ID: it.ID})
		if len(out) > 100 {
			h.t.Fatal("the queue never empties")
		}
	}
}

// seqOf reads the number in a payload, checking that it is still a valid envelope.
func seqOf(t *testing.T, it Item) uint64 {
	t.Helper()
	env, err := contract.ParseEnvelope(it.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if env.ID != it.ID || env.Seq != it.Seq {
		t.Fatalf("envelope %s/%d, item %s/%d", env.ID, env.Seq, it.ID, it.Seq)
	}
	return env.Seq
}

func (h *harness) replay(from, to uint64) []Action {
	h.t.Helper()
	_, _, acts := h.cloud(topic("agent", "cmd"), contract.TypeAgentReplay, contract.ReplayData{From: from, To: to})
	return acts
}

func TestWhatGoesToTheCloudIsNumberedWithoutHoles(t *testing.T) {
	h := newHarness(t)
	h.local(topic("system", "health"), contract.TypeHealth, health)
	h.local(topic("device", "dev_smoke", "event"), contract.TypeDeviceEvent, event("dev_smoke", contract.CapSmoke, "detected", ""))
	h.local(topic("device", "dev_door", "event"), contract.TypeDeviceEvent, event("dev_door", contract.CapContact, "open", "")) // stays on the site
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	h.linkUp()

	// A message tried without an acknowledgement keeps its number.
	first, _ := h.next()
	again, _ := h.next()
	if first.Seq != 1 || again.Seq != 1 || again.ID != first.ID {
		t.Fatalf("%d then %d", first.Seq, again.Seq)
	}
	var seqs []uint64
	for _, it := range h.send() {
		seqs = append(seqs, seqOf(t, it))
	}
	// In the order of sending, the alarm first: not the order of arrival.
	if !reflect.DeepEqual(seqs, []uint64{1, 2, 3}) {
		t.Fatalf("%v", seqs)
	}

	// The count survives a restart.
	h.restart()
	h.local(topic("alarm", "event"), contract.TypeAlarmCleared, contract.ClearedData{Incident: "inc1"})
	h.linkUp()
	if got := h.send(); len(got) != 1 || seqOf(t, got[0]) != 4 {
		t.Fatalf("%+v", got)
	}
}

func TestAReplacedStateTakesTheNumberOfTheOneItReplaces(t *testing.T) {
	h := newHarness(t)
	h.local(topic("system", "health"), contract.TypeHealth, health)
	h.linkUp()
	tried, _ := h.next() // the link drops before the acknowledgement
	newer, _ := h.local(topic("system", "health"), contract.TypeHealth, contract.HealthData{Links: map[string]string{"zigbee": "down"}})
	got := h.send()
	if len(got) != 1 || got[0].ID != newer || seqOf(t, got[0]) != tried.Seq {
		t.Fatalf("%+v, tried %d", got, tried.Seq)
	}
	if h.uplink != 1 {
		t.Fatal("no number may be lost")
	}
}

func TestTheCloudGetsAgainWhatItMissed(t *testing.T) {
	h := newHarness(t)
	h.local(topic("alarm", "state"), contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed})
	for range 4 {
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	h.linkUp()
	sent := h.send()
	if len(sent) != 5 || !sent[4].Retain {
		t.Fatalf("%+v", sent)
	}
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	queued, _ := h.next() // 6, on its way

	acts := h.replay(4, 9)
	rep := of[Replayed](acts)
	want := contract.ReplayedData{From: 4, To: 9, Last: 6, Resent: 3, Missing: []contract.SeqRange{{First: 7, Last: 9}}}
	if len(rep) != 1 || !reflect.DeepEqual(rep[0].Report, want) {
		t.Fatalf("%+v", acts)
	}
	if len(of[PublishLocal](acts)) != 0 {
		t.Fatal("a replay is for the agent: it never reaches the hub")
	}
	if st := h.r.Status(h.now); st.Queued != 3 {
		t.Fatalf("queued %d: the replays count", st.Queued)
	}
	// The live message first, then the replays, as they were, and never retained.
	got := h.send()
	if len(got) != 3 || got[0].ID != queued.ID {
		t.Fatalf("%+v", got)
	}
	for i, it := range got[1:] {
		if it.ID != sent[3+i].ID || seqOf(t, it) != uint64(4+i) || it.Retain {
			t.Fatalf("replay %d: %+v", i, it)
		}
	}
	// Asking twice queues it once.
	h.replay(5, 5)
	h.replay(5, 5)
	if got := h.send(); len(got) != 1 {
		t.Fatalf("%+v", got)
	}
}

func TestTheJournalIsBoundedAndNotSaved(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.JournalItems = 2 })
	for range 3 {
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	h.linkUp()
	h.send()
	rep := of[Replayed](h.replay(1, 3))[0].Report
	if rep.Resent != 2 || !reflect.DeepEqual(rep.Missing, []contract.SeqRange{{First: 1, Last: 1}}) {
		t.Fatalf("%+v", rep)
	}
	h.send()

	h.restart()
	rep = of[Replayed](h.replay(1, 3))[0].Report
	if rep.Resent != 0 || rep.Last != 3 || !reflect.DeepEqual(rep.Missing, []contract.SeqRange{{First: 1, Last: 3}}) {
		t.Fatalf("after a restart: %+v", rep)
	}
}

func TestTheJournalIsBoundedInBytes(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.JournalBytes = 1024 })
	for range 20 {
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	h.linkUp()
	h.send()
	rep := of[Replayed](h.replay(1, 20))[0].Report
	if rep.Resent == 0 || rep.Resent == 20 || len(rep.Missing) != 1 || rep.Missing[0].First != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestABadReplayIsRefused(t *testing.T) {
	h := newHarness(t)
	acts := h.replay(0, 3)
	if len(of[Refused](acts)) != 1 || len(of[Replayed](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
	acts = h.replay(1, 1+contract.MaxReplay)
	if len(of[Refused](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
}

// A replay that ends at the top of the numbers ends: counting past it would go back
// to 0 and never stop, and the agent would hang until the watchdog killed it.
func TestAReplayAtTheTopOfTheNumbersEnds(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	done := make(chan []Action, 1)
	go func() { done <- h.replay(math.MaxUint64-2, math.MaxUint64) }()
	select {
	case acts := <-done:
		r := of[Replayed](acts)
		if len(r) != 1 || len(r[0].Report.Missing) != 1 || r[0].Report.Missing[0] != (contract.SeqRange{First: math.MaxUint64 - 2, Last: math.MaxUint64}) {
			t.Fatalf("%+v", acts)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replay never ends")
	}
}
