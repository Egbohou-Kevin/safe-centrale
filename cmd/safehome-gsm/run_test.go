package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"safehome/internal/adapter/atmodem"
	"safehome/internal/adapter/atmodem/attest"
	"safehome/internal/adapter/mqtt"
	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/adapter/pidfile"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/gateway"
	"safehome/internal/gsm"
	"safehome/internal/gsmconfig"
	"safehome/internal/schematest"
	"safehome/internal/siteconfig"
)

const site = "tg-lom-0001"

func writeConfig(t *testing.T, broker, state string) string {
	t.Helper()
	body := fmt.Sprintf(`{
  "site": %q, "broker_url": %q, "client_id": "gsm-test-%d", "state_dir": %q, "log_level": "debug",
  "lines": [{"id": "line1", "port": "/dev/sim", "configure_port": false}],
  "contacts": {"u_owner": "90 12 34 56"},
  "check_every": "1s"
}`, site, broker, time.Now().UnixNano(), state)
	path := filepath.Join(t.TempDir(), "gsm.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cli(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageVersionAndCheck(t *testing.T) {
	if code, out, _ := cli("version"); code != 0 || !strings.Contains(out, "safehome-gsm") {
		t.Fatalf("%d %q", code, out)
	}
	for _, args := range [][]string{{}, {"fly"}, {"run"}, {"check", "-nonsense"}} {
		if code, _, errb := cli(args...); code != 2 || errb == "" {
			t.Errorf("%v: %d %q", args, code, errb)
		}
	}
	path := writeConfig(t, "mqtt://127.0.0.1:1", t.TempDir())
	code, out, _ := cli("check", "-config", path)
	if code != 0 || !strings.Contains(out, "1 line(s), 1 contact(s)") || !strings.Contains(out, "no test number") {
		t.Fatalf("%d %q", code, out)
	}
	if code, _, errb := cli("check", "-config", filepath.Join(t.TempDir(), "none.json")); code != 1 || !strings.Contains(errb, "configuration error") {
		t.Fatalf("%d %q", code, errb)
	}
}

func TestTheConfigExampleChecks(t *testing.T) {
	if code, out, errb := cli("check", "-config", filepath.Join("..", "..", "config", "gsm.example.json")); code != 0 {
		t.Fatalf("%d %q %q", code, out, errb)
	}
}

// collector is the test's own MQTT client: it plays the escalation.
type collector struct {
	mu     sync.Mutex
	msgs   []mqtt.Message
	client *mqtt.Client
}

func (c *collector) of(typ string) []contract.Envelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []contract.Envelope
	for _, m := range c.msgs {
		if env, err := contract.ParseEnvelope(m.Payload); err == nil && env.Type == typ && env.Src == "gsm" {
			out = append(out, env)
		}
	}
	return out
}

func TestTheGatewayEndToEndWithASimulatedModem(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end scenario of about 10 s: run without -short")
	}
	broker := mqtttest.Start(t, "safehome-gsm")
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state)

	modem := attest.New()
	replacePort(t, func(gsmconfig.Port) (io.ReadWriteCloser, error) { return modem.Open() })

	col := &collector{}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := mqtt.Dial(ctx, mqtt.Options{
		Username: mqtttest.Peer, Password: mqtttest.Password(mqtttest.Peer),
		BrokerURL: broker.URL(), ClientID: fmt.Sprintf("col-%d", time.Now().UnixNano()),
		Subscriptions: []mqtt.Subscription{{Filter: "safehome/" + site + "/#", QoS: 1}},
		OnMessage:     func(m mqtt.Message) { col.mu.Lock(); col.msgs = append(col.msgs, m); col.mu.Unlock() },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Await(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close(context.Background())

	logs := drivertest.Logs(t)
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(runCtx, []string{"run", "-config", cfg}, &drivertest.SyncBuffer{}, logs) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()

	// The line is checked: the status says it works.
	drivertest.WaitFor(t, "the line status", func() bool {
		st := col.of(contract.TypeGSMStatus)
		if len(st) == 0 {
			return false
		}
		d, _ := contract.DecodeData[contract.GSMStatusData](st[len(st)-1])
		return len(d.Lines) == 1 && d.Lines[0].Registered && d.Lines[0].Operator == "TOGOCOM"
	})

	// The escalation asks for an SMS to the owner.
	id := contract.NewIDGenerator().New()
	env, _ := contract.NewEnvelope(site, "escalation", 1, id, time.Now(), contract.TypeAlertRequest, contract.AlertRequestData{
		Incident: "inc1", Kind: contract.AlertIntrusion, Urgency: contract.UrgencyHigh, Wave: 1,
		Contact: "u_owner", Channel: contract.ChannelSMS, Text: "ALARME Boutique Lome: intrusion a 21:04. Repondre 1 si vous gerez.",
	})
	b, _ := json.Marshal(env)
	if err := client.Publish(ctx, mqtt.Message{Topic: contract.AlertRequestTopic(site), Payload: b, QoS: 1}); err != nil {
		t.Fatal(err)
	}
	results := func() []string {
		var out []string
		for _, e := range col.of(contract.TypeAlertResult) {
			d, _ := contract.DecodeData[contract.AlertResultData](e)
			if d.Ref == id {
				out = append(out, d.Status)
			}
		}
		return out
	}
	drivertest.WaitFor(t, "the SMS to be sent", func() bool { return len(results()) == 2 })
	modem.Mu.Lock()
	sent := modem.Sent[0]
	modem.Mu.Unlock()
	if sent[0] != "+22890123456" || !strings.HasPrefix(sent[1], "ALARME") {
		t.Fatalf("%q", sent)
	}

	// The operator confirms; the owner answers "1".
	modem.Push(`+CDS: 6,1,"+22890123456",145,"26/10/17,21:05:00+00","26/10/17,21:05:03+00",0`)
	drivertest.WaitFor(t, "the delivery", func() bool { r := results(); return len(r) == 3 && r[2] == contract.ResultDelivered })
	modem.Mu.Lock()
	modem.Inbox[1] = [2]string{"+22890123456", "1"}
	modem.Mu.Unlock()
	modem.Push(`+CMTI: "SM",1`)
	drivertest.WaitFor(t, "the acknowledgement", func() bool { return len(col.of(contract.TypeAlertAck)) == 1 })
	ack, _ := contract.DecodeData[contract.AlertAckData](col.of(contract.TypeAlertAck)[0])
	if ack.Incident != "inc1" || ack.Contact != "u_owner" || ack.Via != "sms" {
		t.Fatalf("%+v", ack)
	}

	// Everything published follows the contract, and no text or number is logged.
	col.mu.Lock()
	msgs := append([]mqtt.Message(nil), col.msgs...)
	col.mu.Unlock()
	files := map[string]string{contract.TypeAlertResult: "alert-result.json", contract.TypeAlertAck: "alert-ack.json", contract.TypeGSMStatus: "gsm-status.json"}
	for _, m := range msgs {
		e, err := contract.ParseEnvelope(m.Payload)
		if err != nil || e.Src != "gsm" {
			continue
		}
		if err := schematest.Check(files[e.Type], e.Data); err != nil {
			t.Errorf("%s: %v", e.Type, err)
		}
	}
	if l := logs.String(); strings.Contains(l, "ALARME") || strings.Contains(l, "90123456") {
		t.Errorf("the logs leak a text or a number:\n%s", l)
	}

	// A clean stop removes the pid file.
	stop()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway did not stop")
	}
	if _, err := os.Stat(filepath.Join(state, pidName)); !os.IsNotExist(err) {
		t.Fatal("the pid file must be removed")
	}
}

func TestAMissingModemDoesNotStopTheGateway(t *testing.T) {
	broker := mqtttest.Start(t, "safehome-gsm")
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state)
	replacePort(t, func(gsmconfig.Port) (io.ReadWriteCloser, error) { return nil, fmt.Errorf("no such device") })

	logs := drivertest.Logs(t)
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"run", "-config", cfg}, &drivertest.SyncBuffer{}, logs) }()
	drivertest.WaitFor(t, "a failed check to be logged", func() bool { return strings.Contains(logs.String(), "a line check failed") })
	select {
	case code := <-done:
		t.Fatalf("the gateway gave up (exit %d): a modem may come back", code)
	default:
	}
	stop()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestASecondGatewayOnTheSameStateIsRefused(t *testing.T) {
	state := t.TempDir()
	cfg := writeConfig(t, "mqtt://127.0.0.1:1", state)
	running, err := pidfile.Lock(state, pidName) // a copy that runs holds the pid file
	if err != nil {
		t.Fatal(err)
	}
	defer running()
	if code, _, errb := cli("run", "-config", cfg); code != 1 || !strings.Contains(errb, "already using") {
		t.Fatalf("%d %q", code, errb)
	}
}

func replacePort(t *testing.T, open func(gsmconfig.Port) (io.ReadWriteCloser, error)) {
	orig := openPort
	openPort = open
	t.Cleanup(func() { openPort = orig })
}

func TestModemErrorsAreTranslated(t *testing.T) {
	for from, to := range map[error]error{
		atmodem.ErrNoNetwork: gateway.ErrNoNetwork, atmodem.ErrNoCredit: gateway.ErrNoCredit,
		atmodem.ErrBusy: gateway.ErrBusy, atmodem.ErrUnreachable: gateway.ErrUnreachable, atmodem.ErrTimeout: gateway.ErrTimeout,
	} {
		if err := translate(fmt.Errorf("x: %w", from)); !errors.Is(err, to) {
			t.Errorf("%v -> %v", from, err)
		}
	}
	other := errors.New("other")
	if translate(other) != other || translate(nil) != nil {
		t.Fatal("other errors pass through")
	}
}

func TestTheLazyModemOpensOnUseAndOnRestart(t *testing.T) {
	sim := attest.New()
	opens := 0
	l := &lazyModem{opts: atmodem.Options{
		Open:           func() (io.ReadWriteCloser, error) { opens++; return sim.Open() },
		CommandTimeout: time.Second, RestartWait: time.Millisecond,
	}}
	defer l.close()
	ctx := context.Background()
	if err := l.Restart(ctx); err != nil || opens != 1 {
		t.Fatalf("a restart of a modem never opened opens it: %v %d", err, opens)
	}
	if info, err := l.Check(ctx); err != nil || !info.Registered {
		t.Fatalf("%+v %v", info, err)
	}
	if ref, err := l.SendSMS(ctx, "+22890123456", "x"); err != nil || ref == "" {
		t.Fatalf("%q %v", ref, err)
	}
	if err := l.Call(ctx, "+22890123456", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := l.Restart(ctx); err != nil || opens != 2 {
		t.Fatalf("%v %d", err, opens)
	}

	broken := &lazyModem{opts: atmodem.Options{Open: func() (io.ReadWriteCloser, error) { return nil, errors.New("gone") }}}
	if _, err := broken.SendSMS(ctx, "+22890123456", "x"); err == nil {
		t.Fatal("no modem")
	}
	if err := broken.Call(ctx, "+22890123456", time.Second); err == nil {
		t.Fatal("no modem")
	}
	if _, err := broken.Check(ctx); err == nil {
		t.Fatal("no modem")
	}
	broken.close()
}

// publishApplied sends config.applied as the hub does once it runs with a version,
// retained.
func publishApplied(t *testing.T, broker string, version int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := mqtt.Dial(ctx, mqtt.Options{Username: mqtttest.Peer, Password: mqtttest.Password(mqtttest.Peer), BrokerURL: broker, ClientID: fmt.Sprintf("hub-%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := c.Await(ctx); err != nil {
		t.Fatal(err)
	}
	env, _ := contract.NewEnvelope(site, "config", uint64(version), contract.NewIDGenerator().New(), time.Now(),
		contract.TypeConfigApplied, contract.ConfigAppliedData{Version: version, Status: contract.ConfigApplied})
	b, _ := json.Marshal(env)
	if err := c.Publish(ctx, mqtt.Message{Topic: contract.ConfigAppliedTopic(site), Payload: b, QoS: 1, Retain: true}); err != nil {
		t.Fatal(err)
	}
}

func TestTheGatewayRestartsForANewConfigurationFromTheCloud(t *testing.T) {
	broker := mqtttest.Start(t, "safehome-gsm")
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state)
	replacePort(t, func(gsmconfig.Port) (io.ReadWriteCloser, error) { return nil, fmt.Errorf("no such device") })
	start := func(logs *drivertest.SyncBuffer) (context.CancelFunc, chan int) {
		ctx, stop := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"run", "-config", cfg}, &drivertest.SyncBuffer{}, logs) }()
		drivertest.WaitFor(t, "the gateway to start", func() bool { return strings.Contains(logs.String(), "msg=starting") })
		return stop, done
	}

	// The hub says it runs with version 1, but no file holds it here: nothing to load.
	publishApplied(t, broker.URL(), 1)
	logs := drivertest.Logs(t)
	stop, done := start(logs)
	time.Sleep(300 * time.Millisecond)

	// The hub kept version 2 and runs with it: the gateway restarts to load it.
	siteconfig.Write(state, contract.ConfigSetData{Version: 2, Contacts: []contract.ConfigContact{
		{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"sms"}, Phone: "+22891112233"},
	}})
	publishApplied(t, broker.URL(), 2)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\nlogs:\n%s", code, logs.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("no restart\nlogs:\n%s", logs.String())
	}
	stop()
	if !strings.Contains(logs.String(), "restarting to load the configuration from the cloud") {
		t.Fatalf("logs:\n%s", logs.String())
	}

	// Started again, it runs with version 2, and the retained message no longer moves it.
	logs = drivertest.Logs(t)
	stop, done = start(logs)
	if !strings.Contains(logs.String(), "site_config=2") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf("restarted again (exit %d): it would loop", code)
	default:
	}
	stop()
	<-done
	if code, out, _ := cli("check", "-config", cfg); code != 0 || !strings.Contains(out, "from the cloud: version 2") || !strings.Contains(out, "1 contact(s)") {
		t.Fatalf("%d %q", code, out)
	}
	if strings.Contains(logs.String(), "91112233") {
		t.Fatal("a phone number in the logs")
	}
}

// The gateway gives a line its SMS bound for the whole send: the modem's own bound for
// AT+CMGS, counted once the modem is free, and the waits before it must fit in it.
func TestTheGatewayLeavesTheModemItsTimeForAnSMS(t *testing.T) {
	if got := gsm.DefaultConfig(nil, nil).SMSTimeout; got < atmodem.DefaultSMSTimeout+time.Minute {
		t.Fatalf("the gateway gives an SMS %v, the modem alone may take %v", got, atmodem.DefaultSMSTimeout)
	}
}
