package wire

import (
	"encoding/json"
	"testing"
	"time"

	"safehome/internal/contract"
	"safehome/internal/schematest"
	"safehome/internal/video"
)

var now = time.Date(2026, 10, 20, 21, 0, 0, 0, time.UTC)

func TestRequestAddsTheTokenToTheAddress(t *testing.T) {
	d := contract.VideoRequestData{
		Session: "s1", Camera: "cam_caisse", Reason: contract.VideoOnDemand, MaxS: 120,
		PushURL: "rtmps://relay.safehome.example/live/s1", Token: "a b&c=d", Exp: contract.TS(now.Add(30 * time.Second)),
	}
	r := Request(d, now)
	want := video.Request{
		Now: now, Session: "s1", Camera: "cam_caisse", MaxLength: 2 * time.Minute, Exp: now.Add(30 * time.Second),
		Target: "rtmps://relay.safehome.example/live/s1?token=a+b%26c%3Dd",
	}
	if r != want {
		t.Fatalf("%+v\n%+v", r, want)
	}
	if s := Stop(contract.VideoStopData{Session: "s1"}, now); s != (video.Stop{Now: now, Session: "s1"}) {
		t.Fatalf("%+v", s)
	}
}

func TestEveryReportFollowsTheContract(t *testing.T) {
	reports := []video.Report{
		{Session: "s1", Camera: "cam_caisse", Status: video.Starting},
		{Session: "s1", Camera: "cam_caisse", Status: video.Live},
		{Session: "s1", Camera: "cam_caisse", Status: video.Ended, Reason: video.Stopped, Duration: 75*time.Second + 400*time.Millisecond, Bytes: 9_000_000},
	}
	for _, why := range []video.Reason{video.Timeout, video.Quota, video.Busy, video.UnknownCamera, video.CameraUnreachable, video.RelayUnreachable, video.Expired} {
		reports = append(reports, video.Report{Session: "s2", Camera: "cam_cour", Status: video.Failed, Reason: why})
	}
	for _, r := range reports {
		raw, _ := json.Marshal(Session(r))
		if err := schematest.Check("video-session.json", raw); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if d := Session(reports[2]); d.DurationS != 75 || d.Bytes != 9_000_000 || d.Status != contract.VideoEnded || d.Reason != contract.VideoStopped {
		t.Fatalf("%+v", d)
	}
}

func TestStatus(t *testing.T) {
	fixed := Status(video.State{Cameras: []video.CameraState{{ID: "cam_caisse", Name: "Caisse", Reachable: true}}, Used: 300 << 20})
	want := contract.VideoStatusData{
		Cameras: []contract.VideoCamera{{ID: "cam_caisse", Name: "Caisse", Reachable: true}},
		Uplink:  contract.UplinkFixed, UsedMBMonth: 300,
	}
	raw, _ := json.Marshal(fixed)
	if err := schematest.Check("video-status.json", raw); err != nil {
		t.Fatal(err)
	}
	if fixed.Uplink != want.Uplink || fixed.UsedMBMonth != 300 || len(fixed.Cameras) != 1 || fixed.Cameras[0] != want.Cameras[0] {
		t.Fatalf("%+v", fixed)
	}
	mob := Status(video.State{Mobile: true, Active: 1, Quota: video.MobileQuota, MaxLength: video.MobileMaxLength, Used: (1 << 20) - 1})
	if mob.Uplink != contract.UplinkMobile || mob.QuotaMB != 1024 || mob.MaxS != 300 || mob.UsedMBMonth != 0 || mob.Active != 1 || mob.Cameras == nil {
		t.Fatalf("%+v", mob)
	}
	raw, _ = json.Marshal(mob)
	if err := schematest.Check("video-status.json", raw); err != nil {
		t.Fatal(err)
	}
}
