package deploy

// The hardening of the security study (28 October 2026): settings common to every
// program's unit, those that hold only where the program's code allows them, and the
// two units that run third-party programs (Zigbee2MQTT, go2rtc under the video
// service) kept away from our secrets. Where a setting depends on what the code does,
// the code is read (go list for linux/arm64), never a list kept by hand.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// goPackage is what go list says of a package.
type goPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Imports    []string
	Deps       []string
}

var (
	goListOnce sync.Once
	goListPkgs map[string]goPackage
	goListErr  error
)

// goPackages lists the module's packages as built for the hub, without downloading.
func goPackages(t *testing.T) map[string]goPackage {
	t.Helper()
	goListOnce.Do(func() {
		cmd := exec.Command("go", "list", "-json", "safehome/...")
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOPROXY=off")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			goListErr = errors.New(err.Error() + ": " + stderr.String())
			return
		}
		goListPkgs = map[string]goPackage{}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p goPackage
			if err := dec.Decode(&p); err == io.EOF {
				break
			} else if err != nil {
				goListErr = err
				return
			}
			goListPkgs[p.ImportPath] = p
		}
	})
	if goListErr != nil {
		t.Fatalf("go list: %v", goListErr)
	}
	return goListPkgs
}

// ownPackages returns the program's main package and the module's packages it uses.
func ownPackages(t *testing.T, program string) []goPackage {
	t.Helper()
	pkgs := goPackages(t)
	main, ok := pkgs["safehome/cmd/safehome-"+program]
	if !ok {
		t.Fatalf("go list does not know safehome/cmd/safehome-%s", program)
	}
	own := []goPackage{main}
	for _, d := range main.Deps {
		if p, ok := pkgs[d]; ok {
			own = append(own, p)
		}
	}
	return own
}

// codeMatches returns the matches of re in the program's own Go code (tests left out),
// keeping those keep accepts, as "file: match".
func codeMatches(t *testing.T, program string, re *regexp.Regexp, keep func(string) bool) []string {
	t.Helper()
	var found []string
	for _, p := range ownPackages(t, program) {
		for _, f := range p.GoFiles {
			src, err := os.ReadFile(filepath.Join(p.Dir, f))
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range re.FindAllString(string(src), -1) {
				if keep(m) {
					found = append(found, p.ImportPath+"/"+f+": "+m)
				}
			}
		}
	}
	return found
}

// clockCalls are the calls ProtectClock's filter blocks (@clock), even to read.
var clockCalls = regexp.MustCompile(`\b(?:syscall|unix)\.(?:Adjtimex|ClockAdjtime|Settimeofday|ClockSettime)\b`)

func callsTheClock(t *testing.T, program string) []string {
	return codeMatches(t, program, clockCalls, func(string) bool { return true })
}

// procPath is a /proc path written in the code; ProcSubset=pid leaves only the
// processes' own folders.
var procPath = regexp.MustCompile(`"/proc(?:/[^"]*)?"`)

func readsProcBeyondPids(t *testing.T, program string) []string {
	return codeMatches(t, program, procPath, func(m string) bool {
		for _, own := range []string{`"/proc/self`, `"/proc/thread-self`, `"/proc/%d`} {
			if strings.HasPrefix(m, own) {
				return false
			}
		}
		return true
	})
}

// importsHTTP returns the module's packages of the program that import net/http.
func importsHTTP(t *testing.T, program string) []string {
	t.Helper()
	var found []string
	for _, p := range ownPackages(t, program) {
		if slices.Contains(p.Imports, "net/http") {
			found = append(found, p.ImportPath)
		}
	}
	return found
}

// value returns the unit's one value of [Service] key, or "" without it.
func (u unit) value(t *testing.T, key string) string {
	t.Helper()
	if _, ok := u["Service"][key]; !ok {
		return ""
	}
	return u.get(t, "Service", key)
}

// hides says whether the unit makes path unreadable: InaccessiblePaths= or an empty
// folder over it (TemporaryFileSystem=).
func (u unit) hides(path string) bool {
	for _, v := range u["Service"]["InaccessiblePaths"] {
		for _, f := range strings.Fields(v) {
			if strings.TrimPrefix(f, "-") == path {
				return true
			}
		}
	}
	for _, v := range u["Service"]["TemporaryFileSystem"] {
		for _, f := range strings.Fields(v) {
			if p, _, _ := strings.Cut(f, ":"); p == path {
				return true
			}
		}
	}
	return false
}

func programUnit(t *testing.T, p string) unit {
	t.Helper()
	return readUnit(t, filepath.Join("systemd", "safehome-"+p+".service"))
}

func TestEveryProgramHasTheCommonSettings(t *testing.T) {
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			for key, want := range map[string]string{
				"SystemCallArchitectures": "native", "UMask": "0077",
				"ProtectProc": "invisible", "ProtectHostname": "yes",
			} {
				if got := u.value(t, key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// ProtectClock blocks adjtimex even to read the clock's state, and implies
// DeviceAllow=char-rtc r, which restricts /dev to a list unless PrivateDevices has
// already done so. It is set wherever it closes nothing the program uses.
func TestProtectClockOnlyWhereItClosesNothing(t *testing.T) {
	if len(callsTheClock(t, "hub")) == 0 {
		t.Fatal("the scan does not see sysclock's adjtimex in the hub")
	}
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			calls := callsTheClock(t, p)
			private := u.value(t, "PrivateDevices") == "yes"
			switch got := u.value(t, "ProtectClock"); {
			case got != "" && got != "yes":
				t.Errorf("ProtectClock = %q", got)
			case got == "yes" && len(calls) > 0:
				t.Errorf("ProtectClock blocks what the code calls: %v", calls)
			case got == "yes" && !private:
				t.Error("ProtectClock restricts /dev to a list: the devices this program opens close")
			case got == "" && private && len(calls) == 0:
				t.Error("ProtectClock missing: the program neither calls the clock nor opens a device")
			}
		})
	}
	clock := readUnit(t, filepath.Join("system", "safehome-boot-clock.service"))
	if _, ok := clock["Service"]["ProtectClock"]; ok {
		t.Error("safehome-boot-clock sets ProtectClock")
	}
}

// ProcSubset=pid hides everything in /proc but the processes' folders. It is set
// unless the code reads more there, or a third-party program runs under the unit
// (not yet tried that way on the Pi).
func TestProcSubsetOnlyWhereNothingElseInProcIsRead(t *testing.T) {
	for _, p := range []string{"hub", "console"} {
		if len(readsProcBeyondPids(t, p)) == 0 {
			t.Fatalf("the scan does not see what the %s reads in /proc", p)
		}
	}
	untried := map[string]string{"video": "go2rtc and ffmpeg", "maint": "ssh"}
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			reads := readsProcBeyondPids(t, p)
			got := programUnit(t, p).value(t, "ProcSubset")
			switch {
			case got != "" && got != "pid":
				t.Errorf("ProcSubset = %q", got)
			case got == "pid" && len(reads) > 0:
				t.Errorf("ProcSubset=pid hides what the code reads: %v", reads)
			case got == "pid" && untried[p] != "":
				t.Errorf("ProcSubset=pid under %s, not yet tried on the Pi", untried[p])
			case got == "" && len(reads) == 0 && untried[p] == "":
				t.Error("ProcSubset=pid missing: the code reads nothing else in /proc")
			}
		})
	}
}

// The hub, the Zigbee adapter and the battery module speak to the local broker only:
// no IP address but the hub's own. Never the agent (the cloud), the GSM gateway, the
// maintenance (the bastion), the console (the phones) nor the video (the cameras).
func TestOnlyTheProgramsOfTheLocalBrokerAreCutFromTheNetwork(t *testing.T) {
	if len(importsHTTP(t, "agent")) == 0 {
		t.Fatal("the scan does not see the agent's net/http")
	}
	local := []string{"hub", "zigbee", "power"}
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			deny, allow := u.value(t, "IPAddressDeny"), u.value(t, "IPAddressAllow")
			if !slices.Contains(local, p) {
				if deny != "" || allow != "" {
					t.Errorf("IPAddressDeny=%q IPAddressAllow=%q: this program speaks beyond the hub", deny, allow)
				}
				return
			}
			if deny != "any" || allow != "localhost" {
				t.Errorf("IPAddressDeny=%q IPAddressAllow=%q, want any and localhost", deny, allow)
			}
			b, err := url.Parse(readExample(t, p).BrokerURL)
			if err != nil {
				t.Fatal(err)
			}
			if ip := net.ParseIP(b.Hostname()); b.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
				t.Errorf("the example's broker %q is not on the hub", b.Host)
			}
			if h := importsHTTP(t, p); len(h) > 0 {
				t.Errorf("its code speaks HTTP (%v): check where before cutting it off", h)
			}
		})
	}
}

// Zigbee2MQTT (Node) and the video service (go2rtc, ffmpeg) run third-party code: it
// writes only its own data and never sees /etc/safehome (cloud secret, maintenance
// key) nor, for Zigbee2MQTT, /var/lib/safehome (code key, access codes).
func TestTheEdgeUnitsSeeNoneOfOurSecrets(t *testing.T) {
	z2mPath := filepath.Join("systemd", "zigbee2mqtt.service")
	z2m := readUnit(t, z2mPath)
	data := ""
	for _, env := range z2m["Service"]["Environment"] {
		if v, ok := strings.CutPrefix(env, "ZIGBEE2MQTT_DATA="); ok {
			data = v
		}
	}
	if data == "" {
		t.Fatal("zigbee2mqtt.service: no ZIGBEE2MQTT_DATA")
	}
	if got := z2m.value(t, "ProtectSystem"); got != "strict" {
		t.Errorf("zigbee2mqtt: ProtectSystem = %q, want strict", got)
	}
	if got := z2m.value(t, "ReadWritePaths"); got != data {
		t.Errorf("zigbee2mqtt: ReadWritePaths = %q, want its data folder %s", got, data)
	}
	if !slices.Contains(readLines(t, "install.sh"), `chown safehome:safehome "$ROOT/var/lib/safehome" "$ROOT`+data+`"`) {
		t.Errorf("install.sh does not give %s to safehome", data)
	}
	for _, dir := range []string{"/etc/safehome", "/var/lib/safehome"} {
		if !z2m.hides(dir) {
			t.Errorf("zigbee2mqtt sees %s", dir)
		}
	}
	for _, line := range rawLines(t, z2mPath) {
		for _, key := range []string{"PrivateDevices=", "DevicePolicy=", "DeviceAllow=", "ProtectClock="} {
			if strings.HasPrefix(line, key) {
				t.Errorf("zigbee2mqtt: %s would close the stick", line)
			}
		}
	}

	video := programUnit(t, "video")
	if !video.hides("/etc/safehome") {
		t.Error("the video service sees /etc/safehome")
	}
	args := strings.Fields(video.get(t, "Service", "ExecStart"))
	i := slices.Index(args, "-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("the video's ExecStart has no -config: %v", args)
	}
	if got := video.value(t, "BindReadOnlyPaths"); got != args[i+1] {
		t.Errorf("BindReadOnlyPaths = %q, want its configuration %s alone", got, args[i+1])
	}
	if got := video.value(t, "ProtectSystem"); got != "strict" {
		t.Errorf("video: ProtectSystem = %q, want strict", got)
	}
}
