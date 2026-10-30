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
	mu       sync.Mutex
	pending  bool
	writeErr error
	calls    []string
	urls     []string
	secret   string
}

func (m *fakeMover) Pending() bool { return m.pending }

func (m *fakeMover) Write(urls []string, secret string) error {
	m.note("write")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.urls, m.secret = urls, secret
	return m.writeErr
}

func (m *fakeMover) Commit() error  { m.note("commit"); return nil }
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

	m2 := &fakeMover{pending: true}
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
