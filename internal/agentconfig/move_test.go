package agentconfig

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var (
	cellA = []string{"mqtts://cloud.example:8883"}
	cellB = []string{"mqtts://b.example:8883", "wss://b.example:443/mqtt"}
	cellC = []string{"mqtts://c.example:8883"}
)

const secretB, secretC = "s3cret-for-cell-b-0001", "s3cret-for-cell-c-0001"

// moving writes a configuration whose state is in a temporary folder; it returns
// its path and the agent's own folder.
func moving(t *testing.T) (string, string) {
	t.Helper()
	state := t.TempDir()
	path := write(t, `{"site": "tg-lom-0001", "state_dir": "`+state+`", "cloud": {`+goodCloud+`}}`)
	return path, filepath.Join(state, KeyFolder)
}

func load(t *testing.T, path string) Result {
	t.Helper()
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func onCell(t *testing.T, r Result, urls []string, secret string) {
	t.Helper()
	if !slices.Equal(r.Cloud.URLs, urls) || r.Cloud.Password != secret || !slices.Equal(r.App.Relay.CloudURLs, urls) {
		t.Fatalf("on %v (%v for the photos), want %v", r.Cloud.URLs, r.App.Relay.CloudURLs, urls)
	}
}

// files lists what the agent's folder holds: no temporary file is left.
func files(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v", e.Name(), info.Mode().Perm())
		}
	}
	return out
}

func TestAMoveIsWrittenBesideTheCurrentCloudThenTakesItsPlace(t *testing.T) {
	path, dir := moving(t)
	if err := WriteNext(dir, "move-b", cellB, secretB); err != nil {
		t.Fatal(err)
	}
	r := load(t, path)
	onCell(t, r, cellA, "pw") // untouched until B accepts
	if r.Next == nil || !slices.Equal(r.Next.URLs, cellB) || r.Next.Password != secretB || r.Next.ClientID != r.Cloud.ClientID || r.Next.Username != r.Cloud.Username {
		t.Fatalf("the move under way: %+v", r.Next)
	}
	if r.NextRef != "move-b" || r.MovedRef != "" {
		t.Fatalf("the moves: next %q, current %q", r.NextRef, r.MovedRef)
	}
	if err := Commit(dir); err != nil {
		t.Fatal(err)
	}
	r = load(t, path)
	onCell(t, r, cellB, secretB)
	if r.Next != nil || !slices.Equal(files(t, dir), []string{MovedName}) {
		t.Fatalf("after the commit: %+v, %v", r.Next, files(t, dir))
	}
	if r.NextRef != "" || r.MovedRef != "move-b" {
		t.Fatalf("the moves: next %q, current %q", r.NextRef, r.MovedRef)
	}
	// A second move keeps the first cell once.
	if err := WriteNext(dir, "move-c", cellC, secretC); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir); err != nil {
		t.Fatal(err)
	}
	onCell(t, load(t, path), cellC, secretC)
	prev, _, err := readMoved(dir, PreviousName, Cloud{})
	if err != nil || prev == nil || !slices.Equal(prev.URLs, cellB) || prev.Password != secretB {
		t.Fatalf("the previous cloud: %+v, %v", prev, err)
	}
}

func TestAMoveThatGoesBackLeavesTheCurrentCloud(t *testing.T) {
	path, dir := moving(t)
	if err := WriteNext(dir, "move-b", cellB, secretB); err != nil {
		t.Fatal(err)
	}
	if err := Discard(dir); err != nil {
		t.Fatal(err)
	}
	r := load(t, path)
	onCell(t, r, cellA, "pw")
	if r.Next != nil || len(files(t, dir)) != 0 {
		t.Fatalf("%+v, %v", r.Next, files(t, dir))
	}
	if err := Discard(dir); err != nil {
		t.Fatalf("discarding nothing: %v", err)
	}
	if err := Commit(dir); err == nil {
		t.Fatal("committed a move that is not there")
	}
}

// A stop between two writes (the rename never done) leaves the old configuration
// as it was, and no half-written file is read.
func TestAStopBetweenTwoWritesLeavesTheOldConfiguration(t *testing.T) {
	path, dir := moving(t)
	if err := WriteNext(dir, "move-b", cellB, secretB); err != nil {
		t.Fatal(err)
	}
	if err := Commit(dir); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("power cut")
	rename = func(string, string) error { return stop }
	t.Cleanup(func() { rename = os.Rename })

	// Writing the next move stops before its rename.
	if err := WriteNext(dir, "move-c", cellC, secretC); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	r := load(t, path)
	onCell(t, r, cellB, secretB)
	if r.Next != nil || !slices.Equal(files(t, dir), []string{MovedName}) {
		t.Fatalf("%+v, %v", r.Next, files(t, dir))
	}
	// A temporary file a power cut left is never read.
	os.WriteFile(filepath.Join(dir, NextName+"-123"), []byte(`{"urls":["mqtts://c.exam`), 0o600)
	if r := load(t, path); r.Next != nil {
		t.Fatal("a temporary file was read")
	}
	os.Remove(filepath.Join(dir, NextName+"-123"))

	// The commit stops after keeping the previous cloud, before the new one takes the
	// current one's place: the current one is untouched and the move still pending.
	rename = os.Rename
	if err := WriteNext(dir, "move-c", cellC, secretC); err != nil {
		t.Fatal(err)
	}
	calls := 0
	rename = func(from, to string) error {
		if calls++; calls > 1 {
			return stop
		}
		return os.Rename(from, to)
	}
	if err := Commit(dir); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	r = load(t, path)
	onCell(t, r, cellB, secretB)
	if r.Next == nil || !slices.Equal(r.Next.URLs, cellC) {
		t.Fatalf("the move is lost: %+v", r.Next)
	}
}

func TestADamagedMovedCloudIsRefusedWithoutItsSecret(t *testing.T) {
	path, dir := moving(t)
	os.MkdirAll(dir, 0o700)
	for name, body := range map[string]string{
		MovedName: `{"urls":["mqtt://b.example:1883"],"secret":"` + secretB + `"}`,
		NextName:  `{"urls":["mqtts://b.exa`,
	} {
		os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
		_, err := Load(path)
		if err == nil || strings.Contains(err.Error(), secretB) {
			t.Errorf("%s: %v", name, err)
		}
		os.Remove(filepath.Join(dir, name))
	}
	if err := WriteNext(dir, "move-b", []string{"mqtt://b.example:1883"}, secretB); err == nil {
		t.Error("a move to an unencrypted cloud was written")
	}
	if err := WriteNext(dir, "move-b", cellB, ""); err == nil {
		t.Error("a move without a secret was written")
	}
	if err := WriteNext(dir, "", cellB, secretB); err == nil {
		t.Error("a move without its ref was written")
	}
}
