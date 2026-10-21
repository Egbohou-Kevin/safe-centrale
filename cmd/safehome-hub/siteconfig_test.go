package main

import (
	"strings"
	"testing"
	"time"

	"safehome/internal/adapter/mqtt/mqtttest"
	"safehome/internal/contract"
	"safehome/internal/driver/drivertest"
	"safehome/internal/hubconfig"
	"safehome/internal/install"
	"safehome/internal/siteconfig"
)

func TestAConfigurationFromTheCloudRestartsTheHubThenIsApplied(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")

	// Without an owner: refused at once, nothing kept, no restart.
	l.publish(contract.ConfigSetTopic(site), contract.TypeConfigSet, contract.ConfigSetData{Version: 2, Contacts: []contract.ConfigContact{
		{ID: "u_guard", Name: "Kofi", Role: "guard", Channels: []string{"sms"}, Phone: "+22870000002"},
	}}, false)
	if d := l.waitApplied(1); d.Version != 2 || d.Status != contract.ConfigRejected || !strings.Contains(d.Reason, "owner") {
		t.Fatalf("%+v", d)
	}
	if _, ok, _ := siteconfig.Read(state); ok {
		t.Fatal("a refused configuration was kept")
	}

	// A good one: kept, then the hub stops for systemd to start it again.
	l.publish(contract.ConfigSetTopic(site), contract.TypeConfigSet, contract.ConfigSetData{
		Version: 3, SiteName: "Boutique Be", EntryDelayS: 20,
		Contacts: []contract.ConfigContact{
			{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"app", "sms"}, Phone: "+22890000001"},
			{ID: "u_night", Name: "Kofi", Role: "guard", Channels: []string{"sms"}, Phone: "+22870000002"},
		},
	}, false)
	select {
	case code := <-l.done:
		if code != 0 {
			t.Fatalf("exit %d: systemd must see a clean stop", code)
		}
		l.cancel = nil
	case <-time.After(15 * time.Second):
		t.Fatalf("the hub did not restart\nlogs:\n%s", l.logs.String())
	}
	if d, ok, err := siteconfig.Read(state); err != nil || !ok || d.Version != 3 {
		t.Fatalf("%v %v %d", err, ok, d.Version)
	}

	// Started again, it runs with it and says so.
	l.startHub()
	if d := l.waitApplied(2); d.Version != 3 || d.Status != contract.ConfigApplied {
		t.Fatalf("%+v", d)
	}
	code, out, _ := cli(t, "", "check", "-config", cfg)
	if code != 0 || !strings.Contains(out, "2 contact(s)") || !strings.Contains(out, "from the cloud: version 3") {
		t.Fatalf("%d %q", code, out)
	}
	// The entry delay of 20 s from the cloud: an entry, then the alarm only after it.
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.waitState("arming")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_door", contract.CapContact, "open", "")
	l.waitCount(contract.TypeAlarmEntryDelay, 1)
	env := l.seen(contract.TypeAlarmEntryDelay)[0]
	ed, _ := contract.DecodeData[contract.EntryDelayData](env)
	if got := ed.EndsAt.Sub(env.TS.Time).Round(time.Second); got != 20*time.Second {
		t.Fatalf("entry delay %v, the cloud said 20 s", got)
	}

	if strings.Contains(l.logs.String(), "0000001") || strings.Contains(l.logs.String(), "0000002") {
		t.Fatalf("a phone number in the logs:\n%s", l.logs.String())
	}
	l.validateEverything()
}

func TestTheInstallationModeAppliesNewZonesByRestarting(t *testing.T) {
	broker := mqtttest.Start(t)
	state := t.TempDir()
	cfg := writeConfig(t, broker.URL(), state, "")
	if code, _, errb := cli(t, "259031\n", "codes", "-config", cfg, "set", "installer"); code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	l := startLive(t, broker, state, cfg)
	l.waitState("disarmed")

	// The installer code at the keypad opens the mode; the console changes the zones,
	// then closes the mode: the hub stops for systemd to start it again.
	l.event("dev_keypad", contract.CapKeypad, "disarm", "259031")
	drivertest.WaitFor(t, "the installation mode", func() bool {
		for _, env := range l.seen(contract.TypeInstallMode) {
			if d, _ := contract.DecodeData[contract.InstallModeData](env); d.Status == contract.InstallOpen {
				return true
			}
		}
		return false
	})
	devices, err := hubconfig.ReadSite(cfg)
	if err != nil {
		t.Fatal(err)
	}
	devices.Zones["dev_k7p2"] = install.Zone{Name: "Reserve", Kind: "instant"}
	if err := hubconfig.WriteSite(cfg, devices); err != nil {
		t.Fatal(err)
	}
	// A configuration from the cloud meanwhile is kept, but waits for the mode's end:
	// a restart now would close the mode under the installer's feet.
	l.publish(contract.ConfigSetTopic(site), contract.TypeConfigSet, contract.ConfigSetData{Version: 3, Contacts: []contract.ConfigContact{
		{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"app", "sms"}, Phone: "+22890000001"},
		{ID: "u_night", Name: "Kofi", Role: "guard", Channels: []string{"sms"}, Phone: "+22870000002"},
	}}, false)
	drivertest.WaitFor(t, "the configuration kept", func() bool { _, ok, _ := siteconfig.Read(state); return ok })
	// The contact it adds gets a code on the console: the running hub takes it at once.
	if code, _, errb := cli(t, "730264\n", "codes", "-config", cfg, "set", "u_night"); code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	drivertest.WaitFor(t, "the reload", func() bool { return strings.Count(l.logs.String(), "access codes reloaded") >= 2 })
	l.event("dev_keypad", contract.CapKeypad, "disarm", "730264")
	l.waitCount(contract.TypeAlarmAck, 2)
	if n := len(l.seen(contract.TypeAlarmAuthFailed)); n != 0 {
		t.Fatalf("the code of a contact added during the installation mode was refused\nlogs:\n%s", l.logs.String())
	}
	select {
	case <-l.done:
		t.Fatalf("the hub restarted during the installation mode\nlogs:\n%s", l.logs.String())
	case <-time.After(time.Second):
	}
	l.publish(contract.InstallCmdTopic(site), contract.TypeInstallClose, contract.InstallCloseData{}, false)
	select {
	case code := <-l.done:
		if code != 0 {
			t.Fatalf("exit %d: systemd must see a clean stop", code)
		}
		l.cancel = nil
	case <-time.After(15 * time.Second):
		t.Fatalf("the hub did not restart\nlogs:\n%s", l.logs.String())
	}

	// Started again, it watches the new zone.
	l.startHub()
	l.waitState("disarmed")
	l.event("dev_keypad", contract.CapKeypad, "arm", "")
	l.event("dev_keypad", contract.CapKeypad, "arm_now", "")
	l.waitState("armed")
	l.event("dev_k7p2", contract.CapContact, "open", "")
	l.waitState("triggered")
	if code, out, _ := cli(t, "", "check", "-config", cfg); code != 0 || !strings.Contains(out, "set by the installer's tools") {
		t.Fatalf("%d %q", code, out)
	}
	if strings.Contains(l.logs.String(), "259031") || strings.Contains(l.logs.String(), "730264") {
		t.Fatal("a code shows in the logs")
	}
	l.validateEverything()
}
