package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"safehome/internal/adapter/mqtt"
)

type recorder struct {
	mu   sync.Mutex
	sent []mqtt.Message
}

func (r *recorder) Publish(_ context.Context, m mqtt.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, m)
	return nil
}

func (r *recorder) last(t *testing.T) (mqtt.Message, map[string]any) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) == 0 {
		t.Fatal("nothing published")
	}
	m := r.sent[len(r.sent)-1]
	var body map[string]any
	json.Unmarshal(m.Payload, &body)
	return m, body
}

func newFake() (*bridge, *recorder) {
	r := &recorder{}
	return &bridge{base: "zigbee2mqtt", client: r, names: map[string]string{}, answers: true}, r
}

func ask(z *bridge, req, payload string) {
	z.handle(mqtt.Message{Topic: "zigbee2mqtt/bridge/request/" + req, Payload: []byte(payload)})
}

// Zigbee2MQTT 2.x echoes a request's transaction in its answer, an error or not: the
// adapter matches answers to its requests with it.
func TestAnswersCarryTheTransaction(t *testing.T) {
	z, r := newFake()
	ask(z, "permit_join", `{"value":true,"time":120,"transaction":"p1.1"}`)
	m, body := r.last(t)
	if m.Topic != "zigbee2mqtt/bridge/response/permit_join" || body["status"] != "ok" || body["transaction"] != "p1.1" {
		t.Fatalf("%s %v", m.Topic, body)
	}
	ask(z, "permit_join", `{"value":true,"time":300,"transaction":"p1.2"}`)
	if _, body := r.last(t); body["status"] != "error" || body["error"] != "Cannot permit join for more than 254 seconds." || body["transaction"] != "p1.2" {
		t.Fatalf("%v", body)
	}
	ask(z, "permit_join", `{"value":false,"time":0}`)
	if _, body := r.last(t); body["status"] != "ok" || body["transaction"] != nil {
		t.Fatalf("no transaction asked, none given: %v", body)
	}
	z.order([]string{"answers", "off"})
	before := len(r.sent)
	ask(z, "permit_join", `{"value":true,"time":120,"transaction":"p2.1"}`)
	if len(r.sent) != before {
		t.Fatal("a stuck Zigbee2MQTT answers nothing")
	}
}
