package hubconfig

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"safehome/internal/shared/config/hubtool"
	sitemodel "safehome/internal/shared/core/site"
)

func tool(path string, in string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := SiteTool(path, args, strings.NewReader(in), &out, &errb)
	return code, out.String(), errb.String()
}

func TestTheToolViewsTheConfigurationWithoutItsSecrets(t *testing.T) {
	path, stateDir := copyExample(t)
	code, out, errs := tool(path, "", hubtool.View)
	if code != 0 {
		t.Fatalf("view: %d %s", code, errs)
	}
	var v hubtool.Config
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v, ViewOf(r)) || v.StateDir != stateDir || v.Site == "" || len(v.Contacts) == 0 || v.Access.MaxUsers == 0 {
		t.Fatalf("view %+v", v)
	}
	if strings.Contains(out, "password") {
		t.Fatalf("the view says a password: %s", out)
	}
}

func TestTheToolReadsChecksAndWritesTheSite(t *testing.T) {
	path, _ := copyExample(t)
	code, out, errs := tool(path, "", hubtool.Read)
	if code != 0 {
		t.Fatalf("read: %d %s", code, errs)
	}
	var s hubtool.Site
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatal(err)
	}
	want, _ := ReadSite(path)
	if !reflect.DeepEqual(s.Model(), want) {
		t.Fatalf("read %+v, the file has %+v", s.Model(), want)
	}

	s.Zones["dev_new"] = hubtool.Zone{Name: "Garage", Kind: "entry"}
	in, _ := json.Marshal(s)
	if code, _, errs := tool(path, string(in), hubtool.Check); code != 0 {
		t.Fatalf("check: %d %s", code, errs)
	}
	if got, _ := ReadSite(path); !reflect.DeepEqual(got, want) {
		t.Fatal("check wrote the site")
	}
	if code, _, errs := tool(path, string(in), hubtool.Write); code != 0 {
		t.Fatalf("write: %d %s", code, errs)
	}
	if got, _ := ReadSite(path); got.Zones["dev_new"] != (sitemodel.Zone{Name: "Garage", Kind: "entry"}) {
		t.Fatalf("written %+v", got.Zones)
	}
}

func TestTheToolRefusesWhatTheHubWouldRefuse(t *testing.T) {
	path, _ := copyExample(t)
	bad := `{"zones":{"dev_x":{"name":"X","kind":"garage"}},"siren_ids":[]}`
	for _, c := range []struct {
		name, in string
		args     []string
		code     int
	}{
		{"an unknown zone kind", bad, []string{hubtool.Check}, 1},
		{"an unknown zone kind, written", bad, []string{hubtool.Write}, 1},
		{"an unknown field", `{"zones":{},"siren_ids":[],"extra":1}`, []string{hubtool.Write}, 1},
		{"too long a site", strings.Repeat(" ", maxSiteInput+1), []string{hubtool.Check}, 1},
		{"an unknown verb", "", []string{"delete"}, 1},
		{"no verb", "", nil, 2},
		{"two verbs", "", []string{hubtool.Read, hubtool.View}, 2},
	} {
		code, out, errs := tool(path, c.in, c.args...)
		if code != c.code || out != "" || errs == "" {
			t.Errorf("%s: code %d, out %q, err %q", c.name, code, out, errs)
		}
	}
	if code, _, _ := tool(path+".missing", "", hubtool.View); code != 1 {
		t.Errorf("a missing file: code %d", code)
	}
}
