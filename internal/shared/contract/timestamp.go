package contract

import (
	"encoding/json"
	"fmt"
	"time"
)

// timestampLayout is the contract's date format: UTC, millisecond precision.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// Timestamp is a date that is always written in UTC with millisecond precision in
// the contract's JSON.
type Timestamp struct{ time.Time }

// TS converts a time to a Timestamp.
func TS(t time.Time) Timestamp { return Timestamp{t} }

// TSPtr converts a time to a *Timestamp. The zero time gives nil, which omits the
// field from the JSON.
func TSPtr(t time.Time) *Timestamp {
	if t.IsZero() {
		return nil
	}
	return &Timestamp{t}
}

// MarshalJSON writes the date in the contract format.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.UTC().Format(timestampLayout))
}

// UnmarshalJSON reads an RFC 3339 date.
func (t *Timestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("invalid date: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("invalid date %q: %w", s, err)
	}
	t.Time = parsed.UTC()
	return nil
}
