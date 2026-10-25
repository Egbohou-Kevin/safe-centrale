package atmodem

import (
	"testing"
	"time"

	"safehome/internal/adapter/atmodem/attest"
)

func TestDecodeDeliver(t *testing.T) {
	stamp := time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		name, pdu, from, text string
		sent                  time.Time
	}{
		// The example of the usual references: GSM 7-bit, international sender, sent
		// 2002-08-26 19:37:41 (its zone octet, 08, is minus zero quarters in the
		// semi-octets of 3GPP TS 23.040).
		{"7-bit", "07911326040000F0040B911346610089F60000208062917314080CC8F71D14969741F977FD07", "+31641600986", "How are you?", time.Date(2002, 8, 26, 19, 37, 41, 0, time.UTC)},
		// UCS-2, a national number, no service centre, a time stamp of zeros.
		{"ucs2", "00040881028967450008000000000000000E004A00650020006700E800720065", "20987654", "Je g\u00e8re", time.Time{}},
		// An alphanumeric sender ("Info"), and a part of a long SMS behind its header.
		{"header", "004407D049B7F90D00000000000000000008050003CC020162", "Info", "1", time.Time{}},
		{"simulator", attest.DeliverPDUAt("+22890123456", "OK\n+CMT: \"+22890000000\",,\"x\"\n1", stamp), "+22890123456", "OK\n+CMT: \"+22890000000\",,\"x\"\n1", stamp},
		{"simulator ucs2", attest.DeliverPDUAt("+22890123456", "d\u00e9j\u00e0 \u20ac", stamp), "+22890123456", "d\u00e9j\u00e0 \u20ac", stamp},
	} {
		t.Run(c.name, func(t *testing.T) {
			from, text, sent, err := decodeDeliver(c.pdu)
			if err != nil || from != c.from || text != c.text || !sent.Equal(c.sent) {
				t.Fatalf("got %q %q %v %v, want %q %q %v", from, text, sent, err, c.from, c.text, c.sent)
			}
		})
	}
	// A zone west of Greenwich: 2026-10-25 06:30 at UTC-3 (12 quarters, sign bit set).
	if got := serviceCentreTime([]byte{0x62, 0x01, 0x52, 0x60, 0x03, 0x00, 0x29}); !got.Equal(stamp) {
		t.Fatalf("UTC-3: %v", got)
	}
	for _, bad := range []string{"", "zz", "00", "0001", "07911326040000F0040B911346610089F6000020806291731408FF"} {
		if _, _, _, err := decodeDeliver(bad); err == nil {
			t.Errorf("%q: an unreadable PDU must fail", bad)
		}
	}
	if len(gsmBasic) != 128 {
		t.Fatalf("the GSM alphabet has %d characters, want 128", len(gsmBasic))
	}
}
