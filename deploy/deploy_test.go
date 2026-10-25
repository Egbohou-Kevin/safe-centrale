// Package deploy holds the files laid on the hub (systemd units, Mosquitto,
// Zigbee2MQTT, system settings; see docs/systeme.md). There is no code here: the test
// checks that these files agree with the programs and their example configurations,
// so a renamed setting or a moved folder breaks the build instead of a site.
package deploy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// unit is a systemd unit: section -> key -> values (a key may repeat).
type unit map[string]map[string][]string

func (u unit) get(t *testing.T, section, key string) string {
	t.Helper()
	v := u[section][key]
	if len(v) != 1 {
		t.Fatalf("[%s] %s: want one value, got %q", section, key, v)
	}
	return v[0]
}

func readUnit(t *testing.T, path string) unit {
	t.Helper()
	u := unit{}
	section := ""
	for i, line := range readLines(t, path) {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = line[1 : len(line)-1]
			u[section] = map[string][]string{}
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || section == "" {
				t.Fatalf("%s:%d: not a key=value line in a section: %q", path, i+1, line)
			}
			u[section][k] = append(u[section][k], v)
		}
	}
	return u
}

// readLines returns the trimmed lines of a file.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		lines = append(lines, strings.TrimSpace(s.Text()))
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// exampleConfig is what the deployment files must agree with in each program's example.
type exampleConfig struct {
	BrokerURL string `json:"broker_url"`
	StateDir  string `json:"state_dir"`
	BaseTopic string `json:"base_topic"`
}

func readExample(t *testing.T, short string) exampleConfig {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "config", short+".example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var c exampleConfig
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("%s.example.json: %v", short, err)
	}
	return c
}

// tools are the safehome-* programs run by hand (over SSH), not services: they have no
// unit and no configuration of their own.
var tools = []string{"install"}

// programs lists the safehome-* services under cmd/ by short name (hub, gsm, ...).
func programs(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("..", "cmd", "safehome-*"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dirs {
		if name := strings.TrimPrefix(filepath.Base(d), "safehome-"); !slices.Contains(tools, name) {
			names = append(names, name)
		}
	}
	if len(names) < 3 {
		t.Fatalf("found programs %v, want at least hub, gsm and zigbee", names)
	}
	return names
}

// directive returns the value of the first "key value..." line of a Mosquitto file.
func directive(t *testing.T, lines []string, key string) string {
	t.Helper()
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, " "); ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("mosquitto.conf: no %s line", key)
	return ""
}

// yamlValue returns the value of "key: value" inside the top-level block of a simple
// YAML file, without the trailing comment. Enough for the few keys checked here.
func yamlValue(t *testing.T, lines []string, block, key string) string {
	t.Helper()
	in := false
	for _, l := range dropComments(lines) {
		if !strings.HasPrefix(l, " ") && l != "" {
			in = strings.TrimSpace(l) == block+":"
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(l), ":")
		if ok && k == key {
			v, _, _ = strings.Cut(v, "#")
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("configuration.yaml: no %s.%s", block, key)
	return ""
}

// dropComments drops comment lines and keeps the indentation of the others.
func dropComments(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return out
}

// rawLines returns the lines of a file untrimmed: YAML needs the indentation.
func rawLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

func TestEveryProgramHasAUnitThatRunsItWithItsConfig(t *testing.T) {
	units, err := filepath.Glob(filepath.Join("systemd", "safehome-*.service"))
	if err != nil {
		t.Fatal(err)
	}
	progs := programs(t)
	if len(units) != len(progs) {
		t.Errorf("units %v for programs %v: want one unit per program", units, progs)
	}
	for _, p := range progs {
		t.Run(p, func(t *testing.T) {
			u := readUnit(t, filepath.Join("systemd", "safehome-"+p+".service"))
			want := "/usr/local/bin/safehome-" + p + " run -config /etc/safehome/" + p + ".json"
			if got := u.get(t, "Service", "ExecStart"); got != want {
				t.Errorf("ExecStart = %q, want %q", got, want)
			}
			if _, err := os.Stat(filepath.Join("..", "config", p+".example.json")); err != nil {
				t.Errorf("no example configuration for %s: %v", p, err)
			}
			cfg := readExample(t, p)
			if !slices.Contains(strings.Fields(u.get(t, "Service", "ReadWritePaths")), cfg.StateDir) {
				t.Errorf("ReadWritePaths does not cover state_dir %q", cfg.StateDir)
			}
			if got := u.get(t, "Service", "User"); got != "safehome" {
				t.Errorf("User = %q, want safehome", got)
			}
			if got := u.get(t, "Service", "ProtectSystem"); got != "strict" {
				t.Errorf("ProtectSystem = %q, want strict", got)
			}
		})
	}
}

func TestEveryServiceRestartsForever(t *testing.T) {
	units, err := filepath.Glob(filepath.Join("systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range units {
		t.Run(filepath.Base(path), func(t *testing.T) {
			u := readUnit(t, path)
			if got := u.get(t, "Service", "Restart"); got != "always" {
				t.Errorf("Restart = %q, want always", got)
			}
			if got := u.get(t, "Unit", "StartLimitIntervalSec"); got != "0" {
				t.Errorf("StartLimitIntervalSec = %q, want 0 (never give up)", got)
			}
			if !strings.Contains(u.get(t, "Unit", "After"), "mosquitto.service") {
				t.Error("does not start after mosquitto.service")
			}
			if got := u.get(t, "Install", "WantedBy"); got != "multi-user.target" {
				t.Errorf("WantedBy = %q, want multi-user.target (started at boot)", got)
			}
		})
	}
}

// Type=notify makes systemd wait for the program's READY=1, and WatchdogSec kill it
// when the pings stop: a program that never says so would be killed in a loop. Each
// program must say both (internal/adapter/sdnotify).
func TestEveryProgramIsWatchedAndTellsSystemd(t *testing.T) {
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := readUnit(t, filepath.Join("systemd", "safehome-"+p+".service"))
			if got := u.get(t, "Service", "Type"); got != "notify" {
				t.Errorf("Type = %q, want notify", got)
			}
			if got := u.get(t, "Service", "NotifyAccess"); got != "main" {
				t.Errorf("NotifyAccess = %q, want main (not go2rtc nor stty)", got)
			}
			d, err := time.ParseDuration(u.get(t, "Service", "WatchdogSec"))
			if err != nil || d < 10*time.Second || d > time.Minute {
				t.Errorf("WatchdogSec = %v (%v): want from 10 s (a slow SD card) to 1 min (a stuck alarm)", d, err)
			}
			// The programs run under internal/service's Daemon, which says both.
			src, err := os.ReadFile(filepath.Join("..", "cmd", "safehome-"+p, "run.go"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(src), "service.Daemon{") {
				t.Errorf("cmd/safehome-%s does not run under service.Daemon", p)
			}
		})
	}
	src, err := os.ReadFile(filepath.Join("..", "internal", "service", "daemon.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"sdnotify.FromEnv()", ".Ready()", ".Keepalive(", ".Stopping()"} {
		if !strings.Contains(string(src), call) {
			t.Errorf("internal/service/daemon.go does not call %s", call)
		}
	}
}

func TestSerialPortsAreReachable(t *testing.T) {
	for _, name := range []string{"safehome-gsm.service", "zigbee2mqtt.service"} {
		u := readUnit(t, filepath.Join("systemd", name))
		if got := u.get(t, "Service", "SupplementaryGroups"); got != "dialout" {
			t.Errorf("%s: SupplementaryGroups = %q, want dialout (serial port)", name, got)
		}
	}
}

// The battery module's lines and bus belong to the gpio and i2c groups on Raspberry
// Pi OS.
func TestTheBatteryModuleIsReachable(t *testing.T) {
	u := readUnit(t, filepath.Join("systemd", "safehome-power.service"))
	if got := u.get(t, "Service", "SupplementaryGroups"); got != "gpio i2c" {
		t.Errorf("SupplementaryGroups = %q, want gpio i2c (the X728's lines and gauge)", got)
	}
	if slices.Contains(rawLines(t, filepath.Join("systemd", "safehome-power.service")), "PrivateDevices=yes") {
		t.Error("PrivateDevices hides /dev/gpiochip0 and /dev/i2c-1")
	}
}

func TestOnlyTheHubReloadsOnSIGHUP(t *testing.T) {
	// The hub rereads its codes on SIGHUP; the other programs would exit.
	units, err := filepath.Glob(filepath.Join("systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range units {
		_, has := readUnit(t, path)["Service"]["ExecReload"]
		if want := filepath.Base(path) == "safehome-hub.service"; has != want {
			t.Errorf("%s: ExecReload present = %v, want %v", path, has, want)
		}
	}
}

// The messages that carry the access code (an order from the app, a keypad's key
// sequence) go at QoS 0 so that the hub's broker keeps none for a program away, in
// memory or in mosquitto.db: it holds only while queue_qos0_messages stays false.
// Written down rather than left to Mosquitto's default.
func TestTheBrokerKeepsNoQoS0MessageForAProgramAway(t *testing.T) {
	if got := directive(t, readLines(t, filepath.Join("mosquitto", "mosquitto.conf")), "queue_qos0_messages"); got != "false" {
		t.Fatalf("queue_qos0_messages %q, want false", got)
	}
}

func TestBrokerListensOnlyOnTheHubWhereEveryoneLooksForIt(t *testing.T) {
	lines := readLines(t, filepath.Join("mosquitto", "mosquitto.conf"))
	var listeners []string
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, " "); ok && k == "listener" {
			listeners = append(listeners, v)
		}
	}
	if len(listeners) != 1 {
		t.Fatalf("listeners %q, want exactly one", listeners)
	}
	port, host, _ := strings.Cut(listeners[0], " ")
	if host != "127.0.0.1" {
		t.Fatalf("listener on %q, want 127.0.0.1 only (never the shop's network)", host)
	}
	broker := "mqtt://" + host + ":" + port
	for _, p := range programs(t) {
		if got := readExample(t, p).BrokerURL; got != broker {
			t.Errorf("%s.example.json: broker_url %q, want %q", p, got, broker)
		}
	}
	z2m := rawLines(t, filepath.Join("zigbee2mqtt", "configuration.yaml"))
	if got := yamlValue(t, z2m, "mqtt", "server"); got != broker {
		t.Errorf("zigbee2mqtt: mqtt.server %q, want %q", got, broker)
	}
}

func TestZigbee2MQTTAndTheAdapterShareTheBaseTopic(t *testing.T) {
	z2m := rawLines(t, filepath.Join("zigbee2mqtt", "configuration.yaml"))
	want := readExample(t, "zigbee").BaseTopic
	if got := yamlValue(t, z2m, "mqtt", "base_topic"); got != want {
		t.Errorf("zigbee2mqtt base_topic %q, adapter base_topic %q", got, want)
	}
	// Our adapter decides presence and pairing; Zigbee2MQTT's own features stay off.
	for _, block := range []string{"availability", "frontend", "homeassistant"} {
		if got := yamlValue(t, z2m, block, "enabled"); got != "false" {
			t.Errorf("zigbee2mqtt %s.enabled = %q, want false", block, got)
		}
	}
}

// The stick as the trial on real hardware found it (24 October 2026): a ZBDongle-E
// with EmberZNet 7.4.4, driven by the ember adapter at 115200 bauds, without hardware
// flow control.
func TestZigbee2MQTTTalksToTheStickAsOnTheTrial(t *testing.T) {
	z2m := rawLines(t, filepath.Join("zigbee2mqtt", "configuration.yaml"))
	for key, want := range map[string]string{"adapter": "ember", "baudrate": "115200", "rtscts": "false"} {
		if got := yamlValue(t, z2m, "serial", key); got != want {
			t.Errorf("zigbee2mqtt serial.%s = %q, want %q", key, got, want)
		}
	}
}

// Zigbee2MQTT publishes the devices' messages at QoS 1: with the adapter's persistent
// session, the broker keeps them while the adapter restarts, instead of dropping a
// door that opened meanwhile. They are not retained: the adapter keeps the states
// itself, and an old message replayed is never an event anyway.
func TestZigbee2MQTTDeviceMessagesSurviveAnAdapterRestart(t *testing.T) {
	z2m := rawLines(t, filepath.Join("zigbee2mqtt", "configuration.yaml"))
	if got := yamlValue(t, z2m, "device_options", "qos"); got != "1" {
		t.Errorf("zigbee2mqtt device_options.qos = %q, want 1", got)
	}
	if got := yamlValue(t, z2m, "device_options", "retain"); got != "false" {
		t.Errorf("zigbee2mqtt device_options.retain = %q, want false", got)
	}
	// A message kept by the broker says when the device was heard, so that a key held
	// while the adapter was down expires at the hub (internal/zigbee/z2m reads it).
	if got := yamlValue(t, z2m, "advanced", "last_seen"); got != "ISO_8601" {
		t.Errorf("zigbee2mqtt advanced.last_seen = %q, want ISO_8601", got)
	}
}

func TestWhatMustBeWrittenIsOnTheDataPartition(t *testing.T) {
	// The system is read-only: every folder written at run time must be bound to the
	// data partition, or what it holds vanishes at the next restart.
	binds := map[string]bool{}
	for _, l := range readLines(t, filepath.Join("system", "fstab-data")) {
		f := strings.Fields(l)
		if len(f) == 6 && !strings.HasPrefix(f[0], "#") && slices.Contains(strings.Split(f[3], ","), "bind") {
			binds[filepath.Clean(f[1])] = true
		}
	}
	var need []string
	for _, p := range programs(t) {
		need = append(need, readExample(t, p).StateDir)
	}
	need = append(need, directive(t, readLines(t, filepath.Join("mosquitto", "mosquitto.conf")), "persistence_location"))
	z2m := readUnit(t, filepath.Join("systemd", "zigbee2mqtt.service"))
	for _, env := range z2m["Service"]["Environment"] {
		if v, ok := strings.CutPrefix(env, "ZIGBEE2MQTT_DATA="); ok {
			need = append(need, v)
		}
	}
	if len(need) < 5 {
		t.Fatalf("folders to check %v: the Zigbee2MQTT data folder is missing", need)
	}
	for _, dir := range need {
		if !binds[filepath.Clean(dir)] {
			t.Errorf("%s is written at run time but not bound to the data partition", dir)
		}
	}
}

func TestLogsStayInMemory(t *testing.T) {
	u := readUnit(t, filepath.Join("system", "journald-safehome.conf"))
	if got := u.get(t, "Journal", "Storage"); got != "volatile" {
		t.Errorf("Storage = %q, want volatile (spare the SD card)", got)
	}
	if u.get(t, "Journal", "RuntimeMaxUse") == "" {
		t.Error("RuntimeMaxUse is empty: the journal could fill the memory")
	}
}

func TestToolsAreNotServices(t *testing.T) {
	for _, name := range tools {
		if _, err := os.Stat(filepath.Join("..", "cmd", "safehome-"+name)); err != nil {
			t.Errorf("tool %s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join("systemd", "safehome-"+name+".service")); err == nil {
			t.Errorf("tool %s has a unit: it runs by hand", name)
		}
	}
}

// The console's polkit rule grants what nmcli needs to make and remove the hub's
// network, and nothing more: every service runs as safehome, and would get it too
// (turning the radio off would cut a site that reaches the internet by Wi-Fi).
func TestThePolkitRuleGrantsOnlyWhatTheHotspotUses(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("system", "polkit-safehome-console.rules"))
	if err != nil {
		t.Fatal(err)
	}
	var granted []string
	for _, m := range regexp.MustCompile(`case "([^"]+)":`).FindAllStringSubmatch(string(data), -1) {
		granted = append(granted, m[1])
	}
	want := []string{
		"org.freedesktop.NetworkManager.settings.modify.system", // connection add, delete
		"org.freedesktop.NetworkManager.network-control",        // connection up, down
		"org.freedesktop.NetworkManager.wifi.share.protected",   // a protected shared network
	}
	if !slices.Equal(granted, want) {
		t.Fatalf("granted %q, want %q", granted, want)
	}
}
