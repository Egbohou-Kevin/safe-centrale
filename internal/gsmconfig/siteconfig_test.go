package gsmconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"safehome/internal/contract"
	"safehome/internal/siteconfig"
)

func exampleWithState(t *testing.T) (string, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "gsm.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	data = []byte(strings.Replace(string(data), `"/var/lib/safehome"`, `"`+state+`"`, 1))
	path := filepath.Join(t.TempDir(), "gsm.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, state
}

func TestTheCloudsPhoneNumbersReplaceTheFiles(t *testing.T) {
	path, state := exampleWithState(t)
	base, err := Load(path)
	if err != nil || base.SiteVersion != 0 || len(base.Gateway.GSM.Contacts) != 4 {
		t.Fatalf("%v %+v", err, base.Gateway.GSM.Contacts)
	}
	siteconfig.Write(state, contract.ConfigSetData{Version: 6, Contacts: []contract.ConfigContact{
		{ID: "u_owner", Name: "Ama", Role: "owner", Channels: []string{"sms"}, Phone: "+22891112233"},
		{ID: "u_mgr", Name: "Efua", Role: "manager", Channels: []string{"app"}}, // no phone: no GSM
	}})
	r, err := Load(path)
	if err != nil || r.SiteErr != nil || r.SiteVersion != 6 {
		t.Fatalf("%v %v %d", err, r.SiteErr, r.SiteVersion)
	}
	c := r.Gateway.GSM.Contacts
	if len(c) != 1 || !strings.HasSuffix(c["u_owner"], "91112233") {
		t.Fatalf("%v", c)
	}
	if len(r.Ports) != len(base.Ports) || r.Gateway.GSM.TestNumber != base.Gateway.GSM.TestNumber {
		t.Fatal("the rest of the file must not change")
	}

	// Without contacts, the file's numbers stay.
	siteconfig.Write(state, contract.ConfigSetData{Version: 7, SiteName: "x"})
	if r, _ := Load(path); r.SiteVersion != 7 || len(r.Gateway.GSM.Contacts) != 4 {
		t.Fatalf("%+v", r.Gateway.GSM.Contacts)
	}

	// A damaged file is left aside, with the reason.
	os.WriteFile(filepath.Join(state, siteconfig.FileName), []byte("{"), 0o600)
	if r, err := Load(path); err != nil || r.SiteErr == nil || len(r.Gateway.GSM.Contacts) != 4 {
		t.Fatalf("%v %v", err, r.SiteErr)
	}
}
