package agent

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"safehome/internal/contract"
)

func TestConfig(t *testing.T) {
	if err := DefaultConfig(site).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*Config){
		"site":    func(c *Config) { c.Site = "X" },
		"items":   func(c *Config) { c.QueueItems = 0 },
		"bytes":   func(c *Config) { c.QueueBytes = 10 },
		"age":     func(c *Config) { c.QueueAge = 0 },
		"confirm": func(c *Config) { c.ConfirmWait = 0 },
		"burst":   func(c *Config) { c.DownBurst = 0 },
		"rate":    func(c *Config) { c.DownPerMinute = 0 },
		"seen":    func(c *Config) { c.SeenSize = 0 },
		"journal": func(c *Config) { c.JournalItems = 0 },
		"jbytes":  func(c *Config) { c.JournalBytes = 10 },
		"economy": func(c *Config) { c.Economy = -time.Second },
	} {
		c := DefaultConfig(site)
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOnlyWhatTheContractListsGoesUp(t *testing.T) {
	h := newHarness(t)
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	h.local(topic("device", "dev_smoke", "event"), contract.TypeDeviceEvent, event("dev_smoke", contract.CapSmoke, contract.ValAlarm, ""))
	h.local(topic("device", "dev_hub", "event"), contract.TypeDeviceEvent, event("dev_hub", contract.CapMains, contract.ValLost, ""))
	h.local(topic("alert", "request"), contract.TypeAlertRequest, contract.AlertRequestData{Incident: "inc1", Kind: contract.AlertIntrusion,
		Urgency: contract.UrgencyCritical, Wave: 1, Contact: "u_guard", Channel: contract.ChannelSMS, Text: "ALARME"})
	h.local(topic("system", "health"), contract.TypeHealth, health)

	// What stays on the site.
	h.local(topic("device", "dev_door", "event"), contract.TypeDeviceEvent, event("dev_door", contract.CapContact, contract.ValOpen, ""))
	h.local(topic("device", "dev_keypad", "event"), contract.TypeDeviceEvent, event("dev_keypad", contract.CapKeypad, "disarm", "123456"))
	h.local(topic("device", "dev_x", "event"), contract.TypeDeviceEvent, event("dev_x", contract.CapPanic, contract.ValPressed, "123456"))
	h.local(topic("registry", "event"), contract.TypeRegistryDeviceAdded, contract.RegistryDeviceAddedData{DeviceID: "dev_a", Model: "m", Capabilities: []string{"contact"}})
	h.local(topic("gsm", "status"), contract.TypeGSMStatus, contract.GSMStatusData{})
	h.local(topic("alarm", "event"), contract.TypeHealth, health) // wrong type for the topic
	h.local(contract.Topic("tg-lom-0002", "alarm", "event"), contract.TypeAlarmArmed, armed)
	h.handle(Local{Now: h.now, Topic: topic("alarm", "event"), Payload: []byte("{oops")})

	want := []string{"alarm/event", "alarm/event", "device/dev_smoke/event", "device/dev_hub/event", "alert/request", "system/health"}
	if got := h.queued(); !reflect.DeepEqual(got, want) {
		t.Fatalf("queued %v\nwant %v", got, want)
	}
}

func TestOnlyTheLatestStateWaits(t *testing.T) {
	h := newHarness(t)
	for i := range 3 {
		h.local(topic("system", "health"), contract.TypeHealth, contract.HealthData{HubBattery: &i, Links: map[string]string{}})
		h.local(topic("system", "heartbeat"), contract.TypeHeartbeat, contract.HeartbeatData{BootID: "b", UptimeS: int64(i)})
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	q := h.r.queue
	if got := h.queued(); !reflect.DeepEqual(got, []string{"alarm/event", "alarm/event", "system/health", "system/heartbeat", "alarm/event"}) {
		t.Fatalf("queued %v", got)
	}
	var hb contract.Envelope
	json.Unmarshal(q[3].Payload, &hb)
	if d, _ := contract.DecodeData[contract.HeartbeatData](hb); d.UptimeS != 2 {
		t.Fatalf("the waiting heartbeat is not the latest: %+v", d)
	}
	if !q[2].Retain || q[3].Retain || q[0].Retain {
		t.Fatal("only the retained topics are retained on the cloud")
	}
}

func TestAMessageSeenTwiceWaitsOnce(t *testing.T) {
	h := newHarness(t)
	b, _ := h.msg(contract.TypeAlarmArmed, armed)
	h.handle(Local{Now: h.now, Topic: topic("alarm", "event"), Payload: b})
	h.handle(Local{Now: h.now, Topic: topic("alarm", "event"), Payload: b})
	if n := len(h.r.queue); n != 1 {
		t.Fatalf("%d items", n)
	}
}

func TestAlarmsLeaveFirstAndSentItemsLeaveTheQueue(t *testing.T) {
	h := newHarness(t)
	h.local(topic("system", "health"), contract.TypeHealth, health)
	h.local(topic("device", "dev_smoke", "event"), contract.TypeDeviceEvent, event("dev_smoke", contract.CapSmoke, contract.ValAlarm, ""))
	first, _ := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	second, _ := h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)

	var order []string
	for {
		it, ok := h.next()
		if !ok {
			break
		}
		order = append(order, it.Topic[len(topic()):])
		if len(order) == 1 && it.ID != first {
			t.Fatal("the first alarm message is not first")
		}
		if len(order) == 2 && it.ID != second {
			t.Fatal("alarm messages keep their order")
		}
		h.handle(Sent{Now: h.now, ID: it.ID})
	}
	if !reflect.DeepEqual(order, []string{"alarm/event", "alarm/event", "device/dev_smoke/event", "system/health"}) {
		t.Fatalf("order %v", order)
	}
}

func TestAFullQueueDropsTheLeastImportantFirst(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.QueueItems = 3 })
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.local(topic("system", "health"), contract.TypeHealth, health)
	h.local(topic("device", "dev_smoke", "event"), contract.TypeDeviceEvent, event("dev_smoke", contract.CapSmoke, contract.ValAlarm, ""))
	_, acts := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	if d := of[Dropped](acts); len(d) != 1 || d[0].Count != 1 {
		t.Fatalf("%+v", acts)
	}
	if got := h.queued(); !reflect.DeepEqual(got, []string{"alarm/event", "device/dev_smoke/event", "alarm/event"}) {
		t.Fatalf("the state should go first: %v", got)
	}
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	if got := h.queued(); !reflect.DeepEqual(got, []string{"alarm/event", "alarm/event", "alarm/event"}) {
		t.Fatalf("then the device event: %v", got)
	}
	h.local(topic("alarm", "event"), contract.TypeAlarmDisarmed, contract.DisarmedData{})
	if n := len(h.r.queue); n != 3 || h.r.Status(h.now).Dropped != 3 {
		t.Fatalf("then the oldest alarm message: %d items, %d dropped", n, h.r.Status(h.now).Dropped)
	}

	small := newHarness(t, func(c *Config) { c.QueueBytes = 1024 })
	for range 10 {
		small.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	}
	size := 0
	for _, it := range small.r.queue {
		size += len(it.Payload)
	}
	if size > 1024 || len(small.r.queue) == 0 {
		t.Fatalf("%d bytes in %d items", size, len(small.r.queue))
	}
}

func TestOldItemsLeaveTheQueue(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.QueueAge = time.Hour })
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.now = h.now.Add(30 * time.Minute)
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	if !h.r.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v", h.r.NextWake())
	}
	if st := h.r.Status(h.now); st.Queued != 2 || st.Oldest != 30*time.Minute {
		t.Fatalf("%+v", st)
	}
	acts := h.advance(30 * time.Minute)
	if d := of[Dropped](acts); len(d) != 1 || d[0].Count != 1 || d[0].Reason != "too old" || len(h.r.queue) != 1 {
		t.Fatalf("%+v %v", acts, h.queued())
	}
}

func TestAnAppAlertFailsAtOnceWithoutTheCloud(t *testing.T) {
	h := newHarness(t)
	id, acts := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != id {
		t.Fatalf("%+v", acts)
	}
	if len(h.r.queue) != 0 {
		t.Fatal("a failed alert must not wait: the escalation sends it again")
	}
	wa := appAlert("inc1")
	wa.Channel = contract.ChannelWhatsApp
	if _, acts := h.local(topic("alert", "request"), contract.TypeAlertRequest, wa); len(of[FailAlert](acts)) != 1 {
		t.Fatal("WhatsApp is a cloud channel too")
	}
}

func TestAnAppAlertTheCloudDoesNotConfirmFails(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	id, acts := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	if len(acts) != 1 || len(of[QueueSave](acts)) != 1 || len(h.r.queue) != 1 {
		t.Fatalf("%+v", acts)
	}
	if !h.r.NextWake().Equal(t0.Add(10 * time.Second)) {
		t.Fatalf("next wake %v", h.r.NextWake())
	}
	if acts := h.advance(10*time.Second - time.Millisecond); len(acts) != 0 {
		t.Fatalf("%+v", acts)
	}
	acts = h.advance(time.Millisecond)
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != id || len(h.r.queue) != 0 {
		t.Fatalf("%+v %v", acts, h.queued())
	}
	h.expectLate(id, contract.ResultSent)
}

// expectLate checks that the cloud's word on an alert the agent gave up on is
// dropped as late: the hub already turned to the GSM, and a late sent or delivered
// would disturb its escalation. It is no refusal either: the agent did send it.
func (h *harness) expectLate(ref, status string) {
	h.t.Helper()
	_, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: ref, Status: status})
	if l := of[Late](acts); len(l) != 1 || l[0].Ref != ref || l[0].Status != status || len(acts) != 1 {
		h.t.Fatalf("%+v: want only a late result for %s", acts, ref)
	}
}

func (h *harness) expectRefusedResult(ref string) {
	h.t.Helper()
	_, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: ref, Status: contract.ResultSent})
	if len(of[Refused](acts)) != 1 || len(acts) != 1 {
		h.t.Fatalf("%+v: want a refusal for %s", acts, ref)
	}
}

// The cloud restarted during an alert: its broker kept the request in its session,
// the agent gave up when the link dropped, and the cloud's results come back after.
func TestAResultAfterTheLinkDroppedIsLate(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	it, _ := h.next()
	h.handle(Sent{Now: h.now, ID: it.ID})
	if f := of[FailAlert](h.handle(Link{Now: h.now, Up: false})); len(f) != 1 || f[0].Ref != id {
		t.Fatal("the hub must turn to the GSM when the link drops")
	}
	h.now = h.now.Add(30 * time.Second)
	h.linkUp()
	h.expectLate(id, contract.ResultSent)
	h.expectLate(id, contract.ResultDelivered)
	h.expectRefusedResult("01SOMETHINGELSE0000000000") // never sent: refused as before
}

// The memory of an alert given up is bounded like the rest: kept across a restart,
// forgotten when the alert would have been.
func TestAnAlertGivenUpIsForgottenAfterAnHour(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	h.advance(10 * time.Second)
	if !h.r.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v", h.r.NextWake())
	}
	h.restart()
	h.linkUp()
	h.expectLate(id, contract.ResultQueued)
	h.advance(time.Hour - 10*time.Second)
	if len(h.r.Snapshot().GivenUp) != 0 {
		t.Fatal("still remembered")
	}
	h.expectRefusedResult(id)
}

func TestTheCloudConfirmsAndDeliversAnAppAlert(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	it, _ := h.next()
	h.handle(Sent{Now: h.now, ID: it.ID})

	_, raw, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultSent})
	if p := of[PublishLocal](acts); len(p) != 1 || p[0].Topic != topic("alert", "result") || string(p[0].Payload) != string(raw) {
		t.Fatalf("%+v", acts)
	}
	if acts := h.advance(time.Minute); len(of[FailAlert](acts)) != 0 {
		t.Fatal("a confirmed alert failed")
	}
	if _, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultDelivered}); len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	if len(h.r.Snapshot().Pending) != 0 {
		t.Fatal("a delivered alert is still followed")
	}
	if _, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: "01SOMETHINGELSE0000000000", Status: contract.ResultSent}); len(of[Refused](acts)) != 1 {
		t.Fatal("a result for an unknown alert was accepted")
	}
}

// An alert the hub waits for does not queue behind a backlog of alarm messages: the
// cloud has 10 s to confirm it, or the hub turns to the GSM for nothing.
func TestAnAppAlertLeavesBeforeABacklogOfAlarms(t *testing.T) {
	h := newHarness(t)
	for range 50 {
		h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	}
	h.linkUp()
	tried, _ := h.next() // the cloud's broker did not acknowledge it
	sms := appAlert("inc1")
	sms.Channel = contract.ChannelSMS // the GSM's: no hurry, it goes up for the record
	record, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, sms)
	awaited, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	it, _ := h.next()
	if it.ID != awaited {
		t.Fatalf("sent %s first, want the awaited alert", it.Topic)
	}
	h.handle(Sent{Now: h.now, ID: it.ID})
	sent := h.send()
	if len(sent) != 51 || sent[0].ID != tried.ID || sent[0].Seq != tried.Seq || sent[50].ID != record {
		t.Fatalf("%d sent; the one tried keeps its number, the SMS alert its place in arrival order", len(sent))
	}
}

// A message the cloud's broker refuses for good leaves the queue, counted as dropped:
// tried again, it would hold everything behind it for days.
func TestAMessageTheCloudsBrokerRefusesIsDropped(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	refused, _ := h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	after, _ := h.local(topic("alarm", "event"), contract.TypeAlarmDisarmed, contract.DisarmedData{})
	alert, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	h.next() // the awaited alert goes first
	acts := h.handle(Rejected{Now: h.now, ID: alert})
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != alert {
		t.Fatalf("%+v: the hub must turn to the GSM now", acts)
	}
	h.next()
	acts = h.handle(Rejected{Now: h.now, ID: refused})
	if d := of[Dropped](acts); len(d) != 1 || d[0].Count != 1 || len(of[QueueDrop](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	if it, ok := h.next(); !ok || it.ID != after || h.r.Status(h.now).Dropped != 2 {
		t.Fatalf("next %s, %d dropped", it.ID, h.r.Status(h.now).Dropped)
	}
}

func TestAConfirmedAlertIsForgottenAfterAnHour(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: contract.ResultQueued})
	if !h.r.NextWake().Equal(t0.Add(time.Hour)) {
		t.Fatalf("next wake %v", h.r.NextWake())
	}
	h.advance(time.Hour)
	if len(h.r.Snapshot().Pending) != 0 {
		t.Fatal("still followed")
	}
	if _, _, acts := h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: id, Status: "weird"}); len(of[Refused](acts)) != 1 {
		t.Fatal("accepted")
	}
}

func TestLosingTheLinkFailsTheUnconfirmedAlerts(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	a, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	b, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	h.cloud(topic("alert", "result"), contract.TypeAlertResult, contract.AlertResultData{Ref: b, Status: contract.ResultSent})
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)

	acts := h.handle(Link{Now: h.now, Up: false})
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != a {
		t.Fatalf("%+v", acts)
	}
	if s := of[StatusChanged](acts); len(s) != 1 || s[0].Status.Up || s[0].Status.Transport != "" {
		t.Fatalf("%+v", acts)
	}
	if got := h.queued(); !reflect.DeepEqual(got, []string{"alert/request", "alarm/event"}) {
		t.Fatalf("the confirmed one and the event still wait: %v", got)
	}
	if acts := h.handle(Link{Now: h.now, Up: false}); len(acts) != 0 {
		t.Fatalf("the same link twice: %+v", acts)
	}
	acts = h.handle(Link{Now: h.now, Up: true, Transport: "wss"})
	if s := of[StatusChanged](acts); len(s) != 1 || !s[0].Status.Up || s[0].Status.Transport != "wss" || s[0].Status.Queued != 2 {
		t.Fatalf("%+v", acts)
	}
}

func TestADroppedAppAlertFails(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.QueueItems = 1 })
	h.linkUp()
	id, _ := h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	_, acts := h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	if f := of[FailAlert](acts); len(f) != 1 || f[0].Ref != id {
		t.Fatalf("%+v", acts)
	}
}

func TestOrdersFromTheCloud(t *testing.T) {
	h := newHarness(t)
	cmd := func(origin string, exp *contract.Timestamp) contract.AlarmCommandData {
		return contract.AlarmCommandData{Command: contract.CmdArm, Origin: origin, User: "u_owner", Exp: exp}
	}
	later := contract.TS(t0.Add(time.Minute))
	_, raw, acts := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, cmd(contract.OriginApp, &later))
	if p := of[PublishLocal](acts); len(p) != 1 || string(p[0].Payload) != string(raw) {
		t.Fatalf("%+v", acts)
	}
	// The same order again (QoS 1): passed on once.
	if acts := h.handle(Cloud{Now: h.now, Topic: topic("alarm", "cmd"), Payload: raw}); len(acts) != 0 {
		t.Fatalf("%+v", acts)
	}

	refused := map[string]func() []Action{
		"keypad origin": func() []Action {
			_, _, a := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, cmd(contract.OriginKeypad, nil))
			return a
		},
		"wrong type": func() []Action {
			_, _, a := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmState, contract.AlarmStateData{})
			return a
		},
		"siren": func() []Action {
			_, _, a := h.cloud(topic("device", "dev_siren1", "cmd"), contract.TypeCommand, map[string]any{"command": "siren.on"})
			return a
		},
		"pairing": func() []Action {
			_, _, a := h.cloud(topic("registry", "cmd"), contract.TypeRegistryPair, contract.RegistryPairData{DurationS: 60})
			return a
		},
		"config of another type": func() []Action { _, _, a := h.cloud(topic("config", "set"), contract.TypeHealth, health); return a },
		"video stop without session": func() []Action {
			_, _, a := h.cloud(topic("video", "cmd"), contract.TypeVideoStop, contract.VideoStopData{})
			return a
		},
		"config without version": func() []Action {
			_, _, a := h.cloud(topic("config", "set"), contract.TypeConfigSet, contract.ConfigSetData{EntryDelayS: 30})
			return a
		},
		"other site": func() []Action {
			_, _, a := h.cloud(contract.Topic("tg-lom-0002", "alarm", "cmd"), contract.TypeAlarmCommand, cmd(contract.OriginApp, nil))
			return a
		},
		"garbage": func() []Action {
			return h.handle(Cloud{Now: h.now, Topic: topic("alarm", "cmd"), Payload: []byte("[]")})
		},
	}
	for name, send := range refused {
		acts := send()
		if r := of[Refused](acts); len(r) != 1 || len(of[PublishLocal](acts)) != 0 {
			t.Errorf("%s: %+v", name, acts)
		}
	}
	_, _, acts = h.cloud(topic("config", "set"), contract.TypeConfigSet, contract.ConfigSetData{Version: 3, EntryDelayS: 30})
	if len(of[PublishLocal](acts)) != 1 {
		t.Errorf("a configuration passes: %+v", acts)
	}
}

func TestVideoOrdersFromTheCloud(t *testing.T) {
	h := newHarness(t)
	req := contract.VideoRequestData{Session: "s1", Camera: "cam_caisse", Reason: contract.VideoOnDemand, User: "u_owner",
		PushURL: "rtmps://relay.safehome.example/live/s1", Token: "secret-token", Exp: contract.TS(t0.Add(time.Minute))}
	if _, _, acts := h.cloud(topic("video", "cmd"), contract.TypeVideoRequest, req); len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	if _, _, acts := h.cloud(topic("video", "cmd"), contract.TypeVideoStop, contract.VideoStopData{Session: "s1"}); len(of[PublishLocal](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
	old := req
	old.Exp = contract.TS(t0)
	if _, _, acts := h.cloud(topic("video", "cmd"), contract.TypeVideoRequest, old); len(of[Refused](acts)) != 1 {
		t.Fatal("an expired video request passed")
	}
	bad := req
	bad.PushURL = "rtmp://relay.example/live"
	if _, _, acts := h.cloud(topic("video", "cmd"), contract.TypeVideoRequest, bad); len(of[Refused](acts)) != 1 {
		t.Fatal("a plain http push URL passed")
	}
}

func TestAcknowledgementsFromTheApp(t *testing.T) {
	h := newHarness(t)
	ack := func(incident, via string) []Action {
		_, _, a := h.cloud(topic("alert", "ack"), contract.TypeAlertAck, contract.AlertAckData{Incident: incident, Contact: "u_owner", Via: via})
		return a
	}
	if len(of[Refused](ack("inc1", contract.ChannelApp))) != 1 {
		t.Fatal("an ack for an incident the cloud never heard of passed")
	}
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered) // incident inc1 goes up
	if len(of[PublishLocal](ack("inc1", contract.ChannelApp))) != 1 {
		t.Fatal("the app's ack was refused")
	}
	if len(of[Refused](ack("inc1", contract.ChannelSMS))) != 1 {
		t.Fatal("an SMS ack from the cloud passed: SMS acks come from the GSM gateway")
	}
	h.advance(24 * time.Hour)
	if len(of[Refused](ack("inc1", contract.ChannelApp))) != 1 {
		t.Fatal("an ack a day later passed")
	}
}

func TestTheCloudCannotFloodTheHub(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DownBurst = 3; c.DownPerMinute = 6 })
	order := contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp}
	passed := func() int {
		_, _, a := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
		return len(of[PublishLocal](a))
	}
	n := 0
	for range 5 {
		n += passed()
	}
	if n != 3 {
		t.Fatalf("%d orders passed at once, want the burst of 3", n)
	}
	h.now = h.now.Add(10 * time.Second) // one more allowed (6 a minute)
	if passed() != 1 || passed() != 0 {
		t.Fatal("the allowance does not refill at the right pace")
	}
	h.now = h.now.Add(time.Hour)
	n = 0
	for range 5 {
		n += passed()
	}
	if n != 3 {
		t.Fatalf("after a long calm, %d passed, want the burst of 3", n)
	}
}

func TestDuplicatesAreRememberedWithinBounds(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SeenSize = 2; c.DownBurst = 100 })
	order := contract.AlarmCommandData{Command: contract.CmdArm, Origin: contract.OriginApp}
	_, first, _ := h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
	h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
	h.cloud(topic("alarm", "cmd"), contract.TypeAlarmCommand, order)
	if len(h.r.Snapshot().Seen) != 2 {
		t.Fatalf("%d ids remembered", len(h.r.Snapshot().Seen))
	}
	if acts := h.handle(Cloud{Now: h.now, Topic: topic("alarm", "cmd"), Payload: first}); len(of[PublishLocal](acts)) != 1 {
		t.Fatal("the oldest id should be forgotten")
	}
}

func TestTheQueueSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	h.local(topic("alarm", "event"), contract.TypeAlarmTriggered, triggered)
	h.local(topic("alert", "request"), contract.TypeAlertRequest, appAlert("inc1"))
	q1 := slices.Clone(h.r.queue)
	h.restart()
	q2 := h.r.queue
	if len(q2) != 2 || q2[0].ID != q1[0].ID || q2[1].ID != q1[1].ID || q2[0].Prio != PriorityAlarm || !q2[0].At.Equal(q1[0].At) {
		t.Fatalf("%+v", q2)
	}
	if h.r.Up() {
		t.Fatal("a restored relay must wait for the link")
	}
	// The unconfirmed alert still fails at its deadline.
	if acts := h.handle(Tick{Now: t0.Add(10 * time.Second)}); len(of[FailAlert](acts)) != 1 {
		t.Fatalf("%+v", acts)
	}
}

func TestOwnMessagesGoUpLikeTheOthers(t *testing.T) {
	h := newHarness(t)
	b, _ := h.msg(contract.TypeAgentStatus, contract.AgentStatusData{Uplink: contract.UplinkDown})
	h.handle(Own{Now: h.now, Topic: topic("agent", "status"), Payload: b})
	b, _ = h.msg(contract.TypePhotoSent, contract.PhotoSentData{Photo: "01JB8ZQ7V2T4X9H6N5R0WYCE00", Status: contract.PhotoSent})
	h.handle(Own{Now: h.now, Topic: topic("photo", "sent"), Payload: b})
	if got := h.queued(); !reflect.DeepEqual(got, []string{"agent/status", "photo/sent"}) {
		t.Fatalf("%v", got)
	}
}

// The hub's broker gives the retained messages again at each new connection of the
// agent. An old state sent again would take a new number, which the cloud drops as a
// duplicate of its id: a hole that never closes.
func TestARetainedStateGivenBackDoesNotGoUpAgain(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.QueueAge = time.Hour })
	state, _ := h.msg(contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed})
	given := func() Input { return Local{Now: h.now, Topic: topic("alarm", "state"), Payload: state} }
	h.handle(given())
	h.linkUp()
	if len(h.send()) != 1 {
		t.Fatal("the state went up")
	}
	h.handle(given())
	h.restart()
	h.handle(given())
	if len(h.r.queue) != 0 {
		t.Fatalf("the same state given back waits again: %v", h.queued())
	}
	// A state that changed while the agent was away goes.
	changed, _ := h.msg(contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateDisarmed})
	h.handle(Local{Now: h.now, Topic: topic("alarm", "state"), Payload: changed})
	if len(h.r.queue) != 1 {
		t.Fatal("a new state must go")
	}
	// A state that never reached the cloud goes if the broker gives it back.
	h.handle(Link{Now: h.now, Up: false})
	if acts := h.advance(time.Hour); len(of[Dropped](acts)) != 1 {
		t.Fatalf("%+v: dropped as too old", acts)
	}
	h.handle(Local{Now: h.now, Topic: topic("alarm", "state"), Payload: changed})
	if len(h.r.queue) != 1 {
		t.Fatalf("%v", h.queued())
	}
}

// The agent's own status, retained on the hub's broker, comes back at each new
// connection, even with NoLocal: it is not the hub's, and the latest goes already.
func TestTheAgentsOwnStatusGivenBackIsIgnored(t *testing.T) {
	h := newHarness(t)
	b, _ := h.msg(contract.TypeAgentStatus, contract.AgentStatusData{Uplink: contract.UplinkUp})
	h.handle(Local{Now: h.now, Topic: topic("agent", "status"), Payload: b})
	if len(h.r.queue) != 0 {
		t.Fatal("an old status of the agent went up")
	}
}

func TestFilters(t *testing.T) {
	up := UpFilters(site)
	for _, want := range []string{topic("alarm", "event"), topic("device", "+", "event"), topic("system", "heartbeat"), topic("system", "health")} {
		if !slices.Contains(up, want) {
			t.Errorf("up filters miss %s: %v", want, up)
		}
	}
	if !slices.Contains(up, topic("install", "mode")) {
		t.Errorf("up filters miss install/mode: the owner would not see the installation mode")
	}
	for _, never := range []string{topic("registry", "cmd"), topic("device", "+", "cmd"), topic("alarm", "cmd"), topic("install", "cmd"),
		topic("agent", "status"), topic("agent", "replayed")} { // the agent's own
		if slices.Contains(up, never) {
			t.Errorf("up filters include %s", never)
		}
	}
	// alarm/cmd at QoS 0: the cloud's broker keeps no order with the access code for
	// an agent away. The rest at QoS 1, kept until the agent is back.
	down := DownFilters(site)
	want := []Filter{
		{topic("agent", "cmd"), 1}, {topic("alarm", "cmd"), 0}, {topic("alert", "ack"), 1}, {topic("alert", "result"), 1},
		{topic("config", "set"), 1}, {topic("maint", "cmd"), 1}, {topic("photo", "cmd"), 1}, {topic("video", "cmd"), 1},
	}
	if !reflect.DeepEqual(down, want) {
		t.Errorf("down filters %v", down)
	}
}
