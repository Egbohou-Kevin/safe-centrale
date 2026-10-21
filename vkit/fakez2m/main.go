// Command fakez2m is the Zigbee2MQTT of the virtual kit (docs/kit-virtuel.md): it
// speaks on the broker as Zigbee2MQTT does, without a Zigbee stick, so the real
// safehome-zigbee runs unchanged. Devices join, report and leave on orders from a
// named pipe; requests (permit join, rename, remove) are answered; commands sent to a
// device (a siren) are printed.
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
	z := &bridge{base: *base, names: map[string]string{}, requests: make(chan mqtt.Message, 64)}
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
	client   *mqtt.Client
	requests chan mqtt.Message // what the broker hands over, served one at a time

	mu    sync.Mutex
	names map[string]string // ieee -> friendly name
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

// answer does what a request asks and replies on bridge/response/, as Zigbee2MQTT
// does.
func (z *bridge) answer(req string, payload []byte) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
		ID   string `json:"id"`
	}
	json.Unmarshal(payload, &body)
	z.mu.Lock()
	switch req {
	case "device/rename":
		z.names[body.From] = body.To
	case "device/remove":
		delete(z.names, body.ID)
	}
	z.mu.Unlock()
	log.Printf("request %s %s", req, payload)
	z.publish("bridge/response/"+req, map[string]any{"status": "ok", "data": json.RawMessage(payload)}, false)
}

func (z *bridge) publish(topic string, body any, retain bool) {
	b, err := json.Marshal(body)
	if err != nil {
		log.Printf("%s: %v", topic, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := mqtt.Message{Topic: z.base + "/" + topic, Payload: b, QoS: 1, Retain: retain}
	if err := z.client.Publish(ctx, m); err != nil {
		log.Printf("publishing on %s: %v", m.Topic, err)
	}
}
