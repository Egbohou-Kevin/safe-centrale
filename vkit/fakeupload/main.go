// Command fakeupload is the cloud's photo store of the virtual kit
// (docs/kit-virtuel.md): it takes the photos the link agent sends, one HTTPS PUT
// each (docs/contract.md, photo.upload), and prints what it got. A token that does
// not start with "vkit-ok" is refused (401), as an expired address would be.
//
//	fakeupload -listen 127.0.0.1:8443 -cert cloud.pem -key cloud.key
package main

import (
	"bytes"
	"flag"
	"io"
	"log"
	"net/http"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8443", "address to listen on")
	cert := flag.String("cert", "/etc/vkit/cloud.pem", "TLS certificate")
	key := flag.String("key", "/etc/vkit/cloud.key", "TLS key")
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	http.HandleFunc("/photos/", receive)
	log.Fatal(http.ListenAndServeTLS(*listen, *cert, *key, nil))
}

// receive takes one photo. The token itself is never printed.
func receive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "PUT only", http.StatusMethodNotAllowed)
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(token, "vkit-ok") {
		log.Printf("REFUSED %s: bad token", r.URL.Path)
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jpeg := r.Header.Get("Content-Type") == "image/jpeg" && bytes.HasPrefix(body, []byte{0xFF, 0xD8})
	log.Printf("PUT %s bytes=%d jpeg=%t", r.URL.Path, len(body), jpeg)
	w.WriteHeader(http.StatusCreated)
}
