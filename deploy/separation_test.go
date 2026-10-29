package deploy

// What each program's unit sees of /etc/safehome (29 October 2026, the security
// study's remark 3, first phase). Every service runs as safehome, whose group reads the
// whole folder: systemd shows each one its own files alone, and none of them the
// Zigbee network's key. The two tables come from the study of the files each service
// reads (fichiers-par-service.md, commit 581bd17, its section 3), checked here against
// the examples of config/ and the names install.sh makes.

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"safehome/internal/agentconfig"
)

// etc is the programs' configuration folder.
const etc = "/etc/safehome"

// mustSee lists, for each program, the files of /etc/safehome its code reads: a unit
// that hides one keeps its program from starting (the hub: no alarm at all).
var mustSee = map[string][]string{
	"hub":     {"hub.json"},
	"agent":   {"agent.json", "cloud.secret"},
	"gsm":     {"gsm.json"},
	"zigbee":  {"zigbee.json", "profiles"},
	"power":   {"power.json"},
	"maint":   {"maint.json", "maint.key", "maint_known_hosts"},
	"console": {"console.json", "hub.json", "console.secret"},
	"video":   {"video.json"},
}

// readers lists every file of /etc/safehome with the programs allowed to read it:
// every other program's unit hides it.
var readers = map[string][]string{
	"hub.json":          {"hub", "console"}, // the contacts
	"agent.json":        {"agent"},
	"cloud.secret":      {"agent"}, // the site's secret for the cloud
	"gsm.json":          {"gsm"},   // the contacts' numbers
	"zigbee.json":       {"zigbee"},
	"profiles":          {"zigbee"},
	"power.json":        {"power"},
	"maint.json":        {"maint"},
	"maint.key":         {"maint"}, // the hub's key for the tunnel
	"maint.key.pub":     {"maint"}, // ssh may look for it next to the key
	"maint_known_hosts": {"maint"},
	"console.json":      {"console"},
	"console.secret":    {"console"}, // the console's Wi-Fi passphrase
	"video.json":        {"video"},   // the cameras' passwords
	"mqtt":              nil,         // the broker's passwords: systemd reads them (LoadCredential=), outside the unit
}

// rereads names the programs that read a file of /etc/safehome again while they run.
// A file bound alone keeps the inode it had at the start, so one replaced by rename (an
// editor, install.sh putting the previous folder back) would stay the old one, in
// silence: their units hide the others' files one by one instead.
var rereads = map[string]string{
	"hub":     "hub.json: the contacts, and each configuration from the cloud checked against it (cmd/safehome-hub/run.go)",
	"console": "hub.json at each access code changed (cmd/safehome-console/codes.go)",
	"maint":   "ssh reads the key and the bastion's host key at each tunnel (internal/adapter/sshtunnel)",
}

func TestEachUnitSeesTheFilesItsProgramReads(t *testing.T) {
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			need, ok := mustSee[p]
			if !ok {
				t.Fatalf("mustSee does not name %s: say which files of %s its code reads", p, etc)
			}
			u := programUnit(t, p)
			if c := configOf(t, u); !slices.Contains(need, strings.TrimPrefix(c, etc+"/")) {
				t.Errorf("its configuration %s is not in mustSee", c)
			}
			for _, f := range need {
				if !u.sees(etc + "/" + f) {
					t.Errorf("hides %s/%s, which its program reads: it would not start", etc, f)
				}
				if !slices.Contains(readers[f], p) {
					t.Errorf("readers does not give %s to %s", f, p)
				}
			}
		})
	}
}

func TestEveryFileOfTheConfigIsHiddenFromTheOtherUnits(t *testing.T) {
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			for _, f := range slices.Sorted(maps.Keys(readers)) {
				if !slices.Contains(readers[f], p) && u.sees(etc+"/"+f) {
					t.Errorf("sees %s/%s, which is for %v alone", etc, f, readers[f])
				}
			}
		})
	}
}

// A unit whose program reads its files at the start only puts an empty folder over
// /etc/safehome and binds them back: a file added later stays hidden. One that reads a
// file again binds none, and hides the others' one by one. Either way an entry of
// InaccessiblePaths= is optional: a file the site does not have must not stop the unit.
func TestAUnitBindsTheFilesOfTheConfigOnlyIfItReadsThemOnce(t *testing.T) {
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			why, again := rereads[p]
			switch shut := u.hides(etc); {
			case again && shut:
				t.Errorf("an empty folder over %s, its files bound back, but it reads %s", etc, why)
			case !again && !shut:
				t.Errorf("no empty folder over %s: it reads its files at the start only, and a file the table forgets would stay seen", etc)
			}
			for _, f := range fields(u["Service"]["InaccessiblePaths"]) {
				if !strings.HasPrefix(f, "-") {
					t.Errorf("InaccessiblePaths=%s without -: the unit would not start where it is missing", f)
				}
			}
		})
	}
}

// configOf returns the configuration file the unit's ExecStart gives its program.
func configOf(t *testing.T, u unit) string {
	t.Helper()
	args := strings.Fields(u.get(t, "Service", "ExecStart"))
	i := slices.Index(args, "-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("ExecStart has no -config: %v", args)
	}
	return args[i+1]
}

// etcName is a file of /etc/safehome named in install.sh: under $ETC, or a site's file
// it looks for (configured, site_file).
var etcName = regexp.MustCompile(`\$\{?ETC\}?/([A-Za-z0-9_.$-]+)|\b(?:configured|site_file) "?([A-Za-z0-9_.$-]+)`)

// argument is a shell function's argument ($1), not a name.
var argument = regexp.MustCompile(`^\$[0-9]`)

// etcNames returns the files of /etc/safehome the script's lines name (comments left
// out), $p read as each program; a name it cannot read is returned as it is written.
func etcNames(lines, programs []string) []string {
	var names []string
	for _, l := range lines {
		if strings.HasPrefix(l, "#") {
			continue
		}
		for _, m := range etcName.FindAllStringSubmatch(l, -1) {
			name := m[1] + m[2]
			switch {
			case argument.MatchString(name):
			case strings.HasPrefix(name, "$p"):
				for _, p := range programs {
					names = append(names, p+strings.TrimPrefix(name, "$p"))
				}
			default:
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// install.sh makes no file of /etc/safehome the tables do not name: a new one would be
// seen by every unit that hides the others' files one by one.
func TestInstallMakesNoFileOfTheConfigTheTablesDoNotName(t *testing.T) {
	names := etcNames(readLines(t, "install.sh"), programs(t))
	for _, want := range []string{"cloud.secret", "maint.key", "console.secret", "hub.json", "profiles", "mqtt"} {
		if !slices.Contains(names, want) {
			t.Fatalf("the scan does not see install.sh make %s: %v", want, names)
		}
	}
	if got := etcNames([]string{`ssh-keygen -f "$ETC/new.key"`, `configured extra.json`}, nil); !slices.Equal(got, []string{"extra.json", "new.key"}) {
		t.Fatalf("the scan reads %v in two new names", got)
	}
	for _, n := range names {
		if _, ok := readers[n]; !ok {
			t.Errorf("install.sh makes %s/%s, which readers does not name: say who reads it", etc, n)
		}
	}
}

// configFiles appends to found the files of /etc/safehome a configuration names: an
// absolute path under it, or a relative *_file or *_dir (relative to its folder).
func configFiles(v any, key string, found *[]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			configFiles(c, k, found)
		}
	case []any:
		for _, c := range x {
			configFiles(c, key, found)
		}
	case string:
		switch {
		case strings.HasPrefix(x, etc+"/"):
			*found = append(*found, strings.TrimPrefix(x, etc+"/"))
		case x != "" && !filepath.IsAbs(x) && (strings.HasSuffix(key, "_file") || strings.HasSuffix(key, "_dir")):
			*found = append(*found, x)
		}
	}
}

// What each program's example names in /etc/safehome is in mustSee: a site that
// follows the example gets a unit that shows its program those files.
func TestMustSeeHoldsWhatTheExamplesName(t *testing.T) {
	named := map[string][]string{}
	for _, p := range programs(t) {
		b, err := os.ReadFile(filepath.Join("..", "config", p+".example.json"))
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("%s.example.json: %v", p, err)
		}
		var found []string
		configFiles(v, "", &found)
		named[p] = found
		for _, f := range found {
			if !slices.Contains(mustSee[p], f) {
				t.Errorf("%s.example.json names %s/%s, which mustSee does not give %s", p, etc, f, p)
			}
		}
	}
	for p, f := range map[string]string{"agent": "cloud.secret", "console": "hub.json", "maint": "maint.key", "zigbee": "profiles"} {
		if !slices.Contains(named[p], f) {
			t.Errorf("the scan does not see %s in %s.example.json: %v", f, p, named[p])
		}
	}
}

// No safehome unit sees Zigbee2MQTT's data (the Zigbee network's key), absent on a hub
// without Zigbee, hence the -.
func TestNoUnitSeesTheZigbeeNetworkKey(t *testing.T) {
	data := readUnit(t, filepath.Join("systemd", "zigbee2mqtt.service")).value(t, "ReadWritePaths")
	if data == "" {
		t.Fatal("zigbee2mqtt.service: no ReadWritePaths")
	}
	for _, p := range programs(t) {
		u := programUnit(t, p)
		if !slices.Contains(fields(u["Service"]["InaccessiblePaths"]), "-"+data) || u.sees(data+"/configuration.yaml") {
			t.Errorf("%s: sees %s, or hides it without -", p, data)
		}
	}
}

// stateReaders lists the folders of the programs' state directory (/var/lib/safehome,
// 29 October 2026, second phase) with the programs allowed to read them: every other
// program's unit hides the folder. The names come from the code.
var stateReaders = map[string][]string{
	agentconfig.KeyFolder: {"agent"}, // the key for the codes sealed with the cloud
}

func TestEveryFolderOfTheStateIsHiddenFromTheOtherUnits(t *testing.T) {
	// The state folder as the example names it (agentconfig.Load would want its secret).
	b, err := os.ReadFile(filepath.Join("..", "config", "agent.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		StateDir string `json:"state_dir"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.StateDir == "" {
		t.Fatalf("agent.example.json: no state_dir (%v)", err)
	}
	for _, p := range programs(t) {
		t.Run(p, func(t *testing.T) {
			u := programUnit(t, p)
			for _, d := range slices.Sorted(maps.Keys(stateReaders)) {
				dir := filepath.Join(cfg.StateDir, d)
				if seen, reader := u.sees(dir), slices.Contains(stateReaders[d], p); seen != reader {
					t.Errorf("sees %s: %v, but it is for %v alone", dir, seen, stateReaders[d])
				}
			}
		})
	}
}
