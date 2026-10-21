package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The install script runs here on a folder that stands for / (SAFEHOME_ROOT), with the
// system commands faked through PATH: each fake writes its command line to a log.
// Files are really copied, so the test sees what lands where.

// fakes are the system commands the script calls. $LOG, $ROOT and the FAKE_* variables
// come from the test.
var fakes = map[string]string{
	"id":         `echo 0`,
	"uname":      `echo aarch64`,
	"hostname":   `echo safehome-tg-lom-0001`,
	"findmnt":    `echo "${FAKE_FSTYPE:-ext4}"`,
	"mountpoint": `exit "${FAKE_UNMOUNTED:-0}"`,
	"getent":     `[ -f "$ROOT/.user" ]`,
	"useradd":    `touch "$ROOT/.user"`,
	"chown":      `exit 0`,
	"apt-get":    `exit 0`,
	"systemctl":  `exit 0`,
	"ssh-keygen": `while [ $# -gt 1 ]; do shift; done; echo key > "$1"; echo "ssh-ed25519 AAAA" > "$1.pub"`,
}

// fakeProgram stands for a safehome-* program: check fails on a configuration or a
// device profile that says BAD.
const fakeProgram = `if [ "$1" = check ]; then ! grep -qrs BAD "$3" "$(dirname "$3")/profiles"; fi`

type rig struct {
	t      *testing.T
	root   string
	bundle string
	log    string
	env    []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	if testing.Short() {
		t.Skip("runs install.sh on a fake system (about 20 s in all)")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	r := &rig{t: t, root: filepath.Join(dir, "root"), bundle: filepath.Join(dir, "bundle"), log: filepath.Join(dir, "log")}
	stubs := filepath.Join(dir, "stubs")
	for name, body := range fakes {
		writeScript(t, filepath.Join(stubs, name), `echo "`+name+` $*" >> "$LOG"; `+body)
	}
	for _, p := range append(programs(t), tools...) {
		name := "safehome-" + p
		writeScript(t, filepath.Join(r.bundle, "bin", name), `echo "bundle/`+name+` $*" >> "$LOG"; `+fakeProgram)
	}
	copyTree(t, ".", filepath.Join(r.bundle, "deploy"))
	copyTree(t, filepath.Join("..", "config", "profiles"), filepath.Join(r.bundle, "config", "profiles"))
	for _, d := range []string{"data", "var/lib/safehome", "var/lib/mosquitto", "var/lib/zigbee2mqtt"} {
		mkdir(t, filepath.Join(r.root, d))
	}
	r.env = append(os.Environ(), "SAFEHOME_ROOT="+r.root, "ROOT="+r.root, "LOG="+r.log,
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	return r
}

// site puts a file in the bundle's site/ folder.
func (r *rig) site(name, content string) {
	r.t.Helper()
	writeFile(r.t, filepath.Join(r.bundle, "site", name), content)
}

// minimalSite is what every site needs.
func (r *rig) minimalSite() {
	for _, f := range []string{"hub.json", "gsm.json", "zigbee.json", "agent.json", "cloud.secret"} {
		r.site(f, "{}")
	}
}

// run runs the script and returns its output and the commands it ran.
func (r *rig) run(env ...string) (out string, cmds []string, err error) {
	r.t.Helper()
	os.Remove(r.log)
	c := exec.Command("bash", filepath.Join(r.bundle, "deploy", "install.sh"))
	c.Env = append(r.env, env...)
	b, err := c.CombinedOutput()
	log, _ := os.ReadFile(r.log)
	for _, l := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		if l != "" {
			cmds = append(cmds, strings.ReplaceAll(l, r.root, ""))
		}
	}
	return string(b), cmds, err
}

func (r *rig) mustRun(env ...string) []string {
	r.t.Helper()
	out, cmds, err := r.run(env...)
	if err != nil {
		r.t.Fatalf("install.sh: %v\n%s", err, out)
	}
	return cmds
}

func (r *rig) path(p string) string { return filepath.Join(r.root, p) }

func (r *rig) exists(p string) bool {
	_, err := os.Stat(r.path(p))
	return err == nil
}

func TestInstallLaysEverythingAndStartsTheSite(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("maint.json", "{}")
	cmds := r.mustRun()

	for _, p := range append(programs(t), tools...) {
		if !r.exists("usr/local/bin/safehome-" + p) {
			t.Errorf("safehome-%s not installed", p)
		}
	}
	for _, f := range []string{
		"etc/safehome/hub.json", "etc/safehome/cloud.secret", "etc/safehome/maint.json",
		"etc/safehome/profiles/aqara-contact-mccgq11lm.json", "etc/safehome/maint.key",
		"etc/systemd/system/safehome-hub.service", "etc/systemd/system/zigbee2mqtt.service",
		"etc/systemd/journald.conf.d/safehome.conf", "etc/systemd/system.conf.d/safehome-watchdog.conf",
		"etc/mosquitto/mosquitto.conf",
	} {
		if !r.exists(f) {
			t.Errorf("%s not installed", f)
		}
	}
	if r.exists("etc/safehome.prev") {
		t.Error("the backup of the configuration is left behind (it holds secrets)")
	}
	if fi, err := os.Stat(r.path("etc/safehome/cloud.secret")); err != nil || fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("cloud.secret readable by others: %v %v", fi.Mode(), err)
	}

	want := []string{
		"apt-get install -y mosquitto",
		"useradd --system --home-dir /var/lib/safehome --shell /usr/sbin/nologin safehome",
		"bundle/safehome-hub check -config /etc/safehome/hub.json",
		"bundle/safehome-maint check -config /etc/safehome/maint.json",
		"systemctl daemon-reload",
		"systemctl restart safehome-hub",
		"systemctl restart safehome-agent",
		"systemctl restart safehome-maint",
	}
	for _, w := range want {
		if !slices.Contains(cmds, w) {
			t.Errorf("did not run %q", w)
		}
	}
	for _, c := range cmds {
		if strings.Contains(c, "video") || strings.Contains(c, "ffmpeg") || c == "systemctl enable zigbee2mqtt" {
			t.Errorf("ran %q: no camera and Zigbee2MQTT not installed", c)
		}
	}
	// Programs are checked before any of them restarts.
	lastCheck, firstRestart := -1, len(cmds)
	for i, c := range cmds {
		if strings.Contains(c, " check ") {
			lastCheck = i
		}
		if strings.HasPrefix(c, "systemctl restart safehome-") && i < firstRestart {
			firstRestart = i
		}
	}
	if lastCheck > firstRestart {
		t.Errorf("a program restarted before every configuration was checked: %q", cmds)
	}
}

func TestInstallCanRunAgain(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("maint.json", "{}")
	r.mustRun()
	key, _ := os.ReadFile(r.path("etc/safehome/maint.key"))
	writeFile(t, r.path("etc/safehome/maint.key"), "the key on the bastion")
	cmds := r.mustRun()
	for _, c := range cmds {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "ssh-keygen") {
			t.Errorf("ran %q again", c)
		}
	}
	if b, _ := os.ReadFile(r.path("etc/safehome/maint.key")); string(b) != "the key on the bastion" {
		t.Errorf("maint.key replaced (%q, first %q): the bastion would refuse the hub", b, key)
	}
	if !slices.Contains(cmds, "systemctl restart safehome-hub") {
		t.Error("the hub did not restart on its new program")
	}
}

func TestABadConfigurationPutsThePreviousOneBack(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("hub.json", `{"site_name": "good"}`)
	r.mustRun()
	before, _ := os.ReadFile(r.path("usr/local/bin/safehome-hub"))
	writeFile(t, r.path("usr/local/bin/safehome-hub"), "the running program")

	r.site("hub.json", `{"site_name": "BAD"}`)
	out, cmds, err := r.run()
	if err == nil {
		t.Fatalf("install.sh accepted a bad configuration:\n%s", out)
	}
	if !strings.Contains(out, "bad configuration: hub.json") {
		t.Errorf("output does not name the bad file:\n%s", out)
	}
	if b, _ := os.ReadFile(r.path("etc/safehome/hub.json")); string(b) != `{"site_name": "good"}` {
		t.Errorf("hub.json = %q, want the previous one back", b)
	}
	if b, _ := os.ReadFile(r.path("usr/local/bin/safehome-hub")); string(b) != "the running program" {
		t.Errorf("the program was replaced (was %d bytes) though its configuration is bad", len(before))
	}
	for _, c := range cmds {
		if strings.HasPrefix(c, "systemctl") {
			t.Errorf("ran %q after a bad configuration", c)
		}
	}
}

func TestABadProfileWithoutSiteFolderPutsThePreviousOnesBack(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.mustRun()

	// An update bundle made without -s: no site/ folder, but new profiles.
	if err := os.RemoveAll(filepath.Join(r.bundle, "site")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.bundle, "config", "profiles", "new-device.json"), `{"model": "BAD"}`)
	out, cmds, err := r.run()
	if err == nil {
		t.Fatalf("install.sh accepted a bad profile:\n%s", out)
	}
	if !strings.Contains(out, "The previous files are back") {
		t.Errorf("output does not say the previous files are back:\n%s", out)
	}
	if r.exists("etc/safehome/profiles/new-device.json") {
		t.Error("the bad profile was left in place")
	}
	if !r.exists("etc/safehome/profiles/aqara-contact-mccgq11lm.json") || !r.exists("etc/safehome/hub.json") {
		t.Error("the previous configuration is not back")
	}
	if r.exists("etc/safehome.prev") {
		t.Error("the backup of the configuration is left behind (it holds secrets)")
	}
	for _, c := range cmds {
		if strings.HasPrefix(c, "systemctl") {
			t.Errorf("ran %q after a bad profile", c)
		}
	}
}

func TestInstallRefusesWhatWouldBreakTheSite(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		site func(*rig)
		want string
	}{
		{"data partition not mounted", []string{"FAKE_UNMOUNTED=1"}, (*rig).minimalSite, "/data is not mounted"},
		{"read-only system", []string{"FAKE_FSTYPE=overlay"}, (*rig).minimalSite, "read-only"},
		{"no configuration", nil, func(*rig) {}, "no hub.json gsm.json zigbee.json agent.json cloud.secret"},
		{"camera without go2rtc", nil, func(r *rig) { r.minimalSite(); r.site("video.json", "{}") }, "not go2rtc"},
		{"battery module of another device", nil, func(r *rig) {
			r.minimalSite()
			r.site("hub.json", `{"hub_device_id": "dev_hubpower"}`)
			r.site("power.json", `{"device_id": "dev_other"}`)
		}, "the hub would never see a power cut"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			c.site(r)
			out, cmds, err := r.run(c.env...)
			if err == nil || !strings.Contains(out, c.want) {
				t.Fatalf("err %v, output %q: want a refusal saying %q", err, out, c.want)
			}
			if r.exists("etc/safehome") || r.exists("usr/local/bin") {
				t.Error("files were laid before the refusal")
			}
			for _, cmd := range cmds {
				if !regexp.MustCompile(`^(id|uname|findmnt|mountpoint) `).MatchString(cmd) {
					t.Errorf("ran %q before the refusal", cmd)
				}
			}
		})
	}
}

func TestASiteWithCamerasGetsVideo(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("video.json", "{}")
	writeScript(t, filepath.Join(r.bundle, "bin", "go2rtc"), "exit 0")
	cmds := r.mustRun()
	if !r.exists("usr/local/bin/go2rtc") {
		t.Error("go2rtc not installed")
	}
	for _, w := range []string{
		"apt-get install -y --no-install-recommends ffmpeg",
		"bundle/safehome-video check -config /etc/safehome/video.json",
		"systemctl restart safehome-video",
	} {
		if !slices.Contains(cmds, w) {
			t.Errorf("did not run %q", w)
		}
	}
}

func TestASiteWithTheBatteryModuleGetsItsService(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("hub.json", `{"site": "tg-lom-0001", "hub_device_id": "dev_hubpower"}`)
	r.site("power.json", `{"site": "tg-lom-0001", "device_id": "dev_hubpower"}`)
	cmds := r.mustRun()
	for _, w := range []string{"bundle/safehome-power check -config /etc/safehome/power.json", "systemctl restart safehome-power"} {
		if !slices.Contains(cmds, w) {
			t.Errorf("did not run %q", w)
		}
	}
}

func TestASiteWithTheConsoleGetsItsPassphraseOnce(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("console.json", "{}")
	cmds := r.mustRun()
	secret, err := os.ReadFile(r.path("etc/safehome/console.secret"))
	if err != nil {
		t.Fatal(err)
	}
	pass := strings.TrimSpace(string(secret))
	if len(pass) != 12 || strings.ContainsAny(pass, "lo01") {
		t.Fatalf("passphrase %q: want 12 characters without look-alikes", pass)
	}
	for _, w := range []string{"bundle/safehome-console check -config /etc/safehome/console.json", "systemctl restart safehome-console"} {
		if !slices.Contains(cmds, w) {
			t.Errorf("did not run %q", w)
		}
	}
	if !r.exists("etc/polkit-1/rules.d/50-safehome-console.rules") {
		t.Error("the polkit rule is not installed: nmcli would be refused")
	}
	r.mustRun()
	if again, _ := os.ReadFile(r.path("etc/safehome/console.secret")); string(again) != string(secret) {
		t.Fatal("the passphrase changed at the second run: the label would be wrong")
	}
}

func TestZigbee2MQTTStartsOnlyOnceItsStickIsSet(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	mkdir(t, r.path("opt/zigbee2mqtt"))
	out, cmds, err := r.run()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	for _, c := range []string{"systemctl enable zigbee2mqtt", "systemctl restart zigbee2mqtt"} {
		if slices.Contains(cmds, c) {
			t.Errorf("ran %q with CHANGE-ME as its stick: it would fail every 5 s, after each reboot too", c)
		}
	}
	if !strings.Contains(out, "first, Zigbee2MQTT: ") || !strings.Contains(out, "sudo systemctl enable --now zigbee2mqtt") {
		t.Errorf("the final message does not say how to start Zigbee2MQTT once set:\n%s", out)
	}

	// Set by the installer; from then on the file holds the network key.
	conf := r.path("var/lib/zigbee2mqtt/configuration.yaml")
	writeFile(t, conf, "serial:\n  port: /dev/serial/by-id/usb-ITead\nnetwork_key: [1, 2, 3]\n")
	cmds = r.mustRun()
	for _, c := range []string{"systemctl enable zigbee2mqtt", "systemctl restart zigbee2mqtt"} {
		if !slices.Contains(cmds, c) {
			t.Errorf("did not run %q once set", c)
		}
	}
	if b, _ := os.ReadFile(conf); !strings.Contains(string(b), "network_key: [1, 2, 3]") {
		t.Errorf("configuration.yaml replaced: the Zigbee network would be lost\n%s", b)
	}
}

func TestTheScriptsKnowEveryProgram(t *testing.T) {
	src := string(must(os.ReadFile("install.sh")))
	list := func(name string) []string {
		m := regexp.MustCompile(`(?m)^` + name + `="([^"]*)"`).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("install.sh has no %s= line", name)
		}
		return strings.Fields(m[1])
	}
	got, want := list("PROGRAMS"), programs(t)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("install.sh PROGRAMS %v, services under cmd/ %v", got, want)
	}
	if got := list("TOOLS"); !slices.Equal(got, tools) {
		t.Errorf("install.sh TOOLS %v, want %v", got, tools)
	}
	for _, p := range list("REQUIRED") {
		if !slices.Contains(want, p) {
			t.Errorf("REQUIRED names %s, which is not a program", p)
		}
	}
}

func TestBundleHoldsWhatInstallNeeds(t *testing.T) {
	if testing.Short() {
		t.Skip("builds every program for the Raspberry Pi")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	dir := t.TempDir()
	site := filepath.Join(dir, "site")
	writeFile(t, filepath.Join(site, "cloud.secret"), "s3cret")
	out := filepath.Join(dir, "bundle")
	if b, err := exec.Command("bash", "bundle.sh", "-v", "1.2.3", "-s", site, out).CombinedOutput(); err != nil {
		t.Fatalf("bundle.sh: %v\n%s", err, b)
	}
	for _, p := range append(programs(t), tools...) {
		if fi, err := os.Stat(filepath.Join(out, "bin", "safehome-"+p)); err != nil || fi.Mode()&0o111 == 0 {
			t.Errorf("bin/safehome-%s: %v", p, err)
		}
	}
	for _, f := range []string{"deploy/install.sh", "deploy/systemd/safehome-hub.service", "config/profiles", "config/hub.example.json", "site/cloud.secret"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, f := range []string{"deploy/deploy_test.go", "deploy/bundle.sh"} {
		if _, err := os.Stat(filepath.Join(out, f)); err == nil {
			t.Errorf("%s is in the bundle", f)
		}
	}
	if fi, err := os.Stat(filepath.Join(out, "site", "cloud.secret")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("site/cloud.secret readable by others: %v", err)
	}
	if b, err := exec.Command("bash", "bundle.sh", out).CombinedOutput(); err == nil {
		t.Errorf("bundle.sh wrote over an existing bundle:\n%s", b)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// copyTree copies a folder, keeping the file modes (install.sh stays executable).
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, fi.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
