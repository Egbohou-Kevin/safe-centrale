package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestVideoRequestValidate(t *testing.T) {
	exp := time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC)
	if err := goodVideoRequest(exp).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*VideoRequestData){
		"no session":      func(d *VideoRequestData) { d.Session = "" },
		"no camera":       func(d *VideoRequestData) { d.Camera = "" },
		"bad reason":      func(d *VideoRequestData) { d.Reason = "curious" },
		"negative length": func(d *VideoRequestData) { d.MaxS = -1 },
		"too long":        func(d *VideoRequestData) { d.MaxS = MaxVideoLength + 1 },
		"no token":        func(d *VideoRequestData) { d.Token = "" },
		"no exp":          func(d *VideoRequestData) { d.Exp = Timestamp{} },
		"plain rtmp":      func(d *VideoRequestData) { d.PushURL = "rtmp://relay.example/x" },
		"https":           func(d *VideoRequestData) { d.PushURL = "https://relay.example/x" },
		"no host":         func(d *VideoRequestData) { d.PushURL = "rtmps:///x" },
		"credentials":     func(d *VideoRequestData) { d.PushURL = "rtmps://user:pw@relay.example/x" },
		"query":           func(d *VideoRequestData) { d.PushURL = "rtmps://relay.example/x?token=t" },
		"not a URL":       func(d *VideoRequestData) { d.PushURL = "://" },
	} {
		d := goodVideoRequest(exp)
		mutate(&d)
		err := d.Validate()
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), "tok-secret-123") {
			t.Errorf("%s: the error leaks the token", name)
		}
	}
	unlimited := goodVideoRequest(exp)
	unlimited.MaxS = 0
	if err := unlimited.Validate(); err != nil {
		t.Fatalf("no limit on a fixed line: %v", err)
	}
	if (VideoStopData{Session: "s"}).Validate() != nil {
		t.Fatal("a session is enough")
	}
	if err := (VideoStopData{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestTheVideoTokenIsNeverPrinted(t *testing.T) {
	d := goodVideoRequest(time.Date(2026, 10, 17, 21, 6, 5, 0, time.UTC))
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("request", "data", d)
	for _, out := range []string{fmt.Sprint(d), fmt.Sprintf("%v", d), fmt.Sprintf("%+v", d), fmt.Sprintf("%#v", d), logs.String()} {
		if strings.Contains(out, "tok-secret-123") || !strings.Contains(out, "cam_caisse") {
			t.Errorf("%q", out)
		}
	}
	// The JSON sent on the wire still carries it: the site needs it.
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), "tok-secret-123") {
		t.Fatal("the token is part of the message")
	}
}
