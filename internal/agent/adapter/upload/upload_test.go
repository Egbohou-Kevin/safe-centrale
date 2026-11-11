package upload

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPut(t *testing.T) {
	var got []byte
	var auth, ctype string
	var landed atomic.Int32 // requests that followed the redirection
	status := http.StatusCreated
	var elsewhere atomic.Int32 // requests that reached another host
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "", http.StatusMethodNotAllowed)
			return
		}
		got, _ = io.ReadAll(r.Body)
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		switch r.URL.Path {
		case "/moved":
			// To the same server: the token would follow it there (Go drops it only
			// for another host).
			http.Redirect(w, r, srv.URL+"/landing", http.StatusTemporaryRedirect)
			return
		case "/away":
			// To another host: the photo would leave the cloud.
			http.Redirect(w, r, other.URL+"/landing", http.StatusTemporaryRedirect)
			return
		case "/landing":
			landed.Add(1)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	c, err := New(ca)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	photo := []byte{0xFF, 0xD8, 1, 2, 3}
	if err := c.Put(ctx, srv.URL+"/v1/photos/p1", "tok-secret", bytes.NewReader(photo), int64(len(photo))); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, photo) || auth != "Bearer tok-secret" || ctype != "image/jpeg" {
		t.Fatalf("%q %q %q", got, auth, ctype)
	}
	for code, want := range map[int]error{403: ErrExpired, 404: ErrExpired, 413: ErrRefused, 503: nil} {
		status = code
		err := c.Put(ctx, srv.URL+"/p", "tok-secret", bytes.NewReader(photo), int64(len(photo)))
		switch {
		case err == nil:
			t.Errorf("%d: accepted", code)
		case want != nil && !errors.Is(err, want):
			t.Errorf("%d: %v", code, err)
		case want == nil && (errors.Is(err, ErrExpired) || errors.Is(err, ErrRefused)):
			t.Errorf("%d: worth another try: %v", code, err)
		case strings.Contains(err.Error(), "tok-secret"):
			t.Errorf("%d: the token in the error", code)
		}
	}
	status = http.StatusCreated
	if err := c.Put(ctx, srv.URL+"/moved", "tok-secret", bytes.NewReader(photo), int64(len(photo))); err == nil || landed.Load() != 0 {
		t.Fatalf("a redirection is not followed (%d followed), nor taken for a success: %v", landed.Load(), err)
	}
	if err := c.Put(ctx, srv.URL+"/away", "tok-secret", bytes.NewReader(photo), int64(len(photo))); err == nil || elsewhere.Load() != 0 {
		t.Fatalf("a redirection never takes the request to another host (%d went): %v", elsewhere.Load(), err)
	}
	if err := c.Put(ctx, strings.Replace(srv.URL, "https", "http", 1)+"/p", "t", bytes.NewReader(photo), 5); !errors.Is(err, ErrExpired) {
		t.Fatalf("http: %v", err)
	}
	// The system's authorities do not know the test server.
	sys, _ := New("")
	if err := sys.Put(ctx, srv.URL+"/p", "tok-secret", bytes.NewReader(photo), 5); err == nil || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("%v", err)
	}
	if _, err := New(filepath.Join(t.TempDir(), "none.pem")); err == nil {
		t.Fatal("missing CA")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, []byte("nothing"), 0o600)
	if _, err := New(empty); err == nil {
		t.Fatal("empty CA")
	}
}
