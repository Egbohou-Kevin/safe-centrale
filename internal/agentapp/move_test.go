package agentapp

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/schematest"
)

const newSecret = "s3cret-for-cell-b-0001"

var cellB = []string{"mqtts://b.safehome.example:8883"}

// fakeMover records what the agent asked of the disk and the connection.
type fakeMover struct {
	mu        sync.Mutex
	pending   bool
	next      string // the move that wrote the pending configuration
	current   string // the move that wrote the current one
	writeErr  error
	commitErr error
	calls     []string
	ref       string
	urls      []string
	secret    string
}

func (m *fakeMover) Pending() (string, bool) { return m.next, m.pending }
func (m *fakeMover) Current() string         { return m.current }

func (m *fakeMover) Write(ref string, urls []string, secret string) error {
	m.note("write")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ref, m.urls, m.secret = ref, urls, secret
	return m.writeErr
}

func (m *fakeMover) Commit() error  { m.note("commit"); return m.commitErr }
func (m *fakeMover) Discard() error { m.note("discard"); return nil }
func (m *fakeMover) Reconnect()     { m.note("reconnect") }

func (m *fakeMover) note(call string) { m.mu.Lock(); m.calls = append(m.calls, call); m.mu.Unlock() }

func (m *fakeMover) did() []string { m.mu.Lock(); defer m.mu.Unlock(); return slices.Clone(m.calls) }

func (r *rig) moveOrder() {
	r.t.Helper()
	r.fromCloud(topic("agent", "cmd"), contract.TypeAgentMove, contract.MoveData{URLs: cellB, Secret: newSecret,
		Until: contract.TS(r.clock.Now().Add(5 * time.Minute))})
}

func moved(t *testing.T, r *rig) contract.MovedData {
	t.Helper()
	drivertest.WaitFor(t, "agent.moved", func() bool { return len(r.cloud.OfType(contract.TypeAgentMoved)) == 1 })
	d, err := contract.ValidData[contract.MovedData](r.cloud.OfType(contract.TypeAgentMoved)[0].Env)
	if err != nil {
		t.Fatal(err)
	}
	schematest.Validate(t, "agent-moved.json", d)
	return d
}

// The order writes the new configuration and restarts the agent on it; the restarted
// agent connected with it ends the move there, and says so to the new cell. The
// secret is never in the logs.
func TestTheAgentMovesThroughARestart(t *testing.T) {
	d := newDisk()
	m := &fakeMover{}
	r := newRigMoving(t, d, DefaultConfig(site), m)
	r.app.LinkChanged(true, "mqtts")
	r.moveOrder()
	drivertest.WaitFor(t, "the restart", func() bool { return slices.Equal(m.did(), []string{"write", "reconnect"}) })
	if !slices.Equal(m.urls, cellB) || m.secret != newSecret {
		t.Fatalf("wrote %v", m.urls)
	}
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.settle()
	if len(r.cloud.OfType(contract.TypeAlarmTriggered)) != 0 {
		t.Fatal("an alarm went to the old cell during the move")
	}
	r.stop()

	m2 := &fakeMover{pending: true, next: m.ref}
	r2 := newRigMoving(t, d, DefaultConfig(site), m2)
	r2.app.LinkChanged(true, "mqtts")
	if got := moved(t, r2); got.To != contract.MovedToB {
		t.Fatalf("%+v", got)
	}
	drivertest.WaitFor(t, "the alarm on the new cell", func() bool { return len(r2.cloud.OfType(contract.TypeAlarmTriggered)) == 1 })
	if !same(r2.cloud.OfType(contract.TypeAlarmTriggered)[0], alarm) || !slices.Equal(m2.did(), []string{"commit"}) {
		t.Fatalf("%v", m2.did())
	}
	for _, logs := range []string{r.logs.String(), r2.logs.String()} {
		if strings.Contains(logs, newSecret) {
			t.Fatal("the secret is in the logs")
		}
	}
}

// The new configuration not written: the agent stays, and says why.
func TestAMoveNotWrittenStaysOnTheOldCell(t *testing.T) {
	for name, m := range map[string]*fakeMover{"write fails": {writeErr: errors.New("card full")}, "no mover": nil} {
		var mover Mover
		if m != nil {
			mover = m
		}
		r := newRigMoving(t, nil, DefaultConfig(site), mover)
		r.app.LinkChanged(true, "mqtts")
		r.moveOrder()
		if got := moved(t, r); got.To != contract.MovedBack || got.Reason != contract.MoveNoSave {
			t.Fatalf("%s: %+v", name, got)
		}
		if m != nil && slices.Contains(m.did(), "reconnect") {
			t.Fatalf("%s: restarted for nothing: %v", name, m.did())
		}
	}
}

// After a stop between the disk and the core: a new configuration the core does not
// know of is forgotten and the agent goes back to the old cell.
func TestANewConfigurationWithoutItsMoveIsForgotten(t *testing.T) {
	m := &fakeMover{pending: true}
	newRigMoving(t, nil, DefaultConfig(site), m)
	drivertest.WaitFor(t, "the way back", func() bool { return slices.Equal(m.did(), []string{"discard", "reconnect"}) })
}

// A move the core knows of without its new configuration (lost after a stop) goes
// back at once rather than wait for its deadline.
func TestAMoveWithoutItsConfigurationGoesBackAtOnce(t *testing.T) {
	d := newDisk()
	r := newRigMoving(t, d, DefaultConfig(site), &fakeMover{})
	r.moveOrder()
	r.settle()
	r.stop()
	m := &fakeMover{}
	r2 := newRigMoving(t, d, DefaultConfig(site), m)
	r2.app.LinkChanged(true, "mqtts")
	if got := moved(t, r2); got.To != contract.MovedBack || got.Reason != contract.MoveNoSave {
		t.Fatalf("%+v", got)
	}
	if !slices.Equal(m.did(), []string{"discard"}) {
		t.Fatalf("%v", m.did())
	}
}

// orderThenStop gives the agent the order to move, and stops it once the order was
// written: the move is in its saved state. It returns the move's ref.
func orderThenStop(t *testing.T, d *disk) string {
	t.Helper()
	m := &fakeMover{}
	r := newRigMoving(t, d, DefaultConfig(site), m)
	r.app.LinkChanged(true, "mqtts")
	r.moveOrder()
	drivertest.WaitFor(t, "the restart", func() bool { return slices.Contains(m.did(), "reconnect") })
	r.settle()
	r.stop()
	return m.ref
}

// B accepted the agent but its configuration could not become the current one: tried
// a few times, then again at each start, never undone: back on A, whose secret may be
// gone, the site would have no uplink. Meanwhile the agent works on B.
func TestACommitThatFailsIsTriedAgainNeverUndone(t *testing.T) {
	d := newDisk()
	ref := orderThenStop(t, d)
	full := errors.New("card full")
	m := &fakeMover{pending: true, next: ref, commitErr: full}
	r := newRigMoving(t, d, DefaultConfig(site), m)
	r.app.LinkChanged(true, "mqtts")
	if got := moved(t, r); got.To != contract.MovedToB {
		t.Fatalf("%+v", got)
	}
	drivertest.WaitFor(t, "the tries", func() bool { return len(m.did()) == commitTries })
	r.stop()

	m = &fakeMover{pending: true, next: ref, commitErr: full}
	r = newRigMoving(t, d, DefaultConfig(site), m)
	r.app.LinkChanged(true, "mqtts")
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the alarm on B", func() bool { return len(r.cloud.OfType(contract.TypeAlarmTriggered)) == 1 })
	if !same(r.cloud.OfType(contract.TypeAlarmTriggered)[0], alarm) || !slices.Equal(m.did(), []string{"commit", "commit", "commit"}) {
		t.Fatalf("%v", m.did())
	}
	r.stop()

	m = &fakeMover{pending: true, next: ref}
	r = newRigMoving(t, d, DefaultConfig(site), m)
	drivertest.WaitFor(t, "the start on B's configuration", func() bool { return slices.Equal(m.did(), []string{"commit", "reconnect"}) })
	r.stop()
	if !slices.Equal(m.did(), []string{"commit", "reconnect"}) {
		t.Fatalf("%v", m.did())
	}
}

// Stopped after the commit, before the core's state was saved: B's configuration is
// the current one, so the move ended there; it does not go back, and says so.
func TestAMoveCommittedBeforeTheStateWasSavedEndsOnB(t *testing.T) {
	d := newDisk()
	ref := orderThenStop(t, d)
	m := &fakeMover{current: ref}
	r := newRigMoving(t, d, DefaultConfig(site), m)
	r.app.LinkChanged(true, "mqtts")
	if got := moved(t, r); got != (contract.MovedData{Ref: ref, To: contract.MovedToB}) {
		t.Fatalf("%+v", got)
	}
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the alarm on B", func() bool { return len(r.cloud.OfType(contract.TypeAlarmTriggered)) == 1 })
	if !same(r.cloud.OfType(contract.TypeAlarmTriggered)[0], alarm) || len(m.did()) != 0 {
		t.Fatalf("%v", m.did())
	}
}

// The move ends on B as the agent starts: what the start settles is read before the
// loop runs, never against it (go test -race), and the move is not undone.
func TestAMoveEndingAsTheAgentStartsIsNotUndone(t *testing.T) {
	d := newDisk()
	ref := orderThenStop(t, d)
	m := &fakeMover{pending: true, next: ref}
	r := newRigStarting(t, d, DefaultConfig(site), m, func(a *App) { a.LinkChanged(true, "mqtts") })
	if got := moved(t, r); got.To != contract.MovedToB {
		t.Fatalf("%+v", got)
	}
	r.settle()
	if !slices.Equal(m.did(), []string{"commit"}) {
		t.Fatalf("%v", m.did())
	}
}
