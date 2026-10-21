package contract

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func intPtrC(v int) *int { return &v }

func goodConfig() ConfigSetData {
	return ConfigSetData{
		Version: 7, SiteName: "Boutique Lome",
		Contacts: []ConfigContact{
			{ID: "u_owner", Name: "Proprietaire", Role: "owner", Channels: []string{ChannelApp, ChannelSMS, ChannelCall}, Phone: "+22890000001"},
			{ID: "u_mgr", Name: "Gerant", Role: "manager", Channels: []string{ChannelApp}},
		},
		Closing:    map[string]string{"daily": "20:00", "sun": "off"},
		ExitDelayS: 120, EntryDelayS: 45,
		Video: &ConfigVideo{QuotaMB: intPtrC(500), MaxS: intPtrC(0)},
	}
}

func TestConfigSetValidate(t *testing.T) {
	if err := goodConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (ConfigSetData{Version: 1}).Validate(); err != nil {
		t.Fatalf("only the version is required: %v", err)
	}
	for name, mutate := range map[string]func(*ConfigSetData){
		"no version":        func(d *ConfigSetData) { d.Version = 0 },
		"short exit delay":  func(d *ConfigSetData) { d.ExitDelayS = 5 },
		"long exit delay":   func(d *ConfigSetData) { d.ExitDelayS = MaxExitDelayS + 1 },
		"long entry delay":  func(d *ConfigSetData) { d.EntryDelayS = MaxEntryDelayS + 1 },
		"bad contact id":    func(d *ConfigSetData) { d.Contacts[0].ID = "Owner" },
		"repeated contact":  func(d *ConfigSetData) { d.Contacts[1].ID = "u_owner" },
		"no name":           func(d *ConfigSetData) { d.Contacts[0].Name = "" },
		"unknown role":      func(d *ConfigSetData) { d.Contacts[0].Role = "king" },
		"no channel":        func(d *ConfigSetData) { d.Contacts[0].Channels = nil },
		"unknown channel":   func(d *ConfigSetData) { d.Contacts[1].Channels = []string{"pigeon"} },
		"local phone":       func(d *ConfigSetData) { d.Contacts[0].Phone = "90000001" },
		"sms without phone": func(d *ConfigSetData) { d.Contacts[1].Channels = []string{ChannelSMS} },
		"negative quota":    func(d *ConfigSetData) { d.Video.QuotaMB = intPtrC(-1) },
		"huge quota":        func(d *ConfigSetData) { d.Video.QuotaMB = intPtrC(MaxQuotaMB + 1) },
		"session too long":  func(d *ConfigSetData) { d.Video.MaxS = intPtrC(MaxVideoLength + 1) },
		"too many contacts": func(d *ConfigSetData) {
			for i := range MaxContacts {
				d.Contacts = append(d.Contacts, ConfigContact{ID: fmt.Sprintf("u_%d", i), Name: "x", Role: "other", Channels: []string{ChannelApp}})
			}
		},
	} {
		d := goodConfig()
		mutate(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "2289000") {
			t.Errorf("%s: the error shows a phone number: %v", name, err)
		}
	}
}

func TestTheConfigNeverShowsPhones(t *testing.T) {
	d := goodConfig()
	var logs strings.Builder
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("config received", "config", d)
	for _, s := range []string{fmt.Sprint(d), fmt.Sprintf("%+v", d), fmt.Sprintf("%#v", d), logs.String()} {
		if strings.Contains(s, "+228") {
			t.Errorf("a phone shows in %q", s)
		}
	}
}

func TestConfigAppliedValidate(t *testing.T) {
	for _, ok := range []ConfigAppliedData{{Version: 3, Status: ConfigApplied}, {Version: 0, Status: ConfigRejected, Reason: "x"}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []ConfigAppliedData{{Version: 3, Status: "maybe"}, {Version: 3, Status: ConfigRejected}, {Version: -1, Status: ConfigApplied}} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if ConfigSetTopic("tg-lom-0001") != "safehome/tg-lom-0001/config/set" || ConfigAppliedTopic("s") != "safehome/s/config/applied" {
		t.Fatal("topics")
	}
}
