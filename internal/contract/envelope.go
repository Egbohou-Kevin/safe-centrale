package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	// ErrInvalid is returned, wrapped, for any message that breaks the contract.
	ErrInvalid = errors.New("invalid message")
	// ErrUnsupportedVersion is returned for a message from a newer contract version
	// than this package knows: the receiver ignores it and logs it.
	ErrUnsupportedVersion = errors.New("unsupported contract version")
)

var sitePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

// ValidSite reports whether s is a valid site identifier (lowercase letters, digits
// and dashes, 3 to 40 characters, such as "tg-lom-0001").
func ValidSite(s string) bool { return sitePattern.MatchString(s) }

// Envelope is the envelope shared by every message.
type Envelope struct {
	V    int             `json:"v"`
	ID   string          `json:"id"`
	Site string          `json:"site"`
	Src  string          `json:"src"`
	Seq  uint64          `json:"seq"`
	TS   Timestamp       `json:"ts"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`

	// Reserved for the closed-hardware phase (message signing).
	Sig string `json:"sig,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// NewEnvelope builds a valid envelope around data.
func NewEnvelope(site, src string, seq uint64, id string, ts time.Time, typ string, data any) (Envelope, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return Envelope{}, fmt.Errorf("%w: data: %v", ErrInvalid, err)
	}
	e := Envelope{V: Version, ID: id, Site: site, Src: src, Seq: seq, TS: TS(ts), Type: typ, Data: raw}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// ParseEnvelope reads and validates a message.
func ParseEnvelope(b []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}

// Validate checks the envelope.
func (e Envelope) Validate() error {
	switch {
	case e.V > Version:
		return fmt.Errorf("%w: v=%d, this package knows v=%d", ErrUnsupportedVersion, e.V, Version)
	case e.V < 1:
		return fmt.Errorf("%w: v=%d", ErrInvalid, e.V)
	case !ValidID(e.ID):
		return fmt.Errorf("%w: id %q", ErrInvalid, e.ID)
	case !sitePattern.MatchString(e.Site):
		return fmt.Errorf("%w: site %q", ErrInvalid, e.Site)
	case e.Src == "":
		return fmt.Errorf("%w: empty src", ErrInvalid)
	case e.Type == "":
		return fmt.Errorf("%w: empty type", ErrInvalid)
	case e.TS.IsZero():
		return fmt.Errorf("%w: missing ts", ErrInvalid)
	}
	data := bytes.TrimSpace(e.Data)
	if len(data) == 0 || data[0] != '{' {
		return fmt.Errorf("%w: data must be an object", ErrInvalid)
	}
	return nil
}

// DecodeData reads the envelope's payload. Unknown fields are ignored, which is
// what allows adding a field without changing the contract version.
func DecodeData[T any](e Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		return v, fmt.Errorf("%w: data of %s: %v", ErrInvalid, e.Type, err)
	}
	return v, nil
}
