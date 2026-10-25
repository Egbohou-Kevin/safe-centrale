package agentapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"safehome/internal/agent"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/inbox"
	"safehome/internal/outbox/outboxtest"
	"safehome/internal/schematest"
)

const site = "tg-lom-0001"

var t0 = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

// --- tests -----------------------------------------------------------------------

func TestTheStatusSaysWhetherTheClientsDataIsSpared(t *testing.T) {
	if lastStatus(t, newRig(t, nil).local).Economy {
		t.Error("economy on a fixed line")
	}
	cfg := DefaultConfig(site)
	cfg.Relay.Economy = agent.EconomyEvery
	if !lastStatus(t, newRigWith(t, nil, cfg).local).Economy {
		t.Error("economy not said on mobile data: the cloud would take the site for silent")
	}
}

func TestNewNeedsItsPortsAndAGoodConfig(t *testing.T) {
	d := newDisk()
	good := Deps{Clock: drivertest.NewFakeClock(t0), Store: d.store, Queue: d.queue, Local: &outboxtest.Broker{}, Cloud: &outboxtest.Broker{}, Photos: &memPhotos{},
		Uploader: &fakeUploader{}, NewID: contract.NewIDGenerator().New}
	if _, err := New(DefaultConfig(site), Deps{Clock: good.Clock, Store: d.store, Queue: d.queue, Local: good.Local, NewID: good.NewID}); err == nil {
		t.Error("an agent without the cloud was built")
	}
	noQueue := good
	noQueue.Queue = nil
	if _, err := New(DefaultConfig(site), noQueue); err == nil {
		t.Error("an agent without a store for its queue was built")
	}
	bad := DefaultConfig(site)
	bad.StatusEvery = 0
	if _, err := New(bad, good); err == nil {
		t.Error("a zero status period was accepted")
	}
	other := DefaultConfig(site)
	other.Relay.Site = "tg-lom-0002"
	if _, err := New(other, good); err == nil {
		t.Error("two sites were accepted")
	}
	broken := DefaultConfig(site)
	broken.Relay.ConfirmWait = 0
	if _, err := New(broken, good); err == nil {
		t.Error("a broken relay config was accepted")
	}
}

func TestSubscriptionsNeverHearThemselves(t *testing.T) {
	r := newRig(t, nil)
	var local, cloud []string
	for _, s := range r.app.LocalSubscriptions() {
		local = append(local, s.Filter)
		if !s.NoLocal || s.QoS != 1 {
			t.Errorf("%+v", s)
		}
	}
	for _, s := range r.app.CloudSubscriptions() {
		cloud = append(cloud, s.Filter)
		// The order with the access code is read at QoS 0: never kept for an agent away.
		wantQoS := byte(1)
		if s.Filter == topic("alarm", "cmd") {
			wantQoS = 0
		}
		if !s.NoLocal || s.QoS != wantQoS {
			t.Errorf("%+v", s)
		}
	}
	if !strings.Contains(strings.Join(local, " "), topic("alarm", "event")) || !strings.Contains(strings.Join(cloud, " "), topic("alarm", "cmd")) {
		t.Fatalf("local %v, cloud %v", local, cloud)
	}
}

func TestAtStartTheLinkIsDownAndNothingIsSent(t *testing.T) {
	r := newRig(t, nil)
	st := r.local.OfType(contract.TypeAgentStatus)
	if !st[0].Retain || lastStatus(t, r.local).Uplink != contract.UplinkDown {
		t.Fatalf("%+v", st[0])
	}
	if err := schematest.Check("agent-status.json", st[0].Env.Data); err != nil {
		t.Fatal(err)
	}
	if err := schematest.Check("envelope.json", st[0].Payload); err != nil {
		t.Fatal(err)
	}
	if st[0].Env.Src != contract.AgentLink {
		t.Fatalf("src %q", st[0].Env.Src)
	}
	r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.settle()
	if n := len(r.cloud.All()); n != 0 {
		t.Fatalf("%d messages went to a cloud the agent is not connected to", n)
	}
}

func TestWhenTheLinkComesTheQueueLeavesAlarmsFirst(t *testing.T) {
	r := newRig(t, nil)
	r.fromHub(topic("system", "health"), contract.TypeHealth, contract.HealthData{Links: map[string]string{}})
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.fromHub(topic("device", "dev_door", "event"), contract.TypeDeviceEvent,
		contract.DeviceEventData{DeviceID: "dev_door", Capability: contract.CapContact, Value: json.RawMessage(`"open"`)}) // stays
	// The loop takes its inputs in order: the three are queued before the link comes.
	r.app.LinkChanged(true, "mqtts")
	drivertest.WaitFor(t, "the queue to leave", func() bool { return len(r.cloud.All()) >= 3 })
	msgs := r.cloud.All()
	if !same(msgs[0], alarm) || msgs[0].Retain {
		t.Fatalf("first to the cloud: %s %s", msgs[0].Topic, msgs[0].Payload)
	}
	var topics []string
	for _, m := range msgs {
		topics = append(topics, m.Topic)
		if m.Topic == topic("device", "dev_door", "event") {
			t.Fatal("a raw contact event went to the cloud")
		}
	}
	joined := strings.Join(topics, " ")
	if !strings.Contains(joined, topic("system", "health")) || !strings.Contains(joined, topic("agent", "status")) {
		t.Fatalf("the states should follow: %v", topics)
	}
	if st := lastStatus(t, r.local); st.Uplink != contract.UplinkUp || st.Transport != "mqtts" {
		t.Fatalf("%+v", st)
	}
	drivertest.WaitFor(t, "an empty queue", func() bool {
		queued := -1
		r.app.loop.Do(context.Background(), func(now time.Time) []agent.Action { // read the core on its loop
			queued = r.app.core.Status(now).Queued
			return nil
		})
		return queued == 0
	})
}

func TestAMessageTheCloudDidNotTakeIsTriedAgain(t *testing.T) {
	r := newRig(t, nil)
	r.cloud.SetDown(true)
	r.app.LinkChanged(true, "wss")
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.clock.WaitTimer(t, r.clock.Now().Add(time.Second)) // refused: the sender waits before it tries again
	if len(r.cloud.All()) != 0 {
		t.Fatal("sent to a refusing broker")
	}
	r.cloud.SetDown(false)
	r.clock.Advance(time.Second) // the first retry delay
	drivertest.WaitFor(t, "the retry", func() bool {
		for _, m := range r.cloud.All() {
			if same(m, alarm) {
				return true
			}
		}
		return false
	})
}

func TestAMessageTheCloudRefusesForGoodIsDropped(t *testing.T) {
	r := newRig(t, nil)
	r.cloud.Refuse(func(m outboxtest.Message) error {
		if m.Env.Type == contract.TypeAlarmArmed {
			return fmt.Errorf("%w: not authorized", ErrRefused)
		}
		return nil
	})
	r.fromHub(topic("alarm", "event"), contract.TypeAlarmArmed, contract.ArmedData{})
	r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	r.app.LinkChanged(true, "mqtts")
	// No retry delay to wait for: the clock does not move.
	drivertest.WaitFor(t, "the next message to leave", func() bool { return len(r.cloud.OfType(contract.TypeAlarmTriggered)) == 1 })
	drivertest.WaitFor(t, "the refused one to leave the queue", func() bool { return r.queued() == 0 })
	r.clock.Advance(time.Minute)
	drivertest.WaitFor(t, "the status", func() bool { return lastStatus(t, r.local).Dropped == 1 })
}

func TestTheCloudGetsAgainWhatItMissed(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the alarm to leave", func() bool { return len(r.cloud.OfType(contract.TypeAlarmTriggered)) == 1 })
	first := r.cloud.OfType(contract.TypeAlarmTriggered)[0]
	if first.Env.Seq == 0 {
		t.Fatal("what goes to the cloud is numbered")
	}

	r.fromCloud(topic("agent", "cmd"), contract.TypeAgentReplay, contract.ReplayData{From: first.Env.Seq, To: first.Env.Seq + 500})
	drivertest.WaitFor(t, "the replay", func() bool { return len(r.cloud.OfType(contract.TypeAlarmTriggered)) == 2 })
	again := r.cloud.OfType(contract.TypeAlarmTriggered)[1]
	if !same(again, alarm) || !bytes.Equal(again.Payload, first.Payload) {
		t.Fatalf("replayed %s, first %s", again.Payload, first.Payload)
	}
	drivertest.WaitFor(t, "the report", func() bool { return len(r.cloud.OfType(contract.TypeAgentReplayed)) == 1 })
	rep, err := contract.DecodeData[contract.ReplayedData](r.cloud.OfType(contract.TypeAgentReplayed)[0].Env)
	if err != nil || rep.Resent < 1 || len(rep.Missing) != 1 || rep.Missing[0].Last != first.Env.Seq+500 {
		t.Fatalf("%+v %v", rep, err)
	}
	if len(r.local.OfType(contract.TypeAgentReplay)) != 0 {
		t.Fatal("the request is for the agent: the hub never sees it")
	}
	schematest.Validate(t, "agent-replayed.json", rep)
}

func TestAnAppAlertWithoutTheCloudFailsOnTheHub(t *testing.T) {
	r := newRig(t, nil)
	req := r.fromHub(topic("alert", "request"), contract.TypeAlertRequest, contract.AlertRequestData{Incident: "inc1",
		Kind: contract.AlertIntrusion, Urgency: contract.UrgencyCritical, Wave: 1, Contact: "u_owner", Channel: contract.ChannelApp, Text: "ALARME"})
	env, _ := contract.ParseEnvelope(req)
	drivertest.WaitFor(t, "the failure", func() bool { return len(r.local.OfType(contract.TypeAlertResult)) == 1 })
	res := r.local.OfType(contract.TypeAlertResult)[0]
	d, _ := contract.DecodeData[contract.AlertResultData](res.Env)
	if d.Ref != env.ID || d.Status != contract.ResultFailed || d.Reason != contract.ReasonNoUplink || res.Topic != topic("alert", "result") {
		t.Fatalf("%+v %+v", res, d)
	}
	if err := schematest.Check("alert-result.json", res.Env.Data); err != nil {
		t.Fatal(err)
	}
}

// The system clock is set back 30 minutes (an RTC that ran fast, corrected by NTP):
// the agent's own time stands still until the clock catches up, but an order the app
// sends now, stamped on the corrected clock, is fresh. It reaches the hub, which judges
// it on the same clock, instead of being answered expired for half an hour.
func TestAnOrderAfterTheClockWasSetBackReachesTheHub(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	r.fromHub(topic("alarm", "state"), contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed, Since: contract.TS(t0)})
	r.settle()
	back := t0.Add(-30 * time.Minute)
	r.clock.Set(back)
	r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm,
		Args: contract.CommandArgs{Code: "482915"}, Origin: contract.OriginApp, User: "u_owner", Exp: contract.TSPtr(back.Add(time.Minute))})
	drivertest.WaitFor(t, "the order on the hub", func() bool {
		for _, m := range r.local.All() {
			if m.Topic == topic("alarm", "cmd") {
				return true
			}
		}
		return false
	})
	if acks := r.cloud.OfType(contract.TypeAlarmAck); len(acks) != 0 {
		t.Fatalf("a fresh order answered by the agent: %+v", acks)
	}
}

// An order from the app that came too late is answered to the cloud at once, by the
// agent: the hub never gets it, and the answer never carries the code.
func TestAnOrderTheHubWillNotGetIsAnsweredToTheCloud(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	order := r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm,
		Args: contract.CommandArgs{Code: "482915"}, Origin: contract.OriginApp, User: "u_owner", Exp: contract.TSPtr(t0.Add(-time.Second))})
	env, _ := contract.ParseEnvelope(order)
	drivertest.WaitFor(t, "the answer", func() bool { return len(r.cloud.OfType(contract.TypeAlarmAck)) == 1 })
	ack := r.cloud.OfType(contract.TypeAlarmAck)[0]
	d, err := contract.DecodeData[contract.AlarmAckData](ack.Env)
	if err != nil || d.Ref != env.ID || d.Status != contract.AckExpired || d.Reason != "" || ack.Topic != topic("alarm", "ack") || ack.Env.Src != contract.AgentLink {
		t.Fatalf("%+v %+v %v", ack, d, err)
	}
	if bytes.Contains(ack.Payload, []byte("482915")) {
		t.Fatal("the answer carries the code")
	}
	if err := schematest.Check("alarm-ack.json", ack.Env.Data); err != nil {
		t.Fatal(err)
	}
	r.settle()
	for _, m := range r.local.All() {
		if m.Topic == topic("alarm", "cmd") || m.Topic == topic("alarm", "ack") {
			t.Fatalf("the hub never got the order, nor its answer: %s %s", m.Topic, m.Payload)
		}
	}
}

func TestOrdersFromTheCloudReachTheHubUnchanged(t *testing.T) {
	r := newRig(t, nil)
	order := r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp, User: "u_owner"})
	r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, contract.AlarmCommandData{Command: contract.CmdDisarm, Origin: contract.OriginKeypad})
	r.fromCloud(topic("device", "dev_siren1", "cmd"), contract.TypeCommand, map[string]string{"command": "siren.on"})
	drivertest.WaitFor(t, "the order", func() bool {
		for _, m := range r.local.All() {
			if m.Topic == topic("alarm", "cmd") {
				return true
			}
		}
		return false
	})
	r.settle()
	var orders []outboxtest.Message
	for _, m := range r.local.All() {
		if m.Topic == topic("alarm", "cmd") || strings.HasSuffix(m.Topic, "/cmd") {
			orders = append(orders, m)
		}
	}
	if len(orders) != 1 || !bytes.Equal(orders[0].Payload, order) {
		t.Fatalf("%d orders reached the hub: %+v", len(orders), orders)
	}
	// An order may carry the access code: at QoS 0 on the hub's broker too, which
	// keeps none for a hub away (a restart), neither in memory nor in mosquitto.db.
	// The rest at QoS 1.
	r.fromCloud(topic("config", "set"), contract.TypeConfigSet, contract.ConfigSetData{Version: 3})
	drivertest.WaitFor(t, "the configuration", func() bool { return len(r.local.On(topic("config", "set"))) == 1 })
	if orders[0].QoS != 0 || r.local.On(topic("config", "set"))[0].QoS != 1 {
		t.Fatalf("order at QoS %d, configuration at QoS %d", orders[0].QoS, r.local.On(topic("config", "set"))[0].QoS)
	}
}

func TestTheQueueSurvivesARestartOfTheAgent(t *testing.T) {
	d := newDisk()
	r := newRig(t, d)
	alarm := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the queue to be saved", func() bool {
		items, _ := d.queue.Load()
		for _, it := range items {
			if bytes.Contains(it, []byte("alarm/event")) {
				return true
			}
		}
		return false
	})
	r.stop()

	again := newRig(t, d)
	again.app.LinkChanged(true, "mqtts")
	drivertest.WaitFor(t, "the saved message", func() bool {
		for _, m := range again.cloud.All() {
			if same(m, alarm) {
				return true
			}
		}
		return false
	})
	// The agent's own numbering goes on after the restart.
	first := r.local.OfType(contract.TypeAgentStatus)
	second := again.local.OfType(contract.TypeAgentStatus)
	if second[0].Env.Seq <= first[len(first)-1].Env.Seq {
		t.Fatalf("seq went from %d back to %d", first[len(first)-1].Env.Seq, second[0].Env.Seq)
	}
}

// At each new connection, the cloud gets the hub's current state again, as a new
// message: it may have lost the one sent before (its database reset).
func TestEachConnectionSaysTheCurrentStateAgain(t *testing.T) {
	r := newRig(t, nil)
	r.fromHub(topic("alarm", "state"), contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed, Since: contract.TS(t0)})
	r.app.LinkChanged(true, "mqtts")
	drivertest.WaitFor(t, "the state", func() bool { return len(r.cloud.OfType(contract.TypeAlarmState)) == 1 })
	r.app.LinkChanged(false, "")
	r.app.LinkChanged(true, "wss")
	drivertest.WaitFor(t, "the state again", func() bool { return len(r.cloud.OfType(contract.TypeAlarmState)) == 2 })
	states := r.cloud.OfType(contract.TypeAlarmState)
	first, again := states[0].Env, states[1].Env
	if again.ID == first.ID || again.Seq <= first.Seq || !states[1].Retain {
		t.Fatalf("a new id and a new number, retained: %+v after %+v", again, first)
	}
	again.ID, again.Seq = first.ID, first.Seq
	if !reflect.DeepEqual(again, first) {
		t.Fatalf("said again, the state must be the hub's: %+v, %+v", again, first)
	}
	if err := schematest.Check("envelope.json", states[1].Payload); err != nil {
		t.Fatal(err)
	}
	if len(r.local.OfType(contract.TypeAlarmState)) != 0 {
		t.Fatal("said again to the cloud only, never on the hub")
	}
}

func TestTheStatusIsRepeated(t *testing.T) {
	r := newRig(t, nil)
	n := len(r.local.OfType(contract.TypeAgentStatus))
	r.clock.Advance(time.Minute)
	drivertest.WaitFor(t, "the repeated status", func() bool { return len(r.local.OfType(contract.TypeAgentStatus)) > n })
}

func TestTheWillSaysTheSiteIsOffline(t *testing.T) {
	r := newRig(t, nil)
	tp, payload := r.app.Will()
	env, err := contract.ParseEnvelope(payload)
	if err != nil || tp != topic("agent", "status") || env.Seq != 0 {
		t.Fatalf("%s %+v %v", tp, env, err)
	}
	if d, _ := contract.DecodeData[contract.AgentStatusData](env); d.Uplink != contract.UplinkDown {
		t.Fatalf("%+v", d)
	}
	_, again := r.app.Will()
	if bytes.Equal(payload, again) {
		t.Fatal("two wills with the same id: the cloud would drop the second as a duplicate")
	}
}

func TestOversizedMessagesAreLeftAside(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	pad := strings.Repeat("x", maxPayload)
	// Valid messages the core would pass on, made too big by an extra field.
	r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, map[string]any{
		"incident": "inc1", "device_id": "dev_door", "capability": contract.CapContact, "zone": "Entree", "pad": pad,
	})
	r.fromCloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, map[string]any{
		"command": contract.CmdArm, "origin": contract.OriginApp, "user": "u_owner", "pad": pad,
	})
	small := r.fromHub(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	drivertest.WaitFor(t, "the small message in the cloud", func() bool {
		return slices.ContainsFunc(r.cloud.All(), func(m outboxtest.Message) bool { return same(m, small) })
	})
	r.settle()
	for _, m := range append(r.cloud.All(), r.local.All()...) {
		if len(m.Payload) > maxPayload {
			t.Fatalf("an oversized message went through to %s", m.Topic)
		}
	}
	if !strings.Contains(r.logs.String(), "oversized message from the cloud") || !strings.Contains(r.logs.String(), "oversized message to the cloud") {
		t.Fatal("both are logged")
	}
}

func TestRunTwiceIsRefused(t *testing.T) {
	r := newRig(t, nil)
	if err := r.app.Run(context.Background()); err == nil {
		t.Fatal("Run twice")
	}
}

// --- photos ----------------------------------------------------------------------------

func (r *rig) photoTaken(id string, size int) {
	r.photos.mu.Lock()
	r.photos.files[id] = make([]byte, size)
	r.photos.mu.Unlock()
	r.fromHub(topic("photo", "taken"), contract.TypePhotoTaken, contract.PhotoTakenData{Photo: id, Incident: "inc1", Camera: "cam_caisse", Bytes: int64(size), Width: 640})
}

func (r *rig) uploadOrder(id string) {
	r.fromCloud(topic("photo", "cmd"), contract.TypePhotoUpload, contract.PhotoUploadData{
		Photo: id, URL: "https://api.safehome.example/v1/photos/" + id, Token: "tok-" + id, Exp: contract.TS(r.clock.Now().Add(time.Hour)),
	})
}

func (r *rig) sent(id string) []contract.PhotoSentData {
	var out []contract.PhotoSentData
	for _, p := range r.local.OfType(contract.TypePhotoSent) {
		d, _ := contract.DecodeData[contract.PhotoSentData](p.Env)
		if d.Photo == id {
			out = append(out, d)
		}
	}
	return out
}

func TestAPhotoIsUploadedThenRemoved(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	r.photoTaken("p1", 4000)
	drivertest.WaitFor(t, "photo.taken in the cloud", func() bool { return len(r.cloud.OfType(contract.TypePhotoTaken)) == 1 })
	r.uploadOrder("p1")
	drivertest.WaitFor(t, "the upload", func() bool { return len(r.upl.calls()) == 1 })
	if c := r.upl.calls()[0]; c != "https://api.safehome.example/v1/photos/p1 tok-p1 4000" {
		t.Fatalf("%s", c)
	}
	drivertest.WaitFor(t, "photo.sent", func() bool { return len(r.sent("p1")) == 1 })
	if d := r.sent("p1")[0]; d.Status != contract.PhotoSent {
		t.Fatalf("%+v", d)
	}
	drivertest.WaitFor(t, "photo.sent in the cloud", func() bool { return len(r.cloud.OfType(contract.TypePhotoSent)) == 1 })
	r.photos.mu.Lock()
	defer r.photos.mu.Unlock()
	if len(r.photos.files) != 0 || !slices.Equal(r.photos.removed, []string{"p1"}) {
		t.Fatalf("%v", r.photos.removed)
	}
	for _, m := range r.local.All() {
		if strings.Contains(string(m.Payload), "tok-p1") {
			t.Fatal("the token reached the hub")
		}
	}
}

func TestUploadFailures(t *testing.T) {
	r := newRig(t, nil)
	r.app.LinkChanged(true, "mqtts")
	r.upl.errs = []error{errors.New("connection reset"), fmt.Errorf("%w: 403", ErrUploadExpired)}
	r.photoTaken("p1", 100)
	r.uploadOrder("p1")
	drivertest.WaitFor(t, "the first try", func() bool { return len(r.upl.calls()) == 1 })
	r.clock.PassTimer(t, 30*time.Second)
	drivertest.WaitFor(t, "the second try", func() bool { return len(r.upl.calls()) == 2 })
	drivertest.WaitFor(t, "url_expired", func() bool { return len(r.sent("p1")) == 1 })
	if d := r.sent("p1")[0]; d.Reason != contract.PhotoURLExpired {
		t.Fatalf("%+v", d)
	}
	// A new address, and a file refused.
	r.upl.mu.Lock()
	r.upl.errs = []error{fmt.Errorf("%w: 413", ErrUploadRefused)}
	r.upl.mu.Unlock()
	r.uploadOrder("p1")
	drivertest.WaitFor(t, "refused", func() bool { return len(r.sent("p1")) == 2 })
	if d := r.sent("p1")[1]; d.Reason != contract.PhotoRefused {
		t.Fatalf("%+v", d)
	}
	// A photo whose file is gone.
	r.fromHub(topic("photo", "taken"), contract.TypePhotoTaken, contract.PhotoTakenData{Photo: "p2", Incident: "inc1", Camera: "c", Bytes: 1, Width: 1})
	r.uploadOrder("p2")
	drivertest.WaitFor(t, "missing", func() bool { return len(r.sent("p2")) == 1 && r.sent("p2")[0].Reason == contract.PhotoMissing })
	if n := len(r.upl.calls()); n != 3 {
		t.Fatalf("%d uploads", n)
	}
}

// A message that arrives while the agent stops (here during its last flush to the
// hub, which the hub's broker holds) is given up without an error.
// A message that reaches the agent once it is stopping is not taken: the broker keeps
// it for the next start (the agent acknowledges only what it took). One taken before
// is carried out.
func TestMessagesDuringShutdownAreGivenUpQuietly(t *testing.T) {
	r := newRig(t, nil)
	r.settle()
	first := r.msg(contract.TypeAlarmTriggered, triggered)
	if !r.app.HandleLocal(inbox.Message{Topic: topic("alarm", "event"), Payload: first}) {
		t.Fatal("a message is taken while the agent runs")
	}
	release := r.local.Hold()
	r.app.LinkChanged(true, "mqtts") // a status left for the last flush
	drivertest.WaitFor(t, "a status to send", func() bool { return r.app.out.Depth() > 0 })
	r.cancel()
	drivertest.WaitFor(t, "the loop to stop", func() bool { return r.app.Alive(context.Background()) != nil })
	if r.app.HandleLocal(inbox.Message{Topic: topic("alarm", "event"), Payload: r.msg(contract.TypeAlarmTriggered, triggered)}) ||
		r.app.HandleCloud(inbox.Message{Topic: topic("alarm", "cmd"), Payload: r.msg(contract.TypeAlarmCommand,
			contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp})}) {
		t.Fatal("taken by a stopping agent")
	}
	r.app.LinkChanged(false, "")
	release()
	<-r.done
	if strings.Contains(r.logs.String(), "level=ERROR") {
		t.Fatal("an error was logged for a message given up at shutdown")
	}
	// The one taken before went to the cloud, or waits in the queue's store.
	env, _ := contract.ParseEnvelope(first)
	r.disk.queue.mu.Lock()
	_, kept := r.disk.queue.items[env.ID]
	r.disk.queue.mu.Unlock()
	if !kept && !slices.ContainsFunc(r.cloud.OfType(contract.TypeAlarmTriggered), func(m outboxtest.Message) bool { return m.Env.ID == env.ID }) {
		t.Fatal("a message taken before the stop was lost")
	}
}
