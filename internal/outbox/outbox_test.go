package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
)

type memStore struct {
	mu    sync.Mutex
	docs  map[string][]byte
	fail  bool
	saves int
}

func (s *memStore) Save(name string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk full")
	}
	if s.docs == nil {
		s.docs = map[string][]byte{}
	}
	s.docs[name] = append([]byte(nil), data...)
	s.saves++
	return nil
}

func (s *memStore) Load(name string) ([]byte, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.docs[name]
	return d, ok, nil
}

type sent struct {
	topic  string
	env    contract.Envelope
	raw    []byte // the payload, when it is not an envelope
	retain bool
}

// broker is a fake publisher. Its answers are taken from a script, then nil.
type broker struct {
	mu     sync.Mutex
	script []error
	msgs   []sent
	calls  int
}

func (b *broker) Publish(_ context.Context, topic string, payload []byte, _ byte, retain bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if len(b.script) > 0 {
		err := b.script[0]
		b.script = b.script[1:]
		if err != nil {
			return err
		}
	}
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		b.msgs = append(b.msgs, sent{topic: topic, raw: append([]byte{}, payload...), retain: retain})
		return nil
	}
	b.msgs = append(b.msgs, sent{topic: topic, env: env, retain: retain})
	return nil
}

func (b *broker) got() []sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sent(nil), b.msgs...)
}

var ts = time.Date(2026, 10, 17, 21, 0, 0, 0, time.UTC)

var idCount int

func item(topic string, retain bool) *Item {
	idCount++
	data, _ := json.Marshal(map[string]int{"n": idCount})
	return &Item{
		Topic: topic, Src: "test", Type: contract.TypeHeartbeat, Data: data, QoS: 1, Retain: retain,
		ID: contract.NewIDGenerator().New(), TS: ts,
	}
}

func newBox(t *testing.T, b *broker, max int) (*Outbox, *memStore) {
	t.Helper()
	store := &memStore{}
	seq, err := NewSequencer(store, "seq", nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(b, seq, "tg-lom-0001", Settings{Size: max, RetryMin: time.Millisecond, RetryMax: 4 * time.Millisecond}, nil), store
}

func run(t *testing.T, o *Outbox) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMessagesLeaveInOrderWithIncreasingNumbers(t *testing.T) {
	b := &broker{}
	o, store := newBox(t, b, 10)
	stop := run(t, o)
	defer stop()
	for i := 0; i < 5; i++ {
		o.Put(item("a", false))
	}
	waitFor(t, "five messages", func() bool { return len(b.got()) == 5 })
	for i, m := range b.got() {
		if m.env.Seq != uint64(i+1) || m.env.Site != "tg-lom-0001" || m.env.Src != "test" {
			t.Fatalf("%d: %+v", i, m.env)
		}
	}
	if string(store.docs["seq"]) != `{"seq":1000}` { // the reserved block
		t.Fatalf("the counter must be saved: %s", store.docs["seq"])
	}
	if o.Depth() != 0 {
		t.Fatal("the queue must be empty")
	}
}

func TestARawMessageLeavesAsItIsWithoutTakingANumber(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	stop := run(t, o)
	defer stop()
	o.Put(item("a", false))
	o.Put(&Item{Topic: "zigbee2mqtt/0x01/set", Data: []byte(`{"state":"ON"}`), QoS: 1, Raw: true})
	o.Put(&Item{Topic: "safehome/x/device/d/state", QoS: 1, Retain: true, Raw: true}) // clears a retained state
	o.Put(item("a", false))
	waitFor(t, "four messages", func() bool { return len(b.got()) == 4 })
	got := b.got()
	if string(got[1].raw) != `{"state":"ON"}` || got[1].topic != "zigbee2mqtt/0x01/set" {
		t.Fatalf("%+v", got[1])
	}
	if got[2].raw == nil || len(got[2].raw) != 0 || !got[2].retain {
		t.Fatalf("an empty retained payload clears the topic: %+v", got[2])
	}
	if got[0].env.Seq != 1 || got[3].env.Seq != 2 {
		t.Fatalf("seq %d then %d: raw messages take no number", got[0].env.Seq, got[3].env.Seq)
	}
}

func TestAnUnavailableBrokerIsRetriedWithoutLosingTheOrder(t *testing.T) {
	b := &broker{script: []error{ErrUnavailable, ErrUnavailable, context.DeadlineExceeded}}
	o, _ := newBox(t, b, 10)
	o.Put(item("a", false))
	o.Put(item("b", false))
	stop := run(t, o)
	defer stop()
	waitFor(t, "both messages", func() bool { return len(b.got()) == 2 })
	if g := b.got(); g[0].topic != "a" || g[1].topic != "b" || g[0].env.Seq != 1 {
		t.Fatalf("%+v", g)
	}
}

func TestAMessageTheBrokerKeepsRefusingIsDropped(t *testing.T) {
	refused := errors.New("not authorized")
	b := &broker{script: []error{refused, refused, refused}}
	o, _ := newBox(t, b, 10)
	o.Put(item("bad", false))
	o.Put(item("good", false))
	stop := run(t, o)
	defer stop()
	waitFor(t, "the next message", func() bool { return len(b.got()) == 1 })
	if b.got()[0].topic != "good" {
		t.Fatalf("%+v", b.got())
	}
}

func TestOnlyTheLastRetainedStateWaits(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	o.Put(item("state", true))
	o.Put(item("event", false))
	last := item("state", true)
	o.Put(last)
	if o.Depth() != 2 {
		t.Fatalf("depth %d: an unsent retained state is replaced by the newer one", o.Depth())
	}
	stop := run(t, o)
	defer stop()
	waitFor(t, "two messages", func() bool { return len(b.got()) == 2 })
	if g := b.got(); g[0].topic != "event" || g[1].env.ID != last.ID {
		t.Fatalf("%+v", g)
	}
}

func TestAFullQueueDropsTheOldest(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 2)
	o.Put(item("1", false))
	o.Put(item("2", false))
	o.Put(item("3", false))
	stop := run(t, o)
	defer stop()
	waitFor(t, "two messages", func() bool { return len(b.got()) == 2 })
	if g := b.got(); g[0].topic != "2" || g[1].topic != "3" {
		t.Fatalf("%+v", g)
	}
}

func TestFlushSendsWhatIsLeft(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	o.Put(item("a", false))
	o.Put(item("b", false))
	o.Flush()
	if len(b.got()) != 2 {
		t.Fatalf("%+v", b.got())
	}
	// A broker that is away stops the flush at once.
	b.script = []error{ErrUnavailable}
	o.Put(item("c", false))
	o.Flush()
	if o.Depth() != 1 {
		t.Fatal("the message stays queued")
	}
}

func TestABadMessageIsDroppedNotRetried(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	bad := item("bad", false)
	bad.Type = "" // the contract refuses an envelope without a type
	o.Put(bad)
	o.Put(item("good", false))
	stop := run(t, o)
	defer stop()
	waitFor(t, "the good message", func() bool { return len(b.got()) == 1 })
}

func TestTheCounterSurvivesARestartAndADiskError(t *testing.T) {
	store := &memStore{}
	s, _ := NewSequencer(store, "seq", nil)
	if s.Next() != 1 || s.Next() != 2 {
		t.Fatal("the first numbers")
	}
	// A restart skips the rest of the reserved block: up, never a number twice.
	again, err := NewSequencer(store, "seq", nil)
	if n := again.Next(); err != nil || n != seqBlock+1 {
		t.Fatalf("after a restart: %d %v", n, err)
	}
	for again.Next() < 2*seqBlock {
	}
	store.fail = true
	if again.Next() != 2*seqBlock+1 {
		t.Fatal("a number is still handed out when the disk refuses")
	}
	store.fail = false
	if again.Next() != 2*seqBlock+2 {
		t.Fatal("the next number")
	}
	if n, _ := NewSequencer(store, "seq", nil); n.Next() != 3*seqBlock+2 {
		t.Fatal("the save was not tried again after the disk came back")
	}
	store.docs["seq"] = []byte("{")
	if _, err := NewSequencer(store, "seq", nil); err == nil {
		t.Fatal("a damaged counter is an error")
	}
}

func TestTheCounterWritesOncePerBlock(t *testing.T) {
	store := &memStore{}
	s, _ := NewSequencer(store, "seq", nil)
	for range 2500 {
		s.Next()
	}
	if store.saves != 3 {
		t.Fatalf("%d writes for 2500 numbers, want 3", store.saves)
	}
}

func TestSleepStopsWithItsContext(t *testing.T) {
	if !sleep(context.Background(), time.Millisecond) {
		t.Fatal("a full wait")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleep(ctx, time.Hour) {
		t.Fatal("a cancelled wait")
	}
}

func TestSettingsValidation(t *testing.T) {
	if err := DefaultSettings(10).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Settings{
		"no size":        {Size: 0, RetryMin: time.Second, RetryMax: time.Second},
		"no retry":       {Size: 1, RetryMin: 0, RetryMax: time.Second},
		"retry inverted": {Size: 1, RetryMin: time.Second, RetryMax: time.Millisecond},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestOpenNumbersWithTheCounterSavedUnderItsKey(t *testing.T) {
	store := &memStore{docs: map[string][]byte{"app_seq": []byte(`{"seq":41}`)}}
	b := &broker{}
	o, err := Open(store, b, "app_seq", "tg-lom-0001", DefaultSettings(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := run(t, o)
	defer stop()
	o.Put(item("a", false))
	waitFor(t, "the message", func() bool { return len(b.got()) == 1 })
	if seq := b.got()[0].env.Seq; seq != 42 {
		t.Fatalf("seq %d, want 42", seq)
	}
	store.Save("broken", []byte(`{`))
	if _, err := Open(store, b, "broken", "tg-lom-0001", DefaultSettings(10), nil); err == nil {
		t.Fatal("a damaged counter must stop the start")
	}
}
