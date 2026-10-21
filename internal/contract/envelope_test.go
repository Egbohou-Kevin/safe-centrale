package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/rand"
	"testing"
	"time"
)

func seededEntropy() *rand.Rand { return rand.New(rand.NewSource(1)) }

func TestIDFormatAndOrder(t *testing.T) {
	now := time.Date(2026, 10, 17, 21, 4, 5, 0, time.UTC)
	g := NewIDGeneratorWith(func() time.Time { return now }, seededEntropy())

	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 1000; i++ {
		// 100 identifiers per millisecond, so ties are exercised.
		if i%100 == 0 {
			now = now.Add(time.Millisecond)
		}
		id := g.New()
		if !ValidID(id) {
			t.Fatalf("invalid identifier: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate: %q", id)
		}
		seen[id] = true
		if id <= prev {
			t.Fatalf("not increasing: %q after %q", id, prev)
		}
		prev = id
	}
}

// A clock set back (NTP correcting the time after a power cut) must not break the
// order of the identifiers.
func TestIDsStillIncreaseWhenTheClockStepsBack(t *testing.T) {
	now := time.Date(2026, 10, 17, 21, 4, 5, 0, time.UTC)
	g := NewIDGeneratorWith(func() time.Time { return now }, seededEntropy())
	prev := g.New()
	for _, step := range []time.Duration{-time.Hour, -time.Millisecond, 0, time.Millisecond, 2 * time.Hour} {
		now = now.Add(step)
		id := g.New()
		if !ValidID(id) || id <= prev {
			t.Fatalf("after a step of %v: %q does not follow %q", step, id, prev)
		}
		prev = id
	}
	// The epoch itself is a time like another: still increasing.
	g = NewIDGeneratorWith(func() time.Time { return time.Unix(0, 0) }, seededEntropy())
	if a, b := g.New(), g.New(); a >= b {
		t.Fatalf("%q then %q", a, b)
	}
}

func TestIDEncoding(t *testing.T) {
	var ones [16]byte
	for i := range ones {
		ones[i] = 0xff
	}
	for raw, want := range map[[16]byte]string{
		{}:   "00000000000000000000000000",
		ones: "7ZZZZZZZZZZZZZZZZZZZZZZZZZ",
		{0x01, 0x8f, 0x2e, 0x3a, 0x4b, 0x5c, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}: "01HWQ3MJTW041061050R3GG28A",
	} {
		if got := encodeID(raw); got != want {
			t.Errorf("%x: got %s, want %s", raw, got, want)
		}
	}
}

func TestIDGeneratorRealClock(t *testing.T) {
	g := NewIDGenerator()
	a, b := g.New(), g.New()
	if !ValidID(a) || !ValidID(b) || a >= b {
		t.Fatalf("want two valid increasing identifiers, got %q %q", a, b)
	}
}

func TestTimestampJSON(t *testing.T) {
	in := time.Date(2026, 10, 17, 23, 4, 5, 312_000_000, time.FixedZone("UTC+2", 2*3600))
	b, err := json.Marshal(TS(in))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-10-17T21:04:05.312Z"` {
		t.Fatalf("unexpected format: %s", b)
	}
	var out Timestamp
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Equal(in) {
		t.Fatalf("round trip: %v != %v", out.Time, in)
	}
	if err := json.Unmarshal([]byte(`"not a date"`), &out); err == nil {
		t.Fatal("an invalid date must be refused")
	}
}

func newTestEnvelope(t *testing.T, data any) Envelope {
	t.Helper()
	id := NewIDGeneratorWith(func() time.Time { return time.Unix(1_700_000_000, 0) }, seededEntropy()).New()
	e, err := NewEnvelope("tg-lom-0001", "adapter.zigbee", 48213, id,
		time.Date(2026, 10, 17, 21, 4, 5, 312_000_000, time.UTC), TypeDeviceEvent, data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEnvelopeRoundTrip(t *testing.T) {
	want := DeviceEventData{DeviceID: "dev_k7p2", Capability: CapContact, Value: json.RawMessage(`"open"`)}
	e := newTestEnvelope(t, want)

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Site != "tg-lom-0001" || got.Seq != 48213 || got.V != 1 || got.Type != TypeDeviceEvent {
		t.Fatalf("envelope read wrongly: %+v", got)
	}
	data, err := DecodeData[DeviceEventData](got)
	if err != nil {
		t.Fatal(err)
	}
	if data.DeviceID != want.DeviceID || data.StringValue() != "open" {
		t.Fatalf("data read wrongly: %+v", data)
	}
	if err := data.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeIgnoresUnknownFields(t *testing.T) {
	e := newTestEnvelope(t, map[string]any{"device_id": "d", "capability": "contact", "value": "open", "future": 1})
	b, _ := json.Marshal(e)
	b = bytes.Replace(b, []byte(`"v":1`), []byte(`"v":1,"future_field":true`), 1)
	got, err := ParseEnvelope(b)
	if err != nil {
		t.Fatalf("an unknown field must not break anything: %v", err)
	}
	if _, err := DecodeData[DeviceEventData](got); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeRejects(t *testing.T) {
	good := newTestEnvelope(t, map[string]any{})
	cases := map[string]func(*Envelope){
		"future version":  func(e *Envelope) { e.V = Version + 1 },
		"zero version":    func(e *Envelope) { e.V = 0 },
		"malformed id":    func(e *Envelope) { e.ID = "not-a-ulid" },
		"empty site":      func(e *Envelope) { e.Site = "" },
		"uppercase site":  func(e *Envelope) { e.Site = "TG-LOM" },
		"empty src":       func(e *Envelope) { e.Src = "" },
		"empty type":      func(e *Envelope) { e.Type = "" },
		"missing ts":      func(e *Envelope) { e.TS = Timestamp{} },
		"missing data":    func(e *Envelope) { e.Data = nil },
		"data not object": func(e *Envelope) { e.Data = json.RawMessage(`"text"`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := good
			mutate(&e)
			err := e.Validate()
			if err == nil {
				t.Fatal("want an error")
			}
			if name == "future version" && !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("want ErrUnsupportedVersion, got %v", err)
			}
			if name != "future version" && !errors.Is(err, ErrInvalid) {
				t.Fatalf("want ErrInvalid, got %v", err)
			}
		})
	}
	if _, err := ParseEnvelope([]byte(`{not json`)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("broken JSON must give ErrInvalid, got %v", err)
	}
}
