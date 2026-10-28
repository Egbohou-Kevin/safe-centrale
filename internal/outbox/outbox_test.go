package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
)

type sent struct {
	topic  string
	env    contract.Envelope
	raw    []byte // the payload, when it is not an envelope
	qos    byte
	retain bool
}

// broker is a fake publisher. Its answers are taken from a script, then nil.
type broker struct {
	mu     sync.Mutex
	script []error
	msgs   []sent
	calls  int
}

func (b *broker) Publish(_ context.Context, topic string, payload []byte, qos byte, retain bool) error {
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
		b.msgs = append(b.msgs, sent{topic: topic, raw: append([]byte{}, payload...), qos: qos, retain: retain})
		return nil
	}
	b.msgs = append(b.msgs, sent{topic: topic, env: env, qos: qos, retain: retain})
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

func newBox(t *testing.T, b *broker, max int) (*Outbox, *drivertest.MemStore) {
	t.Helper()
	store := drivertest.NewMemStore()
	seq, err := NewSequencer(store, "seq", nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(b, seq, "tg-lom-0001", Settings{Size: max, RetryMin: time.Millisecond, RetryMax: 4 * time.Millisecond}, contract.NewIDGenerator().New, nil), store
}

func run(t *testing.T, o *Outbox) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx); close(done) }()
	return func() { cancel(); <-done }
}

func TestMessagesLeaveInOrderWithIncreasingNumbers(t *testing.T) {
	b := &broker{}
	o, store := newBox(t, b, 10)
	stop := run(t, o)
	defer stop()
	for i := 0; i < 5; i++ {
		o.Put(item("a", false))
	}
	drivertest.WaitFor(t, "five messages", func() bool { return len(b.got()) == 5 })
	for i, m := range b.got() {
		if m.env.Seq != uint64(i+1) || m.env.Site != "tg-lom-0001" || m.env.Src != "test" {
			t.Fatalf("%d: %+v", i, m.env)
		}
	}
	if saved, _, _ := store.Load("seq"); string(saved) != `{"seq":1000}` { // the reserved block
		t.Fatalf("the counter must be saved: %s", saved)
	}
	if o.Depth() != 0 {
		t.Fatal("the queue must be empty")
	}
}

func TestSendEncodesAtOnceAndLeavesOutWhatCannotBeEncoded(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	data := map[string]int{"n": 1}
	id := o.Send("test", "a", contract.TypeHeartbeat, data, true, ts)
	data["n"] = 2 // too late: the message is encoded already
	if bad := o.Send("test", "b", contract.TypeHeartbeat, func() {}, false, ts); bad != "" || o.Depth() != 1 {
		t.Fatalf("a message that cannot be encoded is left out: id %q, depth %d", bad, o.Depth())
	}
	o.Flush()
	g := b.got()
	if len(g) != 1 || g[0].env.ID != id || id == "" || string(g[0].env.Data) != `{"n":1}` || !g[0].retain || g[0].env.Src != "test" {
		t.Fatalf("%+v", g)
	}
}

// A message with the access code goes at QoS 0, never retained: no broker keeps it
// for a subscriber away. The others at QoS 1.
func TestAMessageWithTheCodeGoesAtQoS0(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 10)
	code := o.SendWithCode("test", "a", contract.TypeDeviceEvent, map[string]string{"code": "482915"}, ts)
	o.Send("test", "b", contract.TypeHeartbeat, map[string]int{"n": 1}, false, ts)
	o.Flush()
	g := b.got()
	if len(g) != 2 || g[0].env.ID != code || g[0].qos != 0 || g[0].retain || g[1].qos != 1 {
		t.Fatalf("%+v", g)
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
	drivertest.WaitFor(t, "four messages", func() bool { return len(b.got()) == 4 })
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
	drivertest.WaitFor(t, "both messages", func() bool { return len(b.got()) == 2 })
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
	drivertest.WaitFor(t, "the next message", func() bool { return len(b.got()) == 1 })
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
	drivertest.WaitFor(t, "two messages", func() bool { return len(b.got()) == 2 })
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
	drivertest.WaitFor(t, "two messages", func() bool { return len(b.got()) == 2 })
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
	drivertest.WaitFor(t, "the good message", func() bool { return len(b.got()) == 1 })
}

func TestTheCounterSurvivesARestartAndADiskError(t *testing.T) {
	store := drivertest.NewMemStore()
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
	store.FailSaves(1)
	if again.Next() != 2*seqBlock+1 {
		t.Fatal("a number is still handed out when the disk refuses")
	}
	if again.Next() != 2*seqBlock+2 {
		t.Fatal("the next number")
	}
	if n, _ := NewSequencer(store, "seq", nil); n.Next() != 3*seqBlock+2 {
		t.Fatal("the save was not tried again after the disk came back")
	}
	store.Save("seq", []byte("{"))
	if _, err := NewSequencer(store, "seq", nil); err == nil {
		t.Fatal("a damaged counter is an error")
	}
}

func TestTheCounterWritesOncePerBlock(t *testing.T) {
	store := drivertest.NewMemStore()
	s, _ := NewSequencer(store, "seq", nil)
	for range 2500 {
		s.Next()
	}
	if n := store.Saves("seq"); n != 3 {
		t.Fatalf("%d writes for 2500 numbers, want 3", n)
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
	store := drivertest.NewMemStore()
	store.Save("app_seq", []byte(`{"seq":41}`))
	b := &broker{}
	o, err := Open(store, b, "app_seq", "tg-lom-0001", DefaultSettings(10), contract.NewIDGenerator().New, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop := run(t, o)
	defer stop()
	o.Put(item("a", false))
	drivertest.WaitFor(t, "the message", func() bool { return len(b.got()) == 1 })
	if seq := b.got()[0].env.Seq; seq != 42 {
		t.Fatalf("seq %d, want 42", seq)
	}
	store.Save("broken", []byte(`{`))
	if _, err := Open(store, b, "broken", "tg-lom-0001", DefaultSettings(10), contract.NewIDGenerator().New, nil); err == nil {
		t.Fatal("a damaged counter must stop the start")
	}
}

const site = "tg-lom-0001"

func TestAFullQueueDropsTheOldestOfTheLeastImportantFirst(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 3)
	alarm := item(contract.AlarmEventTopic(site), false)
	o.Put(alarm)
	o.Put(item(contract.DeviceEventTopic(site, "door"), false))
	o.Put(item(contract.HeartbeatTopic(site), false)) // the least important, though not the oldest
	o.Put(item(contract.DeviceEventTopic(site, "pir"), false))
	stop := run(t, o)
	defer stop()
	drivertest.WaitFor(t, "three messages", func() bool { return len(b.got()) == 3 })
	want := []string{contract.AlarmEventTopic(site), contract.DeviceEventTopic(site, "door"), contract.DeviceEventTopic(site, "pir")}
	for i, g := range b.got() {
		if g.topic != want[i] {
			t.Fatalf("message %d on %s, want %s (the alarm stays, the heartbeat goes, the order holds): %+v", i, g.topic, want[i], b.got())
		}
	}
}

func TestAFullQueueOfNoiseKeepsAnOldAlarm(t *testing.T) {
	b := &broker{}
	o, _ := newBox(t, b, 4)
	request := item(contract.AlertRequestTopic(site), false)
	o.Put(request)
	for _, d := range []string{"a", "b", "c", "d"} {
		o.Put(item(contract.DeviceEventTopic(site, d), false))
	}
	stop := run(t, o)
	defer stop()
	drivertest.WaitFor(t, "four messages", func() bool { return len(b.got()) == 4 })
	want := []string{contract.AlertRequestTopic(site), contract.DeviceEventTopic(site, "b"),
		contract.DeviceEventTopic(site, "c"), contract.DeviceEventTopic(site, "d")}
	for i, g := range b.got() {
		if g.topic != want[i] {
			t.Fatalf("message %d on %s, want %s: %+v", i, g.topic, want[i], b.got())
		}
	}
}

func TestAFullQueueOfAlarmsDropsTheOldestAndSaysSo(t *testing.T) {
	b := &broker{}
	var logs strings.Builder
	store := drivertest.NewMemStore()
	seq, err := NewSequencer(store, "seq", nil)
	if err != nil {
		t.Fatal(err)
	}
	o := New(b, seq, site, Settings{Size: 2, RetryMin: time.Millisecond, RetryMax: 4 * time.Millisecond},
		contract.NewIDGenerator().New, slog.New(slog.NewTextHandler(&logs, nil)))
	o.Put(item(contract.AlarmEventTopic(site), false))
	o.Put(item(contract.AlertRequestTopic(site), false))
	o.Put(item(contract.AlarmAckTopic(site), false))
	if !strings.Contains(logs.String(), "only alarm messages") || !strings.Contains(logs.String(), contract.AlarmEventTopic(site)) {
		t.Fatalf("an alarm message dropped must be logged as such: %q", logs.String())
	}
	stop := run(t, o)
	defer stop()
	drivertest.WaitFor(t, "two messages", func() bool { return len(b.got()) == 2 })
	if g := b.got(); g[0].topic != contract.AlertRequestTopic(site) || g[1].topic != contract.AlarmAckTopic(site) {
		t.Fatalf("%+v", g)
	}
}

func TestPriorityOfEachTopic(t *testing.T) {
	cases := map[string]priority{
		contract.AlarmEventTopic(site):                     priorityAlarm,
		contract.AlarmStateTopic(site):                     priorityAlarm,
		contract.AlarmCmdTopic(site):                       priorityAlarm,
		contract.AlarmAckTopic(site):                       priorityAlarm,
		contract.AlertRequestTopic(site):                   priorityAlarm,
		contract.AlertResultTopic(site):                    priorityAlarm,
		contract.AlertAckTopic(site):                       priorityAlarm,
		contract.AlertSeenTopic(site):                      priorityAlarm,
		contract.DeviceCmdTopic(site, "siren"):             priorityAlarm,
		contract.DeviceAckTopic(site, "siren"):             priorityAlarm,
		"zigbee2mqtt/siren/set":                            priorityAlarm,
		contract.DeviceEventTopic(site, "door"):            priorityNormal,
		contract.RegistryEventTopic(site):                  priorityNormal,
		contract.VideoSessionTopic(site):                   priorityNormal,
		contract.PhotoTakenTopic(site):                     priorityNormal,
		contract.MaintSessionTopic(site):                   priorityNormal,
		contract.ConfigAppliedTopic(site):                  priorityNormal,
		contract.ConfigSetTopic(site):                      priorityNormal,
		contract.InstallModeTopic(site):                    priorityNormal,
		contract.ContactsTopic(site):                       priorityNormal,
		contract.AgentReplayedTopic(site):                  priorityNormal,
		contract.DeviceStateTopic(site, "door", "contact"): priorityState,
		contract.HeartbeatTopic(site):                      priorityState,
		contract.HealthTopic(site):                         priorityState,
		contract.DevicesTopic(site):                        priorityState,
		contract.RegistryDevicesTopic(site):                priorityState,
		contract.GSMStatusTopic(site):                      priorityState,
		contract.ZigbeeStatusTopic(site):                   priorityState,
		contract.VideoStatusTopic(site):                    priorityState,
		contract.AgentStatusTopic(site):                    priorityState,
	}
	for topic, want := range cases {
		if got := priorityOf(topic); got != want {
			t.Errorf("%s: priority %d, want %d", topic, got, want)
		}
	}
}
