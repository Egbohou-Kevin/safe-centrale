package agentapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/outbox"
)

// --- fakes ---------------------------------------------------------------------

// memQueue is the queue's store.
type memQueue struct {
	mu      sync.Mutex
	items   map[string][]byte
	written []int // the size of each Put
	removed int
}

func (q *memQueue) Put(name string, data []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items[name] = append([]byte(nil), data...)
	q.written = append(q.written, len(data))
	return nil
}

func (q *memQueue) Remove(names ...string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, n := range names {
		delete(q.items, n)
	}
	q.removed += len(names)
	return nil
}

func (q *memQueue) Load() (map[string][]byte, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return maps.Clone(q.items), nil
}

// disk is what the agent keeps between two runs.
type disk struct {
	store *drivertest.MemStore
	queue *memQueue
}

func newDisk() *disk {
	return &disk{store: drivertest.NewMemStore(), queue: &memQueue{items: map[string][]byte{}}}
}

type published struct {
	topic  string
	retain bool
	raw    []byte
	env    contract.Envelope
}

// fakeBroker records what is published; while down it answers ErrUnavailable. It
// refuses for good the messages of type refuse.
type fakeBroker struct {
	mu     sync.Mutex
	msgs   []published
	down   bool
	refuse string
}

func (b *fakeBroker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return outbox.ErrUnavailable
	}
	env, _ := contract.ParseEnvelope(payload)
	if env.Type == b.refuse {
		return fmt.Errorf("%w: not authorized", ErrRefused)
	}
	b.msgs = append(b.msgs, published{topic: topic, retain: retain, raw: append([]byte(nil), payload...), env: env})
	return nil
}

func (b *fakeBroker) setDown(down bool) { b.mu.Lock(); b.down = down; b.mu.Unlock() }

func (b *fakeBroker) all() []published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]published(nil), b.msgs...)
}

func (b *fakeBroker) ofType(typ string) []published {
	var out []published
	for _, m := range b.all() {
		if m.env.Type == typ {
			out = append(out, m)
		}
	}
	return out
}

// --- rig -----------------------------------------------------------------------

// memPhotos is the photos' folder.
type memPhotos struct {
	mu      sync.Mutex
	files   map[string][]byte
	removed []string
}

func (m *memPhotos) Open(id string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[id]
	if !ok {
		return nil, 0, fmt.Errorf("%w: %s", ErrNoPhoto, id)
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *memPhotos) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, id)
	m.removed = append(m.removed, id)
	return nil
}

// fakeUploader plays the cloud's upload endpoint: errs are answered in turn.
type fakeUploader struct {
	mu   sync.Mutex
	errs []error
	got  []string // url token bytes
}

func (u *fakeUploader) Put(_ context.Context, url, token string, body io.Reader, size int64) error {
	b, _ := io.ReadAll(body)
	u.mu.Lock()
	defer u.mu.Unlock()
	if int64(len(b)) != size {
		return fmt.Errorf("size %d, read %d", size, len(b))
	}
	u.got = append(u.got, fmt.Sprintf("%s %s %d", url, token, len(b)))
	if len(u.errs) > 0 {
		err := u.errs[0]
		u.errs = u.errs[1:]
		return err
	}
	return nil
}

func (u *fakeUploader) calls() []string { u.mu.Lock(); defer u.mu.Unlock(); return slices.Clone(u.got) }

type rig struct {
	t      *testing.T
	clock  *drivertest.FakeClock
	disk   *disk
	local  *fakeBroker
	cloud  *fakeBroker
	photos *memPhotos
	upl    *fakeUploader
	app    *App
	ids    *contract.IDGenerator
	stop   func()
}

func newRig(t *testing.T, d *disk) *rig {
	t.Helper()
	return newRigWith(t, d, DefaultConfig(site))
}

func newRigWith(t *testing.T, d *disk, cfg Config) *rig {
	t.Helper()
	if d == nil {
		d = newDisk()
	}
	r := &rig{t: t, clock: drivertest.NewFakeClock(t0), disk: d, local: &fakeBroker{}, cloud: &fakeBroker{}, ids: contract.NewIDGenerator(),
		photos: &memPhotos{files: map[string][]byte{}}, upl: &fakeUploader{}}
	app, err := New(cfg, Deps{Clock: r.clock, Store: d.store, Queue: d.queue, Local: r.local, Cloud: r.cloud,
		Photos: r.photos, Uploader: r.upl, NewID: r.ids.New})
	if err != nil {
		t.Fatal(err)
	}
	r.app = app
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { app.Run(ctx); close(done) }()
	r.stop = func() { cancel(); <-done }
	t.Cleanup(r.stop)
	drivertest.WaitFor(t, "the first status", func() bool { return len(r.local.ofType(contract.TypeAgentStatus)) > 0 })
	return r
}

// settle waits until the loop has taken what was given to it, the sender has sent
// all it could, and the outbox to the hub is empty. The sender is woken twice: it
// takes the second wake-up only after a round that found nothing more to send.
func (r *rig) settle() {
	r.t.Helper()
	r.app.Alive(context.Background())
	for range 2 {
		woken := false
		drivertest.WaitFor(r.t, "the sender to take a wake-up", func() bool {
			if !woken {
				select {
				case r.app.kick <- struct{}{}:
					woken = true
				default:
				}
			}
			return woken && len(r.app.kick) == 0
		})
	}
	drivertest.WaitFor(r.t, "the outbox to the hub to empty", func() bool { return r.app.out.Depth() == 0 })
}

// msg builds a message of the site.
func (r *rig) msg(typ string, data any) []byte {
	r.t.Helper()
	env, err := contract.NewEnvelope(site, "hub", 1, r.ids.New(), r.clock.Now(), typ, data)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	return b
}

func (r *rig) fromHub(topic, typ string, data any) []byte {
	b := r.msg(typ, data)
	r.app.HandleLocal(inbox.Message{Topic: topic, Payload: b})
	return b
}

func (r *rig) fromCloud(topic, typ string, data any) []byte {
	b := r.msg(typ, data)
	r.app.HandleCloud(inbox.Message{Topic: topic, Payload: b})
	return b
}

func lastStatus(t *testing.T, b *fakeBroker) contract.AgentStatusData {
	t.Helper()
	st := b.ofType(contract.TypeAgentStatus)
	if len(st) == 0 {
		t.Fatal("no agent.status")
	}
	d, err := contract.DecodeData[contract.AgentStatusData](st[len(st)-1].env)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func topic(parts ...string) string { return contract.Topic(site, parts...) }

// same says whether m is the message sent: the agent numbers again what goes to the
// cloud, so only seq may differ.
func same(m published, sent []byte) bool {
	env, err := contract.ParseEnvelope(sent)
	if err != nil {
		return false
	}
	got := m.env
	got.Seq, env.Seq = 0, 0
	return reflect.DeepEqual(got, env)
}

var triggered = contract.TriggeredData{Incident: "inc1", DeviceID: "dev_door", Capability: contract.CapContact, Zone: "Entree"}
