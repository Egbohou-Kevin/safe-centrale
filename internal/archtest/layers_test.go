package archtest

import (
	"go/ast"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The domain cores are pure: they know no wire format, no adapter and no I/O, and
// they never read the clock. This is what makes them testable with a simulated
// clock.
func TestDomainCoresArePure(t *testing.T) {
	pure := layer{
		noStd:   []string{"os", "os/exec", "io/fs", "net", "net/http", "database/sql", "syscall", "log", "log/slog", "encoding/json"},
		noClock: true,
		why:     ": the domain depends on nothing but the standard library, without I/O, and inputs carry their own time",
	}
	for _, pkg := range []string{"engine", "health", "access", "escalation", "gsm", "zigbee", "video", "maint"} {
		pure.check(t, pkg)
	}
}

// The contract is the published language: it depends on nothing in the module.
func TestContractDependsOnNothing(t *testing.T) {
	layer{why: ": the contract must stay standard-library only"}.check(t, "contract")
}

// The driver runs a core in real time but knows no core: it is generic, and
// describes what it needs with functions. Adapters know the library or the disk they
// wrap and nothing of the domains, the contract or the driver. In both, the
// dependency arrows point one way only: toward the standard library (and, for an
// adapter, its own library). The driver's test fakes are held to the same rule, so
// that the driver's own tests can use them.
func TestDriverAndAdaptersKnowNoDomain(t *testing.T) {
	dirs := []string{filepath.Join("internal", "driver"), filepath.Join("internal", "driver", "drivertest")}
	// Every adapter, including those added later.
	entries, err := os.ReadDir(filepath.Join(root(t), "internal", "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join("internal", "adapter", e.Name()))
		}
	}
	if len(dirs) < 3 {
		t.Fatalf("found only %v: the adapters moved?", dirs)
	}
	for _, dir := range dirs {
		_, files := parsePackage(t, filepath.Join(root(t), dir))
		for name, f := range files {
			if bad := forbiddenImports(f, isLocal); len(bad) > 0 {
				t.Errorf("%s/%s imports %v: it must not know the domains, the contract or another package of the module", dir, name, bad)
			}
		}
	}
}

// The outbox serves every application layer of the site's programs (hub, gateway,
// zigbeeapp, agentapp, videoapp, maintapp, configapp): it builds envelopes, so it
// knows the contract, and saves its counter through the driver's Store port. It
// knows no domain and no application.
// Its fake broker, for the applications' tests, knows only the outbox and the
// contract.
func TestOutboxKnowsOnlyTheContractAndTheStorePort(t *testing.T) {
	layer{local: internal("contract", "driver")}.check(t, "outbox")
	layer{local: internal("contract", "outbox")}.check(t, filepath.Join("outbox", "outboxtest"))
}

// The inbox is the way in of every application layer, the console and the
// installer's tools included: it checks messages against the contract and knows
// nothing else. It does no I/O and never reads the clock.
func TestInboxKnowsOnlyTheContract(t *testing.T) {
	layer{
		local:   internal("contract"),
		noStd:   []string{"os", "os/exec", "io/fs", "net", "net/http", "syscall"},
		noClock: true,
	}.check(t, "inbox")
}

// The agent's core moves contract messages: it knows the contract and nothing else,
// does no I/O and never reads the clock. Its application layer adds the driver, the
// inbox and the outbox, and no adapter.
func TestTheAgentCoreKnowsOnlyTheContract(t *testing.T) {
	layer{local: internal("contract"), noClock: true}.check(t, "agent")
	layer{local: internal("agent", "contract", "driver", "inbox", "outbox")}.check(t, "agentapp")
}

// The video service's application layer drives the streams through its own
// Streamer port: it knows the video core, its translation, the contract, the driver,
// the inbox and the outbox, and no adapter (go2rtc is wired by cmd/safehome-video).
func TestVideoAppKnowsNoAdapter(t *testing.T) {
	layer{local: internal("video", "video/wire", "contract", "driver", "inbox", "outbox")}.check(t, "videoapp")
}

// Remote maintenance's application layer opens the tunnel through its own Tunneler
// port: it knows the maintenance core, its translation, the contract, the driver,
// the inbox and the outbox, and no adapter (ssh is wired by cmd/safehome-maint). It
// never reads the clock: the session's end is the core's.
func TestMaintAppKnowsNoAdapter(t *testing.T) {
	layer{
		local:   internal("maint", "maint/wire", "contract", "driver", "inbox", "outbox"),
		noStd:   []string{"os", "os/exec", "net", "syscall"},
		noClock: true,
	}.check(t, "maintapp")
}

// The configuration set from the cloud: the application that receives it knows no
// file and no process (checking, keeping and restarting are ports, wired by
// cmd/safehome-hub), and never reads the clock. The file that keeps it knows only the
// contract, and writes through the configuration files' helpers.
func TestTheCloudConfigurationKnowsNoAdapter(t *testing.T) {
	layer{
		local:   internal("contract", "driver", "inbox", "outbox"),
		noStd:   []string{"os", "os/exec", "io/fs", "syscall"},
		noClock: true,
	}.check(t, "configapp")
	layer{local: internal("contract", "configfile")}.check(t, "siteconfig")
}

// configfile holds what the configuration packages share (strict decoding, the log
// level, durations, secrets, atomic writes). It knows nothing of the module and never
// reads the clock. Only the configuration packages use it: it reads files, so no
// domain or application may.
func TestConfigFileServesOnlyTheConfigurationPackages(t *testing.T) {
	layer{noClock: true, why: ": configfile is standard-library only"}.check(t, "configfile")
	users := []string{"agentconfig", "consoleconfig", "gsmconfig", "hubconfig", "maintconfig", "siteconfig", "videoconfig", "zigbeeconfig"}
	configfile := module + "/internal/configfile"
	walkSources(t, func(rel string, f *ast.File) {
		dir, name := filepath.Split(filepath.Dir(rel))
		if dir == "internal"+string(filepath.Separator) && slices.Contains(users, name) {
			return
		}
		if slices.Contains(importsOf(f), configfile) {
			t.Errorf("%s imports %s: only the configuration packages may", rel, configfile)
		}
	})
}

// The installer's application layer talks to the broker through its own ports and
// knows the site only as zones and sirens: no adapter, no configuration file, no hub.
// The command-line tool and the web console both sit on it. It reads no clock
// either: the program gives it one.
func TestInstallKnowsOnlyTheContractAndTheZoneKinds(t *testing.T) {
	layer{local: internal("contract", "engine", "inbox"), noClock: true}.check(t, "install")
}

// The local console's application drives the network, the page and the hub's files
// through its own ports, and the devices through the installer's application: no
// adapter, no file, no process, no clock of its own. Its web page knows only it, and
// serves HTTP without touching the system.
func TestTheConsoleKnowsNoAdapter(t *testing.T) {
	layer{
		local:   internal("contract", "driver", "inbox", "install"),
		noStd:   []string{"os", "os/exec", "io/fs", "net", "net/http", "syscall"},
		noClock: true,
	}.check(t, "consoleapp")
	layer{
		local:   internal("consoleapp"),
		noStd:   []string{"os", "os/exec", "io/fs", "syscall"},
		noClock: true,
	}.check(t, filepath.Join("consoleapp", "web"))
}

// The driver is where the real clock lives, but it takes it through the Clock
// interface: only SystemClock may read it.
func TestDriverReadsTheClockOnlyInSystemClock(t *testing.T) {
	fset, files := parsePackage(t, filepath.Join(root(t), "internal", "driver"))
	var total int
	for name, f := range files {
		calls := clockCalls(fset, f)
		total += len(calls)
		for _, c := range calls {
			if !strings.Contains(c, "time.Now") && !strings.Contains(c, "time.NewTimer") {
				t.Errorf("internal/driver/%s uses the clock in an unexpected way: %s", name, c)
			}
		}
	}
	// Exactly two places: SystemClock.Now and SystemClock.Timer.
	if total != 2 {
		t.Errorf("internal/driver reads the clock in %d places, want 2 (SystemClock.Now and SystemClock.Timer)", total)
	}
}

// The application layer (hub) assembles the domains through ports it declares
// itself. It never imports an adapter: wiring the real MQTT client or the real disk
// to those ports is the job of the composition root (cmd/, with internal/service
// that only cmd/ may use, see the next rule), and nothing else.
func TestOnlyTheCompositionRootImportsAdapters(t *testing.T) {
	adapters := module + "/internal/adapter/"
	walkSources(t, func(rel string, f *ast.File) {
		if strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) ||
			strings.HasPrefix(rel, filepath.Join("internal", "service")+string(filepath.Separator)) ||
			strings.HasPrefix(rel, filepath.Join("internal", "adapter")+string(filepath.Separator)) {
			return // the composition root and the adapters themselves
		}
		for _, imp := range importsOf(f) {
			if strings.HasPrefix(imp, adapters) {
				t.Errorf("%s imports %s: only cmd/ may wire adapters", rel, imp)
			}
		}
	})
}

// internal/service is the part of the composition root every program shares: the
// command line, the pid file, the MQTT clients and the life cycle under systemd. It
// knows those adapters, the inbox's message and the outbox's error, nothing of the
// domains, the applications or the configuration files; and only cmd/ imports it.
func TestServiceIsSharedAssemblyForThePrograms(t *testing.T) {
	layer{
		local: internal("adapter/filestore", "adapter/mqtt", "adapter/pidfile", "adapter/sdnotify", "inbox", "outbox"),
		why:   ": the service package only assembles adapters for cmd/",
	}.check(t, "service")
	service := module + "/internal/service"
	walkSources(t, func(rel string, f *ast.File) {
		if strings.HasPrefix(rel, "cmd"+string(filepath.Separator)) {
			return
		}
		if slices.Contains(importsOf(f), service) {
			t.Errorf("%s imports %s: only cmd/ may", rel, service)
		}
	})
}

// The domains know nothing of the application layer, the driver or the adapters
// (this already follows from "a domain imports nothing of the module", and is
// stated here for the hub, which sits above them).
func TestNoDomainImportsTheHub(t *testing.T) {
	hub := module + "/internal/hub"
	for _, pkg := range []string{"engine", "health", "access", "escalation", "gsm", "zigbee", "video", "maint", "contract", "driver"} {
		_, files := parsePackage(t, filepath.Join(root(t), "internal", pkg))
		for name, f := range files {
			if slices.Contains(importsOf(f), hub) {
				t.Errorf("internal/%s/%s imports the hub", pkg, name)
			}
		}
	}
}

// Each wire package is the only bridge between one domain core and the contract.
// It does no I/O.
func TestWireOnlyBridgesItsDomainAndTheContract(t *testing.T) {
	for _, domain := range []string{"engine", "health", "escalation", "gsm", "zigbee", "video", "maint"} {
		layer{
			local: internal(domain, "contract"),
			noStd: []string{"os", "net", "net/http", "database/sql"},
		}.check(t, filepath.Join(domain, "wire"))
	}
}

// The z2m package is the only one that knows Zigbee2MQTT's messages: it bridges them
// to the zigbee core, knows nothing of the contract, and does no I/O.
func TestZ2MOnlyBridgesZigbee2MQTTAndTheCore(t *testing.T) {
	layer{
		local: internal("zigbee"),
		noStd: []string{"os", "net", "net/http", "database/sql", "log", "log/slog"},
	}.check(t, filepath.Join("zigbee", "z2m"))
}

// Every application layer reads and waits on time only through the driver's Clock
// port, so that its tests run on a fake clock: the hub, the gateway and every package
// named *app, including those added later (several also have a rule of their own
// above).
func TestApplicationLayersUseOnlyTheClockPort(t *testing.T) {
	dirs := []string{"hub", "gateway"}
	entries, err := os.ReadDir(filepath.Join(root(t), "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), "app") {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) < 8 {
		t.Fatalf("found only %v: the application layers moved?", dirs)
	}
	for _, dir := range dirs {
		fset, files := parsePackage(t, filepath.Join(root(t), "internal", dir))
		for name, f := range files {
			for _, c := range clockCalls(fset, f) {
				t.Errorf("internal/%s/%s reads the clock (%s): an application layer takes time from its driver.Clock port", dir, name, c)
			}
		}
	}
}
