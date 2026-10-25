package agent

import (
	"testing"

	"safehome/internal/contract"
)

// The hub's broker gives messages again after they reached the cloud: after a power
// cut, Mosquitto brings back what it held for the agent at its last save. They are not
// queued again (each would take a new number, a hole the cloud cannot close), after a
// restart of the agent too: a photo announced again is not followed again, and an
// alert request given again waits for no confirmation.
func TestAMessageGivenAgainAfterItReachedTheCloudIsDropped(t *testing.T) {
	h := newHarness(t)
	h.linkUp()
	given := map[string][]byte{}
	for tp, m := range map[string]struct {
		typ  string
		data any
	}{
		topic("alarm", "event"): {contract.TypeAlarmTriggered, triggered},
		topic("photo", "taken"): {contract.TypePhotoTaken, contract.PhotoTakenData{Photo: "p1", Incident: "inc1", Camera: "cam_caisse", Bytes: 40_000, Width: 640}},
		topic("alert", "request"): {contract.TypeAlertRequest, contract.AlertRequestData{Incident: "inc1", Kind: contract.AlertIntrusion,
			Urgency: contract.UrgencyCritical, Wave: 1, Contact: "u_owner", Channel: contract.ChannelApp, Text: "ALARME"}},
	} {
		given[tp], _ = h.msg(m.typ, m.data)
	}
	giveAgain := func() []Action {
		var out []Action
		for tp, b := range given {
			out = append(out, h.handle(Local{Now: h.now, Topic: tp, Payload: b})...)
		}
		return out
	}
	giveAgain()
	if sent := h.send(); len(sent) != 3 {
		t.Fatalf("%d sent", len(sent))
	}
	delete(h.r.s.Photos, "p1") // uploaded since, and forgotten
	delete(h.r.s.Pending, string(must(contract.ParseEnvelope(given[topic("alert", "request")])).ID))
	if acts := giveAgain(); len(acts) != 0 || len(h.r.queue) != 0 || len(h.r.s.Photos) != 0 || len(h.r.s.Pending) != 0 {
		t.Fatalf("given again: %+v, queue %v", acts, h.queued())
	}
	h.restart()
	h.linkUp()
	if acts := giveAgain(); len(acts) != 0 || len(h.r.queue) != 0 {
		t.Fatalf("given again after a restart: %+v, queue %v", acts, h.queued())
	}
}
