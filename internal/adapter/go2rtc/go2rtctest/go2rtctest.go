// Package go2rtctest plays go2rtc in tests, so that the video service can be tested
// without go2rtc, a camera or a relay. The test binary itself plays it: TestMain calls
// RunIfFake first, and the test points the adapter at os.Args[0] after Enable.
//
// The fake answers like go2rtc v1.9's API on its Unix socket:
//   - a camera whose address holds "deadcam" does not answer (500 "streams: dial...");
//   - one whose address holds "h265" sends H.265 (500 "streams: codecs not matched");
//   - otherwise the push starts, and bytes_send grows by 1000 every 10 ms, unless the
//     target holds "norelay" (nothing reaches the relay);
//   - it writes the target it was given in the file "pushed" next to its socket,
//     and prints it in its logs, as a careless program might;
//   - /api/frame.jpeg?src=ID&w=N returns a small JPEG of N*10 bytes, or, for a
//     "deadcam" camera, 200 and nothing (as go2rtc 1.9.14 does); "h265" in the address
//     answers 500.
//
// For tests only: shipped code never imports it.
package go2rtctest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const env = "SAFEHOME_FAKE_GO2RTC"

// Enable makes the processes the test starts play go2rtc.
func Enable(t *testing.T) { t.Setenv(env, "1") }

// RunIfFake plays go2rtc and exits, when the process was started to play it.
func RunIfFake() {
	if os.Getenv(env) != "1" || len(os.Args) != 3 || os.Args[1] != "-c" {
		return
	}
	if err := serve(os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "fake go2rtc:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type config struct {
	API struct {
		UnixListen string   `json:"unix_listen"`
		Listen     string   `json:"listen"`
		AllowPaths []string `json:"allow_paths"`
	} `json:"api"`
	Streams map[string]string `json:"streams"`
}

func serve(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	if cfg.API.UnixListen == "" || cfg.API.Listen != "" {
		return fmt.Errorf("the API must listen on a Unix socket only: %+v", cfg.API)
	}
	source := cfg.Streams["cam"]
	l, err := net.Listen("unix", cfg.API.UnixListen)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var pushedAt time.Time
	var target string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/streams", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && q.Get("src") == "":
			json.NewEncoder(w).Encode(map[string]any{"cam": map[string]any{"producers": []any{map[string]string{"url": source}}}})
		case r.Method == http.MethodGet:
			mu.Lock()
			defer mu.Unlock()
			consumers := []map[string]any{}
			if !pushedAt.IsZero() {
				c := map[string]any{"format_name": "rtmp"} // as go2rtc 1.9.14 says it
				if !strings.Contains(target, "norelay") {
					c["bytes_send"] = time.Since(pushedAt).Milliseconds() / 10 * 1000
				}
				consumers = append(consumers, c)
			}
			json.NewEncoder(w).Encode(map[string]any{"producers": []any{}, "consumers": consumers})
		case r.Method == http.MethodPost:
			if strings.Contains(source, "deadcam") {
				http.Error(w, "streams: dial tcp 10.0.0.9:554: connect: connection refused", http.StatusInternalServerError)
				return
			}
			if strings.Contains(source, "h265") {
				http.Error(w, "streams: codecs not matched: video:H265 => video:H264, audio:AAC", http.StatusInternalServerError)
				return
			}
			mu.Lock()
			pushedAt, target = time.Now(), q.Get("dst")
			mu.Unlock()
			os.WriteFile(filepath.Join(filepath.Dir(cfg.API.UnixListen), "pushed"), []byte(target), 0o600)
			fmt.Println("WRN [publish] url=" + target + " source=" + source)
		default:
			http.Error(w, "", http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/api/frame.jpeg", func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(cfg.API.AllowPaths, "/api/frame.jpeg") {
			http.NotFound(w, r)
			return
		}
		src, ok := cfg.Streams[r.URL.Query().Get("src")]
		width, _ := strconv.Atoi(r.URL.Query().Get("w"))
		switch {
		case !ok || strings.Contains(src, "deadcam"):
			return // 200, empty
		case strings.Contains(src, "h265"):
			http.Error(w, "exec: ffmpeg failed", http.StatusInternalServerError)
			return
		}
		jpeg := make([]byte, max(width, 1)*10)
		jpeg[0], jpeg[1], jpeg[len(jpeg)-2], jpeg[len(jpeg)-1] = 0xFF, 0xD8, 0xFF, 0xD9
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpeg)
	})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() { <-stop; l.Close() }()
	err = http.Serve(l, mux)
	if strings.Contains(fmt.Sprint(err), "closed") {
		return nil
	}
	return err
}
