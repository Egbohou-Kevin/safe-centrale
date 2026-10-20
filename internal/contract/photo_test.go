package contract

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func goodPhotoUpload(exp time.Time) PhotoUploadData {
	return PhotoUploadData{Photo: "p1", URL: "https://api.safehome.example/v1/photos/p1", Token: "tok-photo-secret", Exp: TS(exp)}
}

func TestPhotoMessagesAreChecked(t *testing.T) {
	exp := time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)
	if err := (PhotoTakenData{Photo: "p", Incident: "i", Camera: "c", Bytes: 1, Width: 640}).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, d := range map[string]PhotoTakenData{
		"no photo":  {Incident: "i", Camera: "c", Bytes: 1, Width: 1},
		"no size":   {Photo: "p", Incident: "i", Camera: "c", Width: 1},
		"no width":  {Photo: "p", Incident: "i", Camera: "c", Bytes: 1},
		"no camera": {Photo: "p", Incident: "i", Bytes: 1, Width: 1},
	} {
		if d.Validate() == nil {
			t.Errorf("taken %s: accepted", name)
		}
	}
	if err := goodPhotoUpload(exp).Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*PhotoUploadData){
		"http":        func(d *PhotoUploadData) { d.URL = "http://x.example/p" },
		"query":       func(d *PhotoUploadData) { d.URL = "https://x.example/p?sig=1" },
		"credentials": func(d *PhotoUploadData) { d.URL = "https://u:pw@x.example/p" },
		"no host":     func(d *PhotoUploadData) { d.URL = "https:///p" },
		"no token":    func(d *PhotoUploadData) { d.Token = "" },
		"no exp":      func(d *PhotoUploadData) { d.Exp = Timestamp{} },
		"no photo":    func(d *PhotoUploadData) { d.Photo = "" },
	} {
		d := goodPhotoUpload(exp)
		change(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("upload %s: accepted", name)
		} else if strings.Contains(err.Error(), "tok-photo-secret") {
			t.Errorf("upload %s: the token is in the error", name)
		}
	}
	for _, ok := range []PhotoSentData{{Photo: "p", Status: PhotoSent}, {Photo: "p", Status: PhotoFailed, Reason: PhotoTooOld}} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []PhotoSentData{{Status: PhotoSent}, {Photo: "p", Status: "lost"}, {Photo: "p", Status: PhotoFailed},
		{Photo: "p", Status: PhotoSent, Reason: PhotoMissing}} {
		if bad.Validate() == nil {
			t.Errorf("%+v: accepted", bad)
		}
	}
}

func TestThePhotoTokenIsNeverPrinted(t *testing.T) {
	d := goodPhotoUpload(time.Now())
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("upload", "order", d)
	for _, s := range []string{fmt.Sprint(d), fmt.Sprintf("%v %+v %#v %s", d, d, d, d), logs.String()} {
		if strings.Contains(s, "tok-photo-secret") {
			t.Fatalf("token printed: %s", s)
		}
	}
	if !strings.Contains(logs.String(), "api.safehome.example") {
		t.Fatalf("the address may be shown: %s", logs.String())
	}
}
