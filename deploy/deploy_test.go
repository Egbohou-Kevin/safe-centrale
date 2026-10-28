// Package deploy holds the files laid on the hub (systemd units, Mosquitto,
// Zigbee2MQTT, system settings; see docs/systeme.md). There is no code here: the test
// checks that these files agree with the programs and their example configurations,
// so a renamed setting or a moved folder breaks the build instead of a site.
package deploy

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
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

// NetworkManager's shared mode routes the console's phones to the hub's other
// networks: the dispatcher script turns forwarding off on that network once it is up,
// and on no other (the hub reaches the internet through them).
func TestTheConsolesNetworkRoutesNothing(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is not installed")
	}
	script, err := filepath.Abs(filepath.Join("system", "nm-dispatcher-safehome-console"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		connection, action, want string
	}{
		{"safehome-console", "up", "0"},
		{"safehome-console", "down", "1"},
		{"Wired connection 1", "up", "1"},
	} {
		root := t.TempDir()
		setting := filepath.Join(root, "proc", "sys", "net", "ipv4", "conf", "wlan0", "forwarding")
		if err := os.MkdirAll(filepath.Dir(setting), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(setting, []byte("1\n"), 0o644) // what the shared mode leaves
		cmd := exec.Command(script, "wlan0", c.action)
		cmd.Env = []string{"CONNECTION_ID=" + c.connection, "SAFEHOME_ROOT=" + root, "PATH=" + os.Getenv("PATH")}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v %s", c.connection, c.action, err, out)
		}
		if got, _ := os.ReadFile(setting); strings.TrimSpace(string(got)) != c.want {
			t.Errorf("%s %s: forwarding %q, want %s", c.connection, c.action, got, c.want)
		}
	}
	if info, err := os.Stat(script); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("NetworkManager runs only an executable script: %v", err)
	}
}

// Every unit of ours closes what the programs never need. ProtectClock and, where a
// device is opened, PrivateDevices are left out: they restrict /dev to a list, which
// would close the serial ports, the battery module's lines and bus, and the camera.
func TestEveryProgramIsHardened(t *testing.T) {
	devices := []string{"gsm", "power", "video", "console", "maint"}
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			path := filepath.Join("systemd", "safehome-"+p+".service")
			u := readUnit(t, path)
			for key, want := range map[string]string{
				"ProtectKernelModules": "yes", "ProtectKernelLogs": "yes", "RestrictSUIDSGID": "yes",
				"RestrictRealtime": "yes", "RestrictNamespaces": "yes", "LockPersonality": "yes",
				"RestrictAddressFamilies": "AF_UNIX AF_INET AF_INET6 AF_NETLINK",
			} {
				if got := u.get(t, "Service", key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if p != "console" && u.get(t, "Service", "CapabilityBoundingSet") != "" {
				t.Error("a program without a capability keeps an empty CapabilityBoundingSet")
			}
			if p != "video" && u.get(t, "Service", "MemoryDenyWriteExecute") != "yes" {
				t.Error("MemoryDenyWriteExecute is for every Go program (not go2rtc and ffmpeg)")
			}
			for _, line := range rawLines(t, path) {
				if strings.HasPrefix(line, "ProtectClock=") || strings.HasPrefix(line, "DeviceAllow=") {
					t.Errorf("%s restricts /dev to a list", line)
				}
				if line == "PrivateDevices=yes" && slices.Contains(devices, p) {
					t.Error("PrivateDevices hides the devices this program opens")
				}
			}
		})
	}
}

// The clock at boot is taken after systemd-timesyncd set it forward to the last time it
// saved, before any network (sysinit.target), so before NTP; once per boot. Its line,
// the time then the uptime, is what the hub reads (sysclock.StartShown); the hub's
// hardening leaves /run readable.
func TestTheClockAtBootIsTakenBeforeNTP(t *testing.T) {
	path := filepath.Join("system", "safehome-boot-clock.service")
	u := readUnit(t, path)
	if got := u.get(t, "Unit", "DefaultDependencies"); got != "no" {
		t.Errorf("DefaultDependencies = %q: it would wait for sysinit.target, after which NTP may run", got)
	}
	if !slices.Contains(strings.Fields(u.get(t, "Unit", "After")), "systemd-timesyncd.service") {
		t.Error("not after systemd-timesyncd.service, which sets the clock forward to the last time it saved")
	}
	if !slices.Contains(strings.Fields(u.get(t, "Unit", "Before")), "sysinit.target") {
		t.Error("not before sysinit.target: the network, hence NTP, may come first")
	}
	if got := u.get(t, "Install", "WantedBy"); got != "sysinit.target" {
		t.Errorf("WantedBy = %q", got)
	}
	if u.get(t, "Service", "Type") != "oneshot" || u.get(t, "Service", "RemainAfterExit") != "yes" {
		t.Error("not a oneshot that stays: taken once per boot")
	}
	hub := readUnit(t, filepath.Join("systemd", "safehome-hub.service"))
	for _, k := range []string{"InaccessiblePaths", "TemporaryFileSystem", "ProtectProc"} {
		if v, ok := hub["Service"][k]; ok {
			t.Errorf("the hub's unit sets %s=%q: check it still reads /run/safehome-boot-clock", k, v)
		}
	}

	// The script, its paths and date replaced: it writes the time, then the uptime.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bin", "date"), "#!/bin/sh\n[ \"$1\" = +%s.%N ] && echo 1761644280.123456789\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "date"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "uptime"), "12.50 40.10\n")
	run := u.get(t, "Service", "ExecStart")
	script, ok := strings.CutPrefix(run, "/bin/sh -c '")
	if !ok || !strings.HasSuffix(script, "'") {
		t.Fatalf("ExecStart = %q, want /bin/sh -c '...'", run)
	}
	script = strings.NewReplacer("%%", "%", "/proc/uptime", filepath.Join(dir, "uptime"),
		"/run/safehome-boot-clock", filepath.Join(dir, "boot-clock")).Replace(strings.TrimSuffix(script, "'"))
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "boot-clock")); err != nil || string(b) != "1761644280.123456789 12.50\n" {
		t.Errorf("wrote %q (%v)", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "boot-clock.new")); err == nil {
		t.Error("the file being written is left behind")
	}
}

// timesyncdClock is where systemd-timesyncd keeps its clock file (its StateDirectory=).
const timesyncdClock = "/var/lib/systemd/timesync"

// timesyncdBind returns the line of fstab-data that binds timesyncd's folder, or "".
func timesyncdBind(t *testing.T) string {
	t.Helper()
	for _, l := range rawLines(t, filepath.Join("system", "fstab-data")) {
		if f := strings.Fields(l); len(f) == 6 && !strings.HasPrefix(f[0], "#") && f[1] == timesyncdClock {
			return l
		}
	}
	return ""
}

func TestTimesyncdsClockFileIsKeptAcrossBoots(t *testing.T) {
	// The system being read-only, timesyncd's clock file never moved from the day the
	// system went read-only, and every boot started from that day (28 October 2026).
	line := timesyncdBind(t)
	if line == "" {
		t.Fatalf("fstab-data binds nothing on %s: the clock starts from the day the system went read-only", timesyncdClock)
	}
	f := strings.Fields(line)
	opts := strings.Split(f[3], ",")
	if !strings.HasPrefix(f[0], "/data/") || f[2] != "none" || !slices.Contains(opts, "bind") {
		t.Errorf("%q: want a bind from a folder of /data", line)
	}
	if !slices.Contains(opts, "nofail") {
		t.Errorf("%q: without nofail, a missing folder would stop the boot", line)
	}
}

// bindSources returns the source folders of the bind lines of fstab-data.
func bindSources(t *testing.T) []string {
	t.Helper()
	var srcs []string
	for _, l := range rawLines(t, filepath.Join("system", "fstab-data")) {
		f := strings.Fields(l)
		if len(f) == 6 && !strings.HasPrefix(f[0], "#") && slices.Contains(strings.Split(f[3], ","), "bind") {
			srcs = append(srcs, f[0])
		}
	}
	return srcs
}

// madeBefore reports whether a mkdir line of lines names dir before the first line that
// starts with stop, and whether that line is there at all. "$ROOT" and quotes are ignored.
func madeBefore(lines []string, dir, stop string) (made, stopped bool) {
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), stop) {
			return made, true
		}
		f := strings.Fields(strings.NewReplacer(`"`, "", "$ROOT", "").Replace(l))
		if i := slices.Index(f, "mkdir"); i >= 0 && slices.Contains(f[i+1:], dir) {
			made = true
		}
	}
	return made, false
}

func TestMadeBeforeSeesAFolderMadeTooLate(t *testing.T) {
	lines := []string{"mkdir -p /data/a", "# before mount -a", "mount -a", `mkdir -p "$ROOT/data/b"`}
	if made, stopped := madeBefore(lines, "/data/a", "mount -a"); !made || !stopped {
		t.Errorf("/data/a: made %v, stopped %v; want both", made, stopped)
	}
	if made, _ := madeBefore(lines, "/data/b", "mount -a"); made {
		t.Error("/data/b is made after mount -a, yet seen as made before")
	}
	if made, _ := madeBefore([]string{`mkdir -p "$ROOT/data/b"`}, "/data/b", "mv"); !made {
		t.Error(`"$ROOT/data/b" not seen as /data/b`)
	}
}

func TestEveryBindSourceIsMadeBeforeItsLineIsMounted(t *testing.T) {
	// A bind without its source fails; nofail keeps the boot going, but timesyncd requires
	// the mounts of its StateDirectory= (systemd.exec(5)): the hub would run without NTP.
	// On 28 October 2026 the virtual kit's prepare-pi.sh ran mount -a on the new line
	// without its folder.
	srcs := bindSources(t)
	if len(srcs) < 4 {
		t.Fatalf("fstab-data binds %v: want the three programs' folders and timesyncd's", srcs)
	}
	var docs []string
	inStep2 := false
	for _, l := range rawLines(t, filepath.Join("..", "docs", "systeme.md")) {
		if strings.HasPrefix(l, "### ") {
			inStep2 = strings.HasPrefix(l, "### 2. ")
		}
		if inStep2 {
			docs = append(docs, l)
		}
	}
	consumers := []struct {
		name  string
		lines []string
		stop  string
		srcs  []string
	}{
		{"vkit/prepare-pi.sh", rawLines(t, filepath.Join("..", "vkit", "prepare-pi.sh")), "mount -a", srcs},
		{"docs/systeme.md, step 2", docs, "sudo mount -a", srcs},
		// install.sh writes only timesyncd's line; it refuses to run without the others.
		{"deploy/install.sh", rawLines(t, "install.sh"), `mv -f "$fstab.new" "$fstab"`, []string{"/data/timesync"}},
	}
	for _, c := range consumers {
		for _, src := range c.srcs {
			made, stopped := madeBefore(c.lines, src, c.stop)
			if !stopped {
				t.Errorf("%s: no line with %q", c.name, c.stop)
				break
			}
			if !made {
				t.Errorf("%s: %s is not made before %q", c.name, src, c.stop)
			}
		}
	}
}
