package videoconfig

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/siteconfig"
	"safehome/internal/video"
	"safehome/internal/videoapp"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "video.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheExampleIsValidAndRepeatsTheDefaults(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "video.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := videoapp.DefaultConfig("tg-lom-0001", video.DefaultConfig([]video.Camera{{ID: "cam_caisse", Name: "Caisse"}}))
	if !reflect.DeepEqual(r.App, want) {
		t.Fatalf("the example differs from the defaults:\n%+v\n%+v", r.App, want)
	}
	if r.Go2rtc != "/usr/local/bin/go2rtc" || r.BrokerURL != "mqtt://127.0.0.1:1883" || r.StateDir != "/var/lib/safehome" || r.LogLevel != slog.LevelInfo {
		t.Fatalf("%v", r)
	}
	if !strings.HasPrefix(r.Sources["cam_caisse"], "rtsp://") {
		t.Fatalf("%v", r.Sources)
	}
}

func TestTheCamerasAddressesNeverShow(t *testing.T) {
	r, err := Load(filepath.Join("..", "..", "config", "video.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{fmt.Sprint(r), fmt.Sprintf("%+v", r), fmt.Sprintf("%#v", r)} {
		if strings.Contains(s, "CHANGE-ME") || strings.Contains(s, "rtsp") {
			t.Errorf("an address shows in %q", s)
		}
	}
}

func cfg(extra string) string {
	return `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a", "source": "rtsp://u:pw@10.0.0.2/s"}]` + extra + `}`
}

func TestMobileDataDefaultsAndOverrides(t *testing.T) {
	r, err := Load(write(t, cfg(`, "uplink": "mobile"`)))
	if err != nil {
		t.Fatal(err)
	}
	if v := r.App.Video; !v.Mobile || v.Quota != 1<<30 || v.MaxLength != 5*time.Minute {
		t.Fatalf("%+v", v)
	}
	r, err = Load(write(t, cfg(`, "uplink": "mobile", "quota_mb": 500, "max_session": "2m", "max_sessions": 1, "probe_every": "1m", "log_level": "debug", "go2rtc": "/opt/go2rtc"`)))
	if err != nil {
		t.Fatal(err)
	}
	if v := r.App.Video; v.Quota != 500<<20 || v.MaxLength != 2*time.Minute || v.MaxSessions != 1 || v.ProbeEvery != time.Minute {
		t.Fatalf("%+v", v)
	}
	if r.Go2rtc != "/opt/go2rtc" || r.LogLevel != slog.LevelDebug || r.ClientID != "safehome-video-tg-lom-0001" {
		t.Fatalf("%v", r)
	}
	r, err = Load(write(t, cfg(`, "uplink": "mobile", "quota_mb": 0, "max_session": "0"`)))
	if err != nil {
		t.Fatal(err)
	}
	if v := r.App.Video; v.Quota != 0 || v.MaxLength != 0 {
		t.Fatalf("the client may lift both: %+v", v)
	}
}

func TestMistakesAreRefused(t *testing.T) {
	for want, body := range map[string]string{
		"state_dir is required":  `{"site": "tg-lom-0001", "cameras": [{"id": "cam_a", "source": "rtsp://h/s"}]}`,
		"at least one camera":    `{"site": "tg-lom-0001", "state_dir": "/tmp/s"}`,
		"lower-case letters":     `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "Cam A", "source": "rtsp://h/s"}]}`,
		"is repeated":            `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a", "source": "rtsp://h/s"}, {"id": "cam_a", "source": "rtsp://h/t"}]}`,
		"source: required":       `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a"}]}`,
		`scheme "http"`:          `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a", "source": "http://u:pw@h/s"}]}`,
		"not a valid address":    `{"site": "tg-lom-0001", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a", "source": "rtsp://"}]}`,
		"apply only":             cfg(`, "quota_mb": 100`),
		"apply only to":          cfg(`, "uplink": "fixed", "max_session": "5m"`),
		`uplink "4g"`:            cfg(`, "uplink": "4g"`),
		"quota_mb from 0":        cfg(`, "uplink": "mobile", "quota_mb": -1`),
		"from 0 (no allowance)":  cfg(`, "uplink": "mobile", "quota_mb": 8388609`),
		"max_session":            cfg(`, "uplink": "mobile", "max_session": "long"`),
		"stall_after":            cfg(`, "stall_after": "soon"`),
		"MaxSessions":            cfg(`, "max_sessions": 0`),
		"log_level":              cfg(`, "log_level": "loud"`),
		"invalid site":           `{"site": "X", "state_dir": "/tmp/s", "cameras": [{"id": "cam_a", "source": "rtsp://h/s"}]}`,
		`unknown field "camera"`: cfg(`, "camera": []`),
		"trailing data":          cfg("") + `{}`,
	} {
		_, err := Load(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want an error with %q", body, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "pw@") {
			t.Errorf("an error shows a password: %v", err)
		}
	}
	if _, err := Load(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing file was accepted")
	}
}

func TestTheClientsVideoSettingsFromTheCloud(t *testing.T) {
	state := t.TempDir()
	body := func(uplink string) string {
		return `{"site": "tg-lom-0001", "state_dir": "` + state + `", "uplink": "` + uplink + `", "cameras": [{"id": "cam_a", "source": "rtsp://u:pw@10.0.0.2/s"}]}`
	}
	q, m := 300, 120
	siteconfig.Write(state, contract.ConfigSetData{Version: 9, Video: &contract.ConfigVideo{QuotaMB: &q, MaxS: &m}})
	r, err := Load(write(t, body("mobile")))
	if err != nil || r.SiteErr != nil || r.SiteVersion != 9 {
		t.Fatalf("%v %v", err, r.SiteErr)
	}
	if v := r.App.Video; v.Quota != 300<<20 || v.MaxLength != 2*time.Minute {
		t.Fatalf("%+v", v)
	}
	r, err = Load(write(t, body("fixed")))
	if err != nil || r.SiteNote == "" || r.App.Video.Quota != 0 || r.SiteVersion != 9 {
		t.Fatalf("on a fixed line the settings are left out: %v %+v", err, r)
	}
	siteconfig.Write(state, contract.ConfigSetData{Version: 10})
	if r, _ := Load(write(t, body("mobile"))); r.App.Video.Quota != video.MobileQuota || r.SiteVersion != 10 {
		t.Fatalf("without video settings, the file's: %+v", r.App.Video)
	}
	huge := contract.MaxQuotaMB + 1
	if err := siteconfig.Write(state, contract.ConfigSetData{Version: 11, Video: &contract.ConfigVideo{QuotaMB: &huge}}); err == nil {
		t.Fatal("the contract lets a huge allowance through")
	}
	// The video service sees the cloud's folder alone (its unit).
	os.Remove(filepath.Join(state, siteconfig.FileName))
	inDir := filepath.Join(state, siteconfig.Dir, siteconfig.FileName)
	os.WriteFile(inDir, fmt.Appendf(nil, `{"version": 11, "video": {"quota_mb": %d}}`, huge), 0o600)
	if r, err := Load(write(t, body("mobile"))); err != nil || r.SiteErr == nil || r.App.Video.Quota != video.MobileQuota {
		t.Fatalf("a huge allowance from the cloud is refused, the file's kept: %v %v %d", err, r.SiteErr, r.App.Video.Quota)
	}
	os.WriteFile(inDir, []byte("{"), 0o600)
	if r, err := Load(write(t, body("mobile"))); err != nil || r.SiteErr == nil {
		t.Fatalf("%v", err)
	}
}

func TestPhotos(t *testing.T) {
	r, err := Load(write(t, cfg("")))
	if err != nil {
		t.Fatal(err)
	}
	if r.App.Video.Photos != video.FixedPhotos || r.FFmpeg != "/usr/bin/ffmpeg" || filepath.Base(r.PhotoDir) != "photos" || filepath.Dir(r.PhotoDir) != r.StateDir ||
		r.VideoDir != filepath.Join(r.StateDir, "video") {
		t.Fatalf("%+v %s %s %s", r.App.Video.Photos, r.FFmpeg, r.PhotoDir, r.VideoDir)
	}
	if r, _ := Load(write(t, cfg(`, "uplink": "mobile", "ffmpeg": "/opt/ffmpeg"`))); r.App.Video.Photos != video.EconomyPhotos || r.FFmpeg != "/opt/ffmpeg" {
		t.Fatalf("economy on mobile data: %+v", r.App.Video.Photos)
	}
	if r, _ := Load(write(t, cfg(`, "photos": false`))); r.App.Video.Photos.Count != 0 {
		t.Fatal("no photos")
	}
}
