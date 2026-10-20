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
	"testing"
	"time"
)

func TestPut(t *testing.T) {
	var got []byte
	var auth, ctype string
	status := http.StatusCreated
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "", http.StatusMethodNotAllowed)
			return
		}
		got, _ = io.ReadAll(r.Body)
		auth, ctype = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "https://elsewhere.example/", http.StatusTemporaryRedirect)
			return
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
	if err := c.Put(ctx, srv.URL+"/moved", "tok-secret", bytes.NewReader(photo), int64(len(photo))); err == nil {
		t.Fatal("a redirection is not followed, nor taken for a success")
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
