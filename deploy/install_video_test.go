package deploy

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A hub updated from before 28 October 2026: the video's state moves to video/ with the
// service stopped, the top copies stay for a rollback, the configuration from the cloud
// is in site/, the photos waiting stay where the agent takes them.
func TestTheVideoKeepsItsStateInItsOwnFolder(t *testing.T) {
	r := newRig(t)
	r.minimalSite()
	r.site("video.json", "{}")
	writeScript(t, filepath.Join(r.bundle, "bin", "go2rtc"), "exit 0")
	state := "var/lib/safehome/"
	before := map[string]string{
		"video.json": "usage", "video.json.prev": "usage before", "video_seq.json": "outbox",
		"site-config.json": `{"version":3}`, "code-key": "secret", "photos/p1.jpg": "photo",
	}
	for f, c := range before {
		writeFile(t, r.path(state+f), c)
	}
	cmds := r.mustRun()
	stop, restart := slices.Index(cmds, "systemctl stop safehome-video"), slices.Index(cmds, "systemctl restart safehome-video")
	if stop < 0 || restart < stop {
		t.Errorf("the video service was not stopped before its state was copied: %q", cmds)
	}
	read := func(f string) string { b, _ := os.ReadFile(r.path(state + f)); return string(b) }
	for f, c := range before {
		if got := read(f); got != c {
			t.Errorf("%s = %q, want %q kept", f, got, c)
		}
	}
	for _, f := range []string{"video.json", "video.json.prev", "video_seq.json"} {
		if got := read("video/" + f); got != before[f] {
			t.Errorf("video/%s = %q, want %q", f, got, before[f])
		}
	}
	if r.exists(state+"video/code-key") || r.exists(state+"video/video_seq.json.prev") {
		t.Error("video/ holds what the video did not keep")
	}
	if got := read("site/site-config.json"); got != before["site-config.json"] {
		t.Errorf("site/site-config.json = %q", got)
	}
	for _, d := range []string{"video", "photos", "site"} {
		if fi, err := os.Stat(r.path(state + d)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s/: %v %v", d, fi, err)
		}
	}

	// Again: video/ is the service's now, never copied over nor the service stopped
	// for it; a top copy left alone by a rolled back hub reaches site/.
	writeFile(t, r.path(state+"video/video.json"), "usage since")
	writeFile(t, r.path(state+"site-config.json"), `{"version":4}`)
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(r.path(state+"site-config.json"), future, future); err != nil {
		t.Fatal(err)
	}
	cmds = r.mustRun()
	if slices.Contains(cmds, "systemctl stop safehome-video") {
		t.Error("stopped again")
	}
	if got := read("video/video.json"); got != "usage since" {
		t.Errorf("video/video.json = %q: copied over", got)
	}
	if got := read("site/site-config.json"); got != `{"version":4}` {
		t.Errorf("site/site-config.json = %q, want the rolled back hub's", got)
	}

	// A rollback ran the earlier video on the top copies: updated again, video/ takes
	// what it wrote since, the service stopped first; nothing built aside stays.
	writeFile(t, r.path(state+"video.json"), "usage after the rollback")
	writeFile(t, r.path(state+"video_seq.json"), "outbox after the rollback")
	later := future.Add(time.Minute)
	for _, f := range []string{"video.json", "video_seq.json"} {
		if err := os.Chtimes(r.path(state+f), later, later); err != nil {
			t.Fatal(err)
		}
	}
	cmds = r.mustRun()
	if !slices.Contains(cmds, "systemctl stop safehome-video") {
		t.Errorf("the video service was not stopped before its state was copied again: %q", cmds)
	}
	for f, want := range map[string]string{"video.json": "usage after the rollback", "video_seq.json": "outbox after the rollback"} {
		if got := read("video/" + f); got != want {
			t.Errorf("after a rollback, video/%s = %q, want %q", f, got, want)
		}
	}
	if r.exists(state+"video.new") || r.exists(state+"video.old") {
		t.Error("a folder built aside stayed")
	}
	if cmds = r.mustRun(); slices.Contains(cmds, "systemctl stop safehome-video") {
		t.Error("stopped again with nothing new at the top")
	}
}

// A copy into video/ cut short (here a file it cannot read) leaves no video/ half
// filled: the next run makes it whole, rather than starting the video on half a state.
func TestAVideoStateCopyCutShortIsMadeAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without rights")
	}
	r := newRig(t)
	r.minimalSite()
	r.site("video.json", "{}")
	writeScript(t, filepath.Join(r.bundle, "bin", "go2rtc"), "exit 0")
	state := "var/lib/safehome/"
	writeFile(t, r.path(state+"video.json"), "usage")
	writeFile(t, r.path(state+"video_seq.json"), "outbox")
	seq := r.path(state + "video_seq.json")
	if err := os.Chmod(seq, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.run(); err == nil {
		t.Fatal("install.sh went on past a copy that failed")
	}
	if r.exists(state + "video") {
		t.Error("a copy cut short left video/")
	}
	if err := os.Chmod(seq, 0o644); err != nil {
		t.Fatal(err)
	}
	r.mustRun()
	for f, want := range map[string]string{"video.json": "usage", "video_seq.json": "outbox"} {
		if b, err := os.ReadFile(r.path(state + "video/" + f)); err != nil || string(b) != want {
			t.Errorf("video/%s = %q %v, want %q", f, b, err, want)
		}
	}
}
