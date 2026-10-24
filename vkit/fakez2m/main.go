// Command fakez2m is the Zigbee2MQTT of the virtual kit (docs/kit-virtuel.md): it
// speaks on the broker as Zigbee2MQTT does, without a Zigbee stick, so the real
// safehome-zigbee runs unchanged. Devices join, report and leave on orders from a
// named pipe; requests (permit join, rename, remove, device options) are answered
// with their transaction, as Zigbee2MQTT 2.x does; commands sent to a device (a
// siren) are printed. A device given the option qos 0 (a keypad) reports at QoS 0,
// the others at QoS 1 (device_options of deploy/zigbee2mqtt/configuration.yaml).
//
//	fakez2m -broker mqtt://127.0.0.1:1883 -control /run/vkit/zigbee
//	echo 'pair MCCGQ11LM 0x00158d0000000001' > /run/vkit/zigbee
//	echo 'send 0x00158d0000000001 {"contact":false}' > /run/vkit/zigbee
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"safehome/internal/adapter/mqtt"
	"safehome/vkit/order"
)

func main() {
	broker := flag.String("broker", "mqtt://127.0.0.1:1883", "the hub's broker")
	base := flag.String("base", "zigbee2mqtt", "the base topic, as in zigbee.json")
	control := flag.String("control", "/run/vkit/zigbee", "the named pipe that takes the orders")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	z := &bridge{base: *base, names: map[string]string{}, qos: map[string]byte{}, answers: true, requests: make(chan mqtt.Message, 64)}
	client, err := mqtt.Dial(ctx, mqtt.Options{
		BrokerURL:     *broker,
		ClientID:      "vkit-fakez2m",
		Subscriptions: []mqtt.Subscription{{Filter: *base + "/#", QoS: 1}},
		OnMessage:     z.received,
	})
	if err != nil {
		log.Fatal(err)
	}
	z.client = client
	if err := client.Await(ctx); err != nil {
		log.Fatal(err)
	}
	if err := order.Listen(*control, z.order); err != nil {
		log.Fatal(err)
	}
	z.publish("bridge/state", map[string]string{"state": "online"}, true)
	log.Print("Zigbee2MQTT (fake) online")
	z.serve(ctx)
}

// bridge is the fake Zigbee2MQTT: its devices are known by their Zigbee address,
// and each has the name our adapter gave it.
type bridge struct {
	base     string
	client   publisher
	requests chan mqtt.Message // what the broker hands over, served one at a time

	mu      sync.Mutex
	names   map[string]string // ieee -> friendly name
	qos     map[string]byte   // ieee -> the QoS of its messages, when set by device options
	answers bool              // false: requests go unanswered, as with a Zigbee2MQTT stuck
}

// publisher is the MQTT client, as the fake uses it.
type publisher interface {
	Publish(ctx context.Context, m mqtt.Message) error
}

// received hands a message over to serve: the client's goroutine must not publish.
func (z *bridge) received(m mqtt.Message) {
	select {
	case z.requests <- m:
	default:
		log.Printf("dropped %s: too many at once", m.Topic)
	}
}

// serve answers the requests and prints the commands sent to the devices.
func (z *bridge) serve(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-z.requests:
			z.handle(m)
		}
	}
}

func (z *bridge) handle(m mqtt.Message) {
	rest := strings.TrimPrefix(m.Topic, z.base+"/")
	if req, ok := strings.CutPrefix(rest, "bridge/request/"); ok {
		z.answer(req, m.Payload)
		return
	}
	if name, ok := strings.CutSuffix(rest, "/set"); ok {
		log.Printf("SET %s %s", name, m.Payload)
	}
}

// request is what the fake reads of a request.
type request struct {
	From        string         `json:"from"`
	To          string         `json:"to"`
	ID          string         `json:"id"`
	Time        int            `json:"time"`
	Options     map[string]any `json:"options"`
	Transaction any            `json:"transaction"`
}

// answer does what a request asks and replies on bridge/response/, as Zigbee2MQTT
// does: the request's transaction comes back in the answer, an error or not
// (lib/util/utils.ts, getResponse).
func (z *bridge) answer(req string, payload []byte) {
	var body request
	json.Unmarshal(payload, &body)
	z.mu.Lock()
	answers := z.answers
	z.mu.Unlock()
	if !answers {
		log.Printf("request %s %s left unanswered", req, payload)
		return
	}
	reply := func(data any, failure string) {
		r := map[string]any{"status": "ok", "data": data}
		if failure != "" {
			r["status"], r["error"] = "error", failure
		}
		if body.Transaction != nil {
			r["transaction"] = body.Transaction
		}
		z.publish("bridge/response/"+req, r, false)
	}
	if req == "permit_join" && body.Time > 254 {
		// Zigbee2MQTT 2.14.2's own answer, seen on the real hardware.
		log.Printf("request %s %s refused", req, payload)
		reply(map[string]any{}, "Cannot permit join for more than 254 seconds.")
		return
	}
	log.Printf("request %s %s", req, payload)
	reply(z.apply(req, body, payload), "")
}

// apply does what a request asks, and returns the data of the answer.
func (z *bridge) apply(req string, body request, payload []byte) any {
	z.mu.Lock()
	defer z.mu.Unlock()
	switch req {
	case "device/rename":
		z.names[body.From] = body.To
	case "device/remove":
		delete(z.names, body.ID)
	case "device/options":
		// Only qos plays a part here; the device is named by its address.
		if q, ok := body.Options["qos"].(float64); ok {
			z.qos[body.ID] = byte(q)
		}
		return map[string]any{"id": body.ID, "from": map[string]any{}, "to": body.Options, "restart_required": false}
	}
	return json.RawMessage(payload)
}

func (z *bridge) publish(topic string, body any, retain bool) {
	z.publishQoS(topic, body, 1, retain)
}

func (z *bridge) publishQoS(topic string, body any, qos byte, retain bool) {
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("%s: %v", topic, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := mqtt.Message{Topic: z.base + "/" + topic, Payload: b, QoS: qos, Retain: retain}
	if err := z.client.Publish(ctx, m); err != nil {
		log.Printf("publishing on %s: %v", m.Topic, err)
	}
}
