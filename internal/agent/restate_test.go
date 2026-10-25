package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"safehome/internal/contract"
)

// restated plays the application: each state to say again goes back to the relay
// as a new message (a new id), and their topics are returned in order.
func (h *harness) restated(acts []Action) []string {
	h.t.Helper()
	var topics []string
	for _, a := range of[Restate](acts) {
		env, err := contract.ParseEnvelope(a.Payload)
		if err != nil {
			h.t.Fatal(err)
		}
		env.ID = h.ids.New()
		b, _ := json.Marshal(env)
		h.handle(Own{Now: h.now, Topic: a.Topic, Payload: b})
		topics = append(topics, a.Topic[len(topic()):])
	}
	return topics
}

// The cloud may have lost the hub's state (its database reset, or it did not keep
// states yet): at each new connection, it gets the current one again, as a new message.
// That of a ringing alarm goes whole, the first time and said again: its incident,
// where its siren stands and until when, as the hub said them.
func TestTheCurrentStateIsSaidAgainAtEachConnection(t *testing.T) {
	h := newHarness(t)
	state, stateID := h.msg(contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateTriggered,
		Since: contract.TS(h.now), EndsAt: contract.TSPtr(h.now.Add(2 * time.Minute)),
		Incident: "inc1", Siren: contract.SirenPaused, Cycle: 1, Of: 3})
	h.handle(Local{Now: h.now, Topic: topic("alarm", "state"), Payload: state})
	healthID, _ := h.local(topic("system", "health"), contract.TypeHealth, health)
	h.linkUp()
	if first := h.send(); len(first) != 2 || first[0].ID != stateID || !bytes.Equal(mustData(t, first[0].Payload), mustData(t, state)) {
		t.Fatalf("the hub's state must go up as it is: %+v", first)
	}

	h.handle(Link{Now: h.now, Up: false})
	acts := h.handle(Link{Now: h.now, Up: true, Transport: "wss"})
	if got := h.restated(acts); !reflect.DeepEqual(got, []string{"alarm/state", "system/health"}) {
		t.Fatalf("said again: %v", got)
	}
	got := h.send()
	if len(got) != 2 {
		t.Fatalf("%d sent", len(got))
	}
	for _, it := range got {
		env, _ := contract.ParseEnvelope(it.Payload)
		if it.ID == stateID || it.ID == healthID || !it.Retain {
			t.Fatalf("%s: a new id, retained like the hub's, is needed", it.Topic)
		}
		if it.Topic == topic("alarm", "state") && !bytes.Equal(env.Data, mustData(t, state)) {
			t.Fatalf("the state said again is not the hub's: %s", env.Data)
		}
	}
	if seqOf(t, got[0]) != 3 || seqOf(t, got[1]) != 4 {
		t.Fatal("said again, a state takes the next numbers")
	}

	// Once per connection: the same link said again is no new connection.
	if acts := h.handle(Link{Now: h.now, Up: true, Transport: "wss"}); len(of[Restate](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
}

func mustData(t *testing.T, payload []byte) json.RawMessage {
	t.Helper()
	env, err := contract.ParseEnvelope(payload)
	if err != nil {
		t.Fatal(err)
	}
	return env.Data
}

// A state still waiting for the cloud goes as it is: it is the current one.
func TestAStateWaitingForTheCloudIsNotSaidTwice(t *testing.T) {
	h := newHarness(t)
	h.local(topic("alarm", "state"), contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed})
	h.local(topic("system", "health"), contract.TypeHealth, health)
	h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"})
	if got := h.queued(); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	acts := h.handle(Link{Now: h.now, Up: false})
	acts = append(acts, h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"})...)
	if len(of[Restate](acts)) != 0 {
		t.Fatalf("%+v", acts)
	}
}

// After a restart (an update of the agent), the relay knows the states only once the
// hub's broker gives them back, which it may do after the cloud's link came up: they
// are said again then, once.
func TestAfterARestartTheStateGivenBackIsSaidAgain(t *testing.T) {
	h := newHarness(t)
	state, _ := h.msg(contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed})
	given := func() []Action { return h.handle(Local{Now: h.now, Topic: topic("alarm", "state"), Payload: state}) }
	given()
	h.linkUp()
	h.send()

	h.restart()
	if acts := h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"}); len(of[Restate](acts)) != 0 {
		t.Fatalf("nothing known yet: %+v", acts)
	}
	if got := h.restated(given()); !reflect.DeepEqual(got, []string{"alarm/state"}) {
		t.Fatalf("said again: %v", got)
	}
	if len(of[Restate](given())) != 0 {
		t.Fatal("said twice on one connection")
	}
	h.send()

	// Given back before the link comes up, it is said again when it comes.
	h.restart()
	given()
	if got := h.restated(h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"})); !reflect.DeepEqual(got, []string{"alarm/state"}) {
		t.Fatalf("said again: %v", got)
	}
}

// A flapping link (or two agents on one client id, a cloned SD card, kicking each
// other off) connects again and again: a state is said again at most once a minute,
// not at every connection.
func TestAFlappingLinkSaysTheStateAgainAtMostOnceAMinute(t *testing.T) {
	h := newHarness(t)
	h.local(topic("alarm", "state"), contract.TypeAlarmState, contract.AlarmStateData{State: contract.StateArmed})
	h.linkUp()
	h.send()
	reconnect := func(after time.Duration) []string {
		h.now = h.now.Add(after)
		h.handle(Link{Now: h.now, Up: false})
		got := h.restated(h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"}))
		h.send()
		return got
	}
	if got := reconnect(time.Second); !reflect.DeepEqual(got, []string{"alarm/state"}) {
		t.Fatalf("said again: %v", got)
	}
	for range 5 {
		if got := reconnect(10 * time.Second); len(got) != 0 {
			t.Fatalf("said again %v within a minute", got)
		}
	}
	if got := reconnect(10 * time.Second); !reflect.DeepEqual(got, []string{"alarm/state"}) {
		t.Fatalf("a minute later, said again: %v", got)
	}
}

// The device list goes up like the health: a retained state, after the alarm's
// messages, and said again at each new connection. Said on another topic, it stays
// here.
func TestTheDeviceListGoesUpLikeTheHealth(t *testing.T) {
	h := newHarness(t)
	list := contract.DevicesData{Devices: []contract.DeviceHealth{{ID: "dev_back", Type: contract.CapContact, Online: true}}}
	listID, _ := h.local(topic("system", "devices"), contract.TypeDevices, list)
	h.local(topic("alarm", "event"), contract.TypeAlarmArmed, armed)
	h.local(topic("system", "health"), contract.TypeDevices, list)
	if got := h.queued(); !reflect.DeepEqual(got, []string{"system/devices", "alarm/event"}) {
		t.Fatalf("queued %v", got)
	}
	if it, ok := h.next(); !ok || it.Topic != topic("alarm", "event") {
		t.Fatalf("the alarm goes first: %+v", it)
	}
	h.linkUp()
	sent := h.send()
	if len(sent) != 2 || sent[1].ID != listID || !sent[1].Retain {
		t.Fatalf("%+v", sent)
	}

	h.handle(Link{Now: h.now, Up: false})
	if got := h.restated(h.handle(Link{Now: h.now, Up: true, Transport: "mqtts"})); !reflect.DeepEqual(got, []string{"system/devices"}) {
		t.Fatalf("said again: %v", got)
	}
}
