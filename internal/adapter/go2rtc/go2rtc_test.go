package go2rtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"safehome/internal/adapter/go2rtc/go2rtctest"
	"safehome/internal/driver/drivertest"
)

func TestMain(m *testing.M) {
	go2rtctest.RunIfFake()
	os.Exit(m.Run())
}

const (
	good   = "rtsp://admin:camPW%2199@192.168.1.10/stream2"
	target = "rtmps://relay.safehome.example/live/s1?token=tok%2Bsecret"
)

func fake(t *testing.T, sources map[string]string) (*Runner, *drivertest.SyncBuffer) {
	t.Helper()
	go2rtctest.Enable(t)
	logs := &drivertest.SyncBuffer{}
	r, err := New(Options{Binary: os.Args[0], Sources: sources, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return r, logs
}

// privateTemp gives the test a temporary folder of its own (short: it holds Unix
// sockets), so that what is left in it is the test's.
func privateTemp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "g2")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	return dir
}

func TestAStreamPushesCountsAndStops(t *testing.T) {
	r, logs := fake(t, map[string]string{"cam_caisse": good})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := r.Start(ctx, "cam_caisse", target)
	if err != nil {
		t.Fatal(err)
	}

	// Its folder and configuration are private, and the API is on the socket only.
	if fi, err := os.Stat(s.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi.Mode(), err)
	}
	fi, err := os.Stat(filepath.Join(s.dir, "go2rtc.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	var cfg map[string]map[string]any
	raw, _ := os.ReadFile(filepath.Join(s.dir, "go2rtc.json"))
	json.Unmarshal(raw, &cfg)
	if cfg["api"]["listen"] != "" || cfg["rtsp"]["listen"] != "" || cfg["streams"]["cam"] != good {
		t.Fatalf("%s", raw)
	}
	if mods := cfg["app"]["modules"].([]any); len(mods) != 3 || strings.Contains(string(raw), "webrtc") {
		t.Fatalf("only api, rtsp and rtmp: %s", raw)
	}
	if pushed, _ := os.ReadFile(filepath.Join(s.dir, "pushed")); string(pushed) != target {
		t.Fatalf("pushed to %q", pushed)
	}

	var n int64
	for deadline := time.Now().Add(5 * time.Second); n < 5000; {
		if time.Now().After(deadline) {
			t.Fatalf("the bytes do not grow: %d", n)
		}
		if n, err = s.Sent(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	dir := s.dir
	s.Stop()
	s.Stop() // twice is fine
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the folder must be removed")
	}
	if got, err := s.Sent(ctx); !errors.Is(err, ErrExited) || got < n {
		t.Fatalf("%d %v", got, err)
	}
	// The fake printed the target and the address: the logs show neither.
	l := logs.String()
	if !strings.Contains(l, "url=***") || strings.Contains(l, "secret") || strings.Contains(l, "camPW") {
		t.Fatalf("logs:\n%s", l)
	}
}

func TestTheCameraDoesNotAnswerOrSendsH265(t *testing.T) {
	tmp := privateTemp(t)
	r, _ := fake(t, map[string]string{"dead": "rtsp://u:p@deadcam.local/x", "hevc": "rtsp://h265.local/x"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.Start(ctx, "dead", target); !errors.Is(err, ErrCameraUnreachable) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("%v", err)
	}
	// A one-letter token: masking it must not hide what go2rtc says.
	if _, err := r.Start(ctx, "hevc", "rtmps://relay.example/live/s1?token=t"); !errors.Is(err, ErrCameraUnreachable) || !strings.Contains(err.Error(), "H.264") {
		t.Fatalf("%v", err)
	}
	if _, err := r.Start(ctx, "nope", target); err == nil {
		t.Fatal("an unknown camera started")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Fatalf("a failed start left its folder: %v", left)
	}
}

func TestNothingReachesTheRelay(t *testing.T) {
	r, _ := fake(t, map[string]string{"cam": good})
	ctx := context.Background()
	s, err := r.Start(ctx, "cam", "rtmps://norelay.example/live/s1?token=t")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	time.Sleep(50 * time.Millisecond)
	if n, err := s.Sent(ctx); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
}

func TestAProcessThatDiesIsSeen(t *testing.T) {
	r, _ := fake(t, map[string]string{"cam": good})
	ctx := context.Background()
	s, err := r.Start(ctx, "cam", target)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.cmd.Process.Kill()
	<-s.exited
	if _, err := s.Sent(ctx); !errors.Is(err, ErrExited) {
		t.Fatalf("%v", err)
	}
}

// TestWhyGo2rtcStoppedIsLogged: the last lines go2rtc prints before it exits are
// the reason, and they reach the logs, masked, before Start returns.
func TestWhyGo2rtcStoppedIsLogged(t *testing.T) {
	for range 20 {
		r, logs := fake(t, map[string]string{"cam": "rtsp://admin:camPW@crash.local/s"})
		if _, err := r.Start(context.Background(), "cam", target); !errors.Is(err, ErrExited) {
			t.Fatalf("%v", err)
		}
		if l := logs.String(); !strings.Contains(l, "cannot start source=***") || strings.Contains(l, "camPW") {
			t.Fatalf("logs:\n%s", l)
		}
	}
}

func TestAMissingBinaryFails(t *testing.T) {
	r, err := New(Options{Binary: filepath.Join(t.TempDir(), "go2rtc"), Sources: map[string]string{"cam": good}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Start(context.Background(), "cam", target); err == nil || errors.Is(err, ErrCameraUnreachable) {
		t.Fatalf("%v", err)
	}
}

func TestTheCounterSurvivesReconnections(t *testing.T) {
	s := &Stream{}
	for _, c := range []struct{ now, want int64 }{{100, 100}, {250, 250}, {30, 280}, {80, 330}, {0, 330}, {5, 335}} {
		if got := s.count(c.now); got != c.want {
			t.Fatalf("count(%d) = %d, want %d", c.now, got, c.want)
		}
	}
}

func TestProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()
	r, err := New(Options{Binary: "go2rtc", Sources: map[string]string{
		"up": "rtsp://u:p@" + l.Addr().String() + "/x", "down": "rtsp://" + closedAddr + "/x",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.Probe(ctx, "up"); err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(ctx, "down"); !errors.Is(err, ErrCameraUnreachable) {
		t.Fatalf("%v", err)
	}
	if err := r.Probe(ctx, "nope"); err == nil {
		t.Fatal("unknown camera")
	}
}

func TestOptions(t *testing.T) {
	for _, src := range []string{"http://cam/x", "rtsp://", "://"} {
		if _, err := New(Options{Binary: "go2rtc", Sources: map[string]string{"c": src}}); err == nil {
			t.Errorf("%q accepted", src)
		}
	}
	if _, err := New(Options{Sources: map[string]string{"c": good}}); err == nil {
		t.Error("no binary accepted")
	}
	for src, want := range map[string]string{"rtsp://h/x": "h:554", "rtsps://h/x": "h:322", "rtsp://h:8554/x": "h:8554"} {
		if got, _ := hostPort(src); got != want {
			t.Errorf("%s: %s, want %s", src, got, want)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitListening(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	drivertest.WaitFor(t, "a listener on "+addr, func() bool {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

// background starts a helper process for the test and stops it at the end.
func background(t *testing.T, out io.Writer, name string, args ...string) chan struct{} {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = t.TempDir()
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	})
	return exited
}

// camera simulates an RTSP camera: a go2rtc fed a test picture by ffmpeg, encoded
// with the given encoder (libx264, libx265). It returns its RTSP port.
func camera(t *testing.T, bin, encoder string) int {
	t.Helper()
	port := freePort(t)
	cfg := filepath.Join(t.TempDir(), "camera.yaml")
	os.WriteFile(cfg, []byte(fmt.Sprintf(`api: {listen: ""}
rtsp: {listen: "127.0.0.1:%d"}
webrtc: {listen: ""}
log: {level: warn, format: text}
streams:
  cam: exec:ffmpeg -hide_banner -loglevel error -re -f lavfi -i testsrc=size=640x360:rate=15 -c:v %s -preset ultrafast -tune zerolatency -g 30 -pix_fmt yuv420p -f rtsp {output}
`, port, encoder)), 0o600)
	background(t, io.Discard, bin, "-c", cfg)
	waitListening(t, port)
	return port
}

// needVideoTools skips the test without go2rtc and ffmpeg, and returns go2rtc.
func needVideoTools(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("go2rtc")
	if err != nil {
		t.Skip("go2rtc is not installed")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed: no camera to simulate")
	}
	return bin
}

// TestARealH265CameraIsRefused: go2rtc pushes H.264 only over RTMP.
func TestARealH265CameraIsRefused(t *testing.T) {
	bin := needVideoTools(t)
	port := camera(t, bin, "libx265")
	r, err := New(Options{Binary: bin, Sources: map[string]string{"cam": fmt.Sprintf("rtsp://127.0.0.1:%d/cam", port)}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.Start(ctx, "cam", fmt.Sprintf("rtmp://127.0.0.1:%d/live/s1?token=t", freePort(t)))
	if err == nil {
		s.Stop()
	}
	if !errors.Is(err, ErrCameraUnreachable) || !strings.Contains(err.Error(), "H.264") {
		t.Fatalf("%v", err)
	}
}

// TestARealStreamFromACameraToARelay runs the real go2rtc between a simulated camera
// (another go2rtc, fed a test picture in H.264 by ffmpeg) and a simulated relay (ffmpeg
// waiting for an RTMP stream): the bytes are counted, and stopping the stream ends
// the push. It needs go2rtc and ffmpeg.
func TestARealStreamFromACameraToARelay(t *testing.T) {
	bin := needVideoTools(t)
	camPort, relayPort := camera(t, bin, "libx264"), freePort(t)
	relayLog := drivertest.Logs(t) // the relay's, shown if the test fails
	relayDone := background(t, relayLog, "ffmpeg", "-nostdin", "-hide_banner", "-listen", "1",
		"-i", fmt.Sprintf("rtmp://127.0.0.1:%d/live/s1", relayPort), "-c", "copy", "-f", "null", "-")
	// Not the relay: ffmpeg takes the first connection for the stream. go2rtc tries
	// again every 5 s if it is not listening yet.

	logs := &drivertest.SyncBuffer{}
	r, err := New(Options{Binary: bin, Sources: map[string]string{"cam": fmt.Sprintf("rtsp://viewer:camPW@127.0.0.1:%d/cam", camPort)},
		Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.Start(ctx, "cam", fmt.Sprintf("rtmp://127.0.0.1:%d/live/s1?token=tok-secret", relayPort))
	if err != nil {
		t.Fatalf("%v\nlogs:\n%s", err, logs.String())
	}
	defer s.Stop()

	var n int64
	for deadline := time.Now().Add(15 * time.Second); n < 50_000; time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d bytes reached the relay\nlogs:\n%s\nrelay:\n%s", n, logs.String(), relayLog.String())
		}
		if n, err = s.Sent(ctx); err != nil {
			t.Fatal(err)
		}
	}
	drivertest.WaitFor(t, "H.264 at the relay", func() bool { return strings.Contains(relayLog.String(), "Video: h264") })
	if n2, _ := s.Sent(ctx); n2 <= n {
		t.Fatalf("the count stopped at %d", n)
	}

	s.Stop()
	select {
	case <-relayDone: // the push ended, so did the relay's input
	case <-time.After(10 * time.Second):
		t.Fatalf("the relay still receives after the stop:\n%s", relayLog.String())
	}
	if l := logs.String(); strings.Contains(l, "tok-secret") || strings.Contains(l, "camPW") {
		t.Fatalf("logs:\n%s", l)
	}
	t.Logf("%d bytes counted", n)
}

// TestTheRealGo2rtcStartsAndAnswers runs the real go2rtc when it is installed: it
// checks that it accepts our configuration, answers on its socket, and says when the
// camera does not answer. That it opens no network port is checked while it runs, in
// TestRealPhotosFromACamera.
func TestTheRealGo2rtcStartsAndAnswers(t *testing.T) {
	bin, err := exec.LookPath("go2rtc")
	if err != nil {
		t.Skip("go2rtc is not installed: skipping the test against the real program")
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := closed.Addr().String()
	closed.Close()
	logs := &drivertest.SyncBuffer{}
	r, err := New(Options{Binary: bin, Sources: map[string]string{"cam": "rtsp://u:pw@" + addr + "/x"}, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err = r.Start(ctx, "cam", target)
	if !errors.Is(err, ErrCameraUnreachable) {
		t.Fatalf("want the camera unreachable, got %v\nlogs:\n%s", err, logs.String())
	}
}

// TestRealPhotosFromACamera takes pictures with the real go2rtc and ffmpeg from a
// simulated camera, and from a camera that does not answer.
func TestRealPhotosFromACamera(t *testing.T) {
	bin := needVideoTools(t)
	privateTemp(t)
	port := camera(t, bin, "libx264")
	logs := &drivertest.SyncBuffer{}
	r, err := New(Options{Binary: bin, Logger: slog.New(slog.NewTextHandler(logs, nil)), Sources: map[string]string{
		"cam_caisse": fmt.Sprintf("rtsp://viewer:camPW@127.0.0.1:%d/cam", port),
		"cam_cour":   fmt.Sprintf("rtsp://viewer:camPW@127.0.0.1:%d/cam", freePort(t)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := r.OpenStills(ctx)
	if err != nil {
		t.Fatalf("%v\nlogs:\n%s", err, logs.String())
	}
	defer s.Close()
	for _, width := range []int{640, 320} {
		start := time.Now()
		jpeg, err := s.Frame(ctx, "cam_caisse", width)
		if err != nil {
			t.Fatalf("%v\nlogs:\n%s", err, logs.String())
		}
		if jpeg[0] != 0xFF || jpeg[1] != 0xD8 || len(jpeg) < 1000 {
			t.Fatalf("%d bytes, not a JPEG", len(jpeg))
		}
		t.Logf("%d px: %d bytes in %v", width, len(jpeg), time.Since(start))
	}
	// It answers on its socket only: no network port, whatever its version makes of
	// our configuration (docs/video.md, "aucun port ouvert").
	if l := listening(t, s.cmd.Process.Pid); l != "" {
		t.Fatalf("go2rtc opened network ports:\n%s", l)
	}
	start := time.Now()
	if _, err := s.Frame(ctx, "cam_cour", 320); !errors.Is(err, ErrCameraUnreachable) {
		t.Fatalf("a dead camera: %v", err)
	} else {
		t.Logf("dead camera in %v: %v", time.Since(start), err)
	}
	if _, err := s.Frame(ctx, "cam_none", 320); !errors.Is(err, ErrCameraUnreachable) {
		t.Fatalf("an unknown camera: %v", err)
	}
	dir := s.dir
	s.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the folder must be removed")
	}
	if strings.Contains(logs.String(), "camPW") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestStillsTakePicturesOfEveryCamera(t *testing.T) {
	privateTemp(t)
	r, logs := fake(t, map[string]string{"cam_caisse": good, "cam_cour": "rtsp://admin:camPW%2199@deadcam.local/s2", "cam_h": "rtsp://h265.local/s"})
	r.o.FFmpeg = "/usr/bin/ffmpeg"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := r.OpenStills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]map[string]any
	raw, _ := os.ReadFile(filepath.Join(s.dir, "go2rtc.json"))
	json.Unmarshal(raw, &cfg)
	if cfg["streams"]["cam_caisse"] != good || cfg["ffmpeg"]["bin"] != "/usr/bin/ffmpeg" || cfg["api"]["listen"] != "" || strings.Contains(string(raw), "webrtc") {
		t.Fatalf("%s", raw)
	}
	jpeg, err := s.Frame(ctx, "cam_caisse", 320)
	if err != nil || len(jpeg) != 3200 {
		t.Fatalf("%d %v", len(jpeg), err)
	}
	for _, cam := range []string{"cam_cour", "cam_h"} {
		_, err := s.Frame(ctx, cam, 320)
		if !errors.Is(err, ErrCameraUnreachable) || strings.Contains(err.Error(), "camPW") {
			t.Fatalf("%s: %v", cam, err)
		}
	}
	dir := s.dir
	s.Close()
	s.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("the folder must be removed")
	}
	if _, err := s.Frame(ctx, "cam_caisse", 320); err == nil {
		t.Fatal("closed")
	}
	if strings.Contains(logs.String(), "camPW") {
		t.Fatalf("logs:\n%s", logs.String())
	}
	// Without ffmpeg set, go2rtc finds it in the PATH.
	r.o.FFmpeg = ""
	s2, err := r.OpenStills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	raw, _ = os.ReadFile(filepath.Join(s2.dir, "go2rtc.json"))
	if strings.Contains(string(raw), `"ffmpeg": {`) {
		t.Fatalf("%s", raw)
	}
}

// listening lists the network ports a process listens on (TCP) or holds (UDP), with
// lsof; nothing is checked without it.
func listening(t *testing.T, pid int) string {
	t.Helper()
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		t.Log("lsof is not installed: the open ports are not checked")
		return ""
	}
	var out strings.Builder
	for _, sel := range [][]string{{"-iTCP", "-sTCP:LISTEN"}, {"-iUDP"}} {
		b, _ := exec.Command(lsof, append([]string{"-a", "-p", strconv.Itoa(pid), "-P", "-n"}, sel...)...).Output() // exit 1: none
		out.Write(b)
	}
	return out.String()
}
