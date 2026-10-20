package contract

import (
	"crypto/rand"
	"io"
	"math/big"
	"regexp"
	"sync"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var idPattern = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// ValidID reports whether s looks like a ULID: 26 Crockford base-32 characters.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// IDGenerator produces ULIDs: unique, sortable by creation time, and strictly
// increasing even within the same millisecond. It is safe for concurrent use.
type IDGenerator struct {
	mu      sync.Mutex
	entropy io.Reader
	now     func() time.Time
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
	if ms == g.lastMs && ms != 0 {
		incrementBytes(&g.last)
	} else {
		if _, err := io.ReadFull(g.entropy, g.last[:]); err != nil {
			panic("contract: entropy source unavailable: " + err.Error())
		}
		g.lastMs = ms
	}

	var raw [16]byte
	raw[0] = byte(ms >> 40)
	raw[1] = byte(ms >> 32)
	raw[2] = byte(ms >> 24)
	raw[3] = byte(ms >> 16)
	raw[4] = byte(ms >> 8)
	raw[5] = byte(ms)
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
// leading bits are zero).
func encodeID(raw [16]byte) string {
	n := new(big.Int).SetBytes(raw[:])
	mask := big.NewInt(31)
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = crockford[new(big.Int).And(n, mask).Int64()]
		n.Rsh(n, 5)
	}
	return string(out)
}
