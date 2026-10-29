package deploy

// The passwords on the hub's broker (29 October 2026), as install.sh makes them on the
// fake system of install_test.go.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The hub's broker takes no anonymous client (29 October 2026): a program without its
// name and password is refused, and the file of names it reads is the one install.sh
// makes.
func TestTheBrokerTakesNoAnonymousClient(t *testing.T) {
	conf := readLines(t, filepath.Join("mosquitto", "mosquitto.conf"))
	if got := directive(t, conf, "allow_anonymous"); got != "false" {
		t.Errorf("allow_anonymous %s: any process of the hub could send an order to the alarm", got)
	}
	if got := directive(t, conf, "password_file"); got != "/etc/mosquitto/safehome.passwd" {
		t.Errorf("password_file %s, want the one install.sh makes, /etc/mosquitto/safehome.passwd", got)
	}
}

// mqttNames are the names on the hub's broker: each program, Zigbee2MQTT and the
// operator.
func mqttNames(t *testing.T) []string {
	var names []string
	for _, p := range programs(t) {
		names = append(names, "safehome-"+p)
	}
	return append(names, "zigbee2mqtt", "operator")
}

// mode returns the permissions of a file of the fake system.
func (r *rig) mode(p string) os.FileMode {
	r.t.Helper()
	fi, err := os.Stat(r.path(p))
	if err != nil {
		r.t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// Each name on the broker has its password, made once, root's alone, and kept at every
// run; Mosquitto's file is made again from them; none shows on a command line or in
// the output.
func TestTheBrokerPasswordsAreMadeOnceAndKeptPrivate(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	out, cmds, err := r.run()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}
	if m := r.mode("etc/safehome/mqtt"); m != 0o700 {
		t.Errorf("etc/safehome/mqtt mode %v, want 0700", m)
	}
	secrets := map[string]string{}
	seen := map[string]bool{}
	for _, n := range mqttNames(t) {
		f := "etc/safehome/mqtt/" + n + ".secret"
		secret := string(must(os.ReadFile(r.path(f))))
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(secret) {
			t.Errorf("%s: %d characters, want 43 in base64url (32 random bytes)", f, len(secret))
		}
		if seen[secret] {
			t.Errorf("%s: the same password as another name", f)
		}
		seen[secret], secrets[n] = true, secret
		if m := r.mode(f); m != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, m)
		}
	}
	if !slices.Contains(cmds, "chown -R root:root /etc/safehome/mqtt") {
		t.Error("etc/safehome/mqtt not given to root")
	}

	// Mosquitto's file: from the secrets, hashed, Mosquitto's alone.
	var plain []string
	for _, n := range mqttNames(t) {
		plain = append(plain, n+":"+secrets[n])
	}
	got := strings.Fields(string(must(os.ReadFile(r.path(".passwd-seen")))))
	slices.Sort(got)
	slices.Sort(plain)
	if !slices.Equal(got, plain) {
		t.Errorf("mosquitto_passwd hashed %d lines, not one per name with its secret", len(got))
	}
	passwd := "etc/mosquitto/safehome.passwd"
	if m := r.mode(passwd); m != 0o600 {
		t.Errorf("%s mode %v, want 0600", passwd, m)
	}
	for _, w := range []string{"mosquitto_passwd -U /" + passwd + ".plain", "chown mosquitto:mosquitto /" + passwd + ".plain"} {
		if !slices.Contains(cmds, w) {
			t.Errorf("did not run %q", w)
		}
	}
	if r.exists(passwd + ".plain") {
		t.Error("the plain list of passwords is left behind")
	}
	// Made before the broker restarts, which comes before the programs.
	hashed, broker, programs := slices.Index(cmds, "mosquitto_passwd -U /"+passwd+".plain"), slices.Index(cmds, "systemctl restart mosquitto"), slices.Index(cmds, "systemctl restart safehome-hub")
	if hashed < 0 || broker < hashed || programs < broker {
		t.Errorf("passwords at %d, broker restarted at %d, the hub at %d: want them in this order", hashed, broker, programs)
	}

	// Zigbee2MQTT's environment and the operator's options.
	for f, want := range map[string]string{
		"etc/safehome/mqtt/zigbee2mqtt.env": "ZIGBEE2MQTT_CONFIG_MQTT_USER=zigbee2mqtt\nZIGBEE2MQTT_CONFIG_MQTT_PASSWORD=" + secrets["zigbee2mqtt"] + "\n",
		"root/.config/mosquitto_sub":        "-u operator\n-P " + secrets["operator"] + "\n",
		"root/.config/mosquitto_pub":        "-u operator\n-P " + secrets["operator"] + "\n",
	} {
		if got := string(must(os.ReadFile(r.path(f)))); got != want {
			t.Errorf("%s does not give its name and password", f)
		}
		if m := r.mode(f); m != 0o600 {
			t.Errorf("%s mode %v, want 0600", f, m)
		}
	}

	// A second run keeps every password, byte for byte, and makes Mosquitto's file again.
	out2, cmds2, err := r.run()
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, out2)
	}
	for n, secret := range secrets {
		if again := string(must(os.ReadFile(r.path("etc/safehome/mqtt/" + n + ".secret")))); again != secret {
			t.Errorf("%s: its password changed at the second run", n)
		}
	}
	if !slices.Contains(cmds2, "mosquitto_passwd -U /"+passwd+".plain") {
		t.Error("Mosquitto's file not made again at the second run")
	}
	shown := strings.Join(append(append(cmds, cmds2...), out, out2), "\n")
	for n, secret := range secrets {
		if strings.Contains(shown, secret) {
			t.Errorf("the password of %s shows on a command line or in the output", n)
		}
	}
}

// install.sh puts the ACL, Mosquitto's alone, before mosquitto.conf, which names it
// (29 October 2026): one without the other, Mosquitto would not start or would refuse
// the programs.
func TestInstallPutsTheACLBeforeTheBrokerConfiguration(t *testing.T) {
	script := string(must(os.ReadFile("install.sh")))
	acl, conf := strings.Index(script, `put "$BUNDLE/deploy/mosquitto/safehome.acl"`), strings.Index(script, `put "$BUNDLE/deploy/mosquitto/mosquitto.conf"`)
	if acl < 0 || conf < acl {
		t.Errorf("install.sh puts safehome.acl at %d, mosquitto.conf at %d: want the ACL first", acl, conf)
	}
	r := newRig(t)
	r.minimalSite()
	cmds := r.mustRun()
	const path = "etc/mosquitto/safehome.acl"
	if got, want := string(must(os.ReadFile(r.path(path)))), string(must(os.ReadFile(filepath.Join("mosquitto", "safehome.acl")))); got != want {
		t.Errorf("%s is not deploy/mosquitto/safehome.acl", path)
	}
	if m := r.mode(path); m != 0o640 {
		t.Errorf("%s mode %v, want 0640", path, m)
	}
	given, broker := slices.Index(cmds, "chown mosquitto:mosquitto /"+path), slices.Index(cmds, "systemctl restart mosquitto")
	if given < 0 || broker < given {
		t.Errorf("%s given to mosquitto at %d, the broker restarted at %d: want both, in this order", path, given, broker)
	}
}
