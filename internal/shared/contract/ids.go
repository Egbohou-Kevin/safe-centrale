package contract

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"regexp"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var idPattern = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// ValidID reports whether s looks like a ULID: 26 Crockford base-32 characters.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// IDGenerator produces ULIDs: unique, sortable by creation time, and strictly
// increasing even within the same millisecond, or when the clock steps back (NTP
// correcting a Raspberry Pi after a power cut). It is safe for concurrent use.
type IDGenerator struct {
	mu      sync.Mutex
	entropy io.Reader
	now     func() time.Time
	started bool
	lastMs  uint64
	last    [10]byte
}

// NewIDGenerator creates a generator backed by the system clock and crypto/rand.
func NewIDGenerator() *IDGenerator {
	return &IDGenerator{entropy: rand.Reader, now: time.Now}
}

// NewIDGeneratorWith creates a generator with a chosen clock and entropy source,
// for tests.
func NewIDGeneratorWith(now func() time.Time, entropy io.Reader) *IDGenerator {
	return &IDGenerator{entropy: entropy, now: now}
}

// New returns a new identifier.
func (g *IDGenerator) New() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(g.now().UnixMilli())
	if g.started && ms <= g.lastMs {
		// Same millisecond, or the clock went back: keep the last time and count on
		// from the last random part, so the order still holds.
		incrementBytes(&g.last)
	} else {
		if _, err := io.ReadFull(g.entropy, g.last[:]); err != nil {
			panic("contract: entropy source unavailable: " + err.Error())
		}
		g.lastMs, g.started = ms, true
	}

	var raw [16]byte
	binary.BigEndian.PutUint64(raw[0:8], g.lastMs<<16) // 48 bits of time first
	copy(raw[6:], g.last[:])
	return encodeID(raw)
}

func incrementBytes(b *[10]byte) {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return
		}
	}
}

// encodeID writes 128 bits as 26 Crockford characters (130 bits, so the two
// leading bits are zero), five bits at a time from the right.
func encodeID(raw [16]byte) string {
	hi := binary.BigEndian.Uint64(raw[0:8])
	lo := binary.BigEndian.Uint64(raw[8:16])
	var out [26]byte
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
