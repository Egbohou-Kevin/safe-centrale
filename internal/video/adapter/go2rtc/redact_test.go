package go2rtc

import (
	"net/url"
	"strings"
	"testing"
)

// An HTTP error quotes the request's URL, where the target is escaped once more (it
// is a query value of the request to go2rtc): the token is escaped twice there. No
// form of it reaches the journal.
func TestTheTokenEscapedTwiceIsMasked(t *testing.T) {
	const tok = "q8+Zr/3kL1x=="
	target := "rtmps://relay.example/live/s1?token=" + url.QueryEscape(tok)
	p := &process{secrets: secrets([]string{good}, target)}
	q := url.Values{"src": {"cam"}, "dst": {target}}
	text := `go2rtc: Post "http://go2rtc/api/streams?` + q.Encode() + `": context deadline exceeded`
	masked := p.redact(text)
	for range 3 {
		if strings.Contains(masked, tok) || strings.Contains(masked, "q8") {
			t.Fatalf("the token shows: %s", p.redact(text))
		}
		masked, _ = url.QueryUnescape(masked)
	}
}
