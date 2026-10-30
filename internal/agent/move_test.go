package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
)

const newSecret = "s3cret-for-cell-b-0001"

var cellB = []string{"mqtts://b.safehome.example:8883", "wss://b.safehome.example:443/mqtt"}

func (h *harness) moveOrder(edit ...func(*contract.MoveData)) (string, []Action) {
	h.t.Helper()
	d := contract.MoveData{URLs: cellB, Secret: newSecret, Until: contract.TS(h.now.Add(5 * time.Minute))}
	for _, e := range edit {
		e(&d)
	}
	id, _, acts := h.cloud(topic("agent", "cmd"), contract.TypeAgentMove, d)
	return id, acts
}

// moving builds a relay connected to cell A that sent two alarm events, then got the
// order to move and left A.
func movingHarness(t *testing.T) (*harness, string, []Item) {
	t.Helper()
	h := newHarness(t)
	h.linkUp()
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	before := h.send()
	ref, acts := h.moveOrder()
	if m := of[Move](acts); len(m) != 1 || m[0].Ref != ref || m[0].Secret != newSecret || !slices.Equal(m[0].URLs, cellB) {
		t.Fatalf("the order did not move the agent: %+v", acts)
	}
	h.handle(Link{Now: h.now, Up: false})
	return h, ref, before
}

func movedOf(t *testing.T, acts []Action) contract.MovedData {
	t.Helper()
	m := of[Moved](acts)
	if len(m) != 1 {
		t.Fatalf("no single agent.moved in %+v", acts)
	}
	if err := m[0].Report.Validate(); err != nil {
		t.Fatal(err)
	}
	return m[0].Report
}

// From the order on, nothing goes up to A: what comes waits in the queue, in its
// order, with no number given; the hub sees the link down and turns to the GSM.
func TestAMoveFreezesWhatGoesUp(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	pending, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	_, acts := h.moveOrder()
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != pending {
		t.Fatalf("the alert the cloud cannot confirm any more does not fail: %+v", acts)
	}
	if st := of[StatusChanged](acts); len(st) != 1 || st[0].Status.Up {
		t.Fatalf("the hub is not told the link is down: %+v", acts)
	}
	// Still connected to A: nothing leaves.
	var ids []string
	for _, it := range h.r.queue {
		ids = append(ids, it.ID)
	}
	for range 3 {
		id, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
		ids = append(ids, id)
	}
	if _, got := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc2")); len(of[FailAlert](got)) != 1 {
		t.Fatalf("an alert for the app during the move does not fail at once: %+v", got)
	}
	if it, ok := h.next(); ok {
		t.Fatalf("%s left during the move", it.Topic)
	}
	var queued []string
	for _, it := range h.r.queue {
		if it.Seq != 0 {
			t.Fatalf("numbered during the move: %+v", it)
		}
		queued = append(queued, it.ID)
	}
	if !slices.Equal(queued[:len(ids)], ids) || len(h.disk) != len(h.r.queue) {
		t.Fatalf("the queue lost or reordered: %v, want %v", queued, ids)
	}
	if h.r.Status(h.now).Up {
		t.Fatal("agent.status says up during the move")
	}
}

// A photo whose address came before the move waits too: its host is A's.
func TestAPhotoWaitsDuringTheMove(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.photo("p1")
	h.sendQueue()
	h.moveOrder()
	if ups := of[Upload](h.address("p1", 10*time.Minute)); len(ups) != 0 {
		t.Fatalf("a photo leaves during the move: %+v", ups)
	}
	h.handle(Link{Now: h.now, Up: false})
	if ups := of[Upload](h.handle(Link{Now: h.now, Up: true, Transport: "mqtts", Next: true})); len(ups) != 0 {
		t.Fatalf("a photo for A's host goes to B: %+v", ups)
	}
}

// Cell B accepts: the new configuration is the current one, and the queue goes to B
// in its order, its numbers following those A got.
func TestCellBAcceptingEndsTheMoveThere(t *testing.T) {
	h, ref, before := movingHarness(t)
	var waiting []string
	for range 2 {
		id, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
		waiting = append(waiting, id)
	}
	// A again, without the new configuration: still frozen.
	h.linkUp()
	if _, ok := h.next(); ok {
		t.Fatal("the queue went to A during the move")
	}
	h.handle(Link{Now: h.now, Up: false})

	acts := h.handle(Link{Now: h.now, Up: true, Transport: "mqtts", Next: true})
	if c := of[MoveCommit](acts); len(c) != 1 || c[0].Ref != ref {
		t.Fatalf("no commit: %+v", acts)
	}
	if r := movedOf(t, acts); r != (contract.MovedData{Ref: ref, To: contract.MovedToB}) {
		t.Fatalf("%+v", r)
	}
	if !h.r.Status(h.now).Up || h.r.Moving() {
		t.Fatal("still frozen on B")
	}
	got := h.send()
	if len(got) != 2 || got[0].ID != waiting[0] || got[1].ID != waiting[1] {
		t.Fatalf("sent %+v", got)
	}
	if seqOf(t, got[0]) != before[len(before)-1].Seq+1 || seqOf(t, got[1]) != got[0].Seq+1 {
		t.Fatalf("renumbered: %d, %d after %d", got[0].Seq, got[1].Seq, before[len(before)-1].Seq)
	}
	if !slices.Equal(h.r.cfg.CloudURLs, cellB) {
		t.Fatalf("the photos still go to A's hosts: %v", h.r.cfg.CloudURLs)
	}
	// The move ended: a late connection with it changes nothing.
	if acts := h.handle(Link{Now: h.now, Up: true, Transport: "mqtts", Next: true}); len(of[MoveCommit](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
}

// B still unreachable at the deadline, counted from the order: back to A, and the
// queue goes to A.
func TestCellBUnreachableSendsTheAgentBackToA(t *testing.T) {
	h, ref, _ := movingHarness(t)
	id, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	if wake := h.r.NextWake(); !wake.Equal(t0.Add(contract.DefaultMoveGiveUp * time.Second)) {
		t.Fatalf("wakes at %v", wake)
	}
	if acts := h.advance(contract.DefaultMoveGiveUp*time.Second - time.Second); h.r.Moving() == false || len(of[MoveBack](acts)) != 0 {
		t.Fatalf("back before the deadline: %+v", acts)
	}
	acts := h.advance(time.Second)
	if b := of[MoveBack](acts); len(b) != 1 || b[0].Ref != ref || b[0].Reason != contract.MoveUnreachable {
		t.Fatalf("not back at the deadline: %+v", acts)
	}
	if r := movedOf(t, acts); r.To != contract.MovedBack || r.Reason != contract.MoveUnreachable {
		t.Fatalf("%+v", r)
	}
	// B answers too late: ignored, the agent stays down until A is back.
	h.handle(Link{Now: h.now, Up: true, Transport: "mqtts", Next: true})
	if h.r.Up() {
		t.Fatal("a late connection to B is taken for the link")
	}
	h.linkUp()
	if got := h.send(); len(got) != 1 || got[0].ID != id {
		t.Fatalf("the queue does not go to A: %+v", got)
	}
}

// A restart in the middle goes on: still frozen, B tried until the deadline counted
// from the order, not from the restart; the snapshot never holds the secret.
func TestAMoveSurvivesARestart(t *testing.T) {
	for _, arrives := range []bool{true, false} {
		h, ref, _ := movingHarness(t)
		id, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
		h.now = h.now.Add(4 * time.Minute)
		b, _ := json.Marshal(h.r.Snapshot())
		if strings.Contains(string(b), newSecret) {
			t.Fatal("the snapshot holds the secret")
		}
		h.restart()
		if !h.r.Moving() || !h.r.NextWake().Equal(t0.Add(contract.DefaultMoveGiveUp*time.Second)) {
			t.Fatalf("the restart forgot the move, or its deadline: %v", h.r.NextWake())
		}
		h.linkUp() // A, without the new configuration
		if _, ok := h.next(); ok {
			t.Fatal("unfrozen by a restart")
		}
		h.handle(Link{Now: h.now, Up: false})
		var acts []Action
		if arrives {
			acts = h.handle(Link{Now: h.now, Up: true, Transport: "mqtts", Next: true})
			if len(of[MoveCommit](acts)) != 1 {
				t.Fatalf("%+v", acts)
			}
		} else {
			acts = h.advance(6 * time.Minute)
			if len(of[MoveBack](acts)) != 1 {
				t.Fatalf("%+v", acts)
			}
			h.linkUp()
		}
		if movedOf(t, acts).Ref != ref {
			t.Fatal("wrong ref")
		}
		if got := h.send(); len(got) == 0 || got[0].ID != id {
			t.Fatalf("arrives %v: %+v", arrives, got)
		}
	}
}

// A stale, malformed or second order is refused and logged: nothing moves.
func TestABadOrSecondMoveIsRefused(t *testing.T) {
	for name, edit := range map[string]func(*contract.MoveData){
		"expired":      func(d *contract.MoveData) { d.Until = contract.TS(t0.Add(-time.Second)) },
		"until now":    func(d *contract.MoveData) { d.Until = contract.TS(t0) },
		"plain mqtt":   func(d *contract.MoveData) { d.URLs = []string{"mqtt://b.example:1883"} },
		"short secret": func(d *contract.MoveData) { d.Secret = "short" },
		"for a day":    func(d *contract.MoveData) { d.GiveUpAfter = 86400 },
	} {
		h := newHarness(t)
		h.linkUp()
		_, acts := h.moveOrder(edit)
		if len(of[Refused](acts)) != 1 || len(of[Move](acts)) != 0 || h.r.Moving() {
			t.Errorf("%s: %+v", name, acts)
		}
		for _, r := range of[Refused](acts) {
			if strings.Contains(r.Reason, newSecret) {
				t.Errorf("%s: the refusal quotes the secret", name)
			}
		}
	}
	h, ref, _ := movingHarness(t)
	h.now = h.now.Add(time.Minute)
	_, acts := h.moveOrder()
	if len(of[Refused](acts)) != 1 || len(of[Move](acts)) != 0 || h.r.s.Move.Ref != ref || !h.r.s.Move.GiveUp.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("a second order changed the move: %+v", acts)
	}
	// A connection with a new configuration, no move under way: ignored.
	h2 := newHarness(t)
	if acts := h2.handle(Link{Now: h2.now, Up: true, Transport: "mqtts", Next: true}); len(acts) != 0 || h2.r.Up() {
		t.Fatalf("%+v", acts)
	}
}

// The new configuration could not be written: back to A at once.
func TestAMoveNotSavedGoesBackAtOnce(t *testing.T) {
	h, ref, _ := movingHarness(t)
	if acts := h.handle(MoveFailed{Now: h.now, Ref: "another"}); len(acts) != 0 {
		t.Fatalf("%+v", acts)
	}
	acts := h.handle(MoveFailed{Now: h.now, Ref: ref})
	if b := of[MoveBack](acts); len(b) != 1 || b[0].Reason != contract.MoveNoSave {
		t.Fatalf("%+v", acts)
	}
	if r := movedOf(t, acts); !reflect.DeepEqual(r, contract.MovedData{Ref: ref, To: contract.MovedBack, Reason: contract.MoveNoSave}) {
		t.Fatalf("%+v", r)
	}
}

// agent.moved goes up as the agent's own message, the move order never reaches the
// hub, and a Move never prints its secret.
func TestTheMoveIsTheAgentsOwn(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	_, acts := h.moveOrder()
	if len(of[PublishLocal](acts)) != 0 {
		t.Fatal("the order reached the hub")
	}
	m := of[Move](acts)[0]
	for _, s := range []string{m.String(), m.GoString()} {
		if strings.Contains(s, newSecret) {
			t.Fatalf("printed: %s", s)
		}
	}
	report, _ := h.msg(contract.TypeAgentMoved, contract.MovedData{Ref: "m", To: contract.MovedToB})
	if got := h.handle(Own{Now: h.now, Topic: topic("agent", "moved"), Payload: report}); len(of[QueueSave](got)) != 1 {
		t.Fatalf("agent.moved does not go up: %+v", got)
	}
	if got := h.handle(Local{Now: h.now, Topic: topic("agent", "moved"), Payload: report}); len(got) != 0 {
		t.Fatalf("the hub's broker gives agent.moved back and it goes again: %+v", got)
	}
}

// A forward step of the clock (NTP after a power cut) does not end the move early: B
// keeps the time that really passed.
func TestAClockStepKeepsTheMovesTime(t *testing.T) {
	h, _, _ := movingHarness(t)
	h.r.Stepped(time.Hour)
	if acts := h.advance(time.Hour + time.Minute); len(of[MoveBack](acts)) != 0 || !h.r.Moving() {
		t.Fatalf("the step ended the move: %+v", acts)
	}
	if !h.r.NextWake().Equal(t0.Add(time.Hour + 10*time.Minute)) {
		t.Fatalf("wakes at %v", h.r.NextWake())
	}
}
