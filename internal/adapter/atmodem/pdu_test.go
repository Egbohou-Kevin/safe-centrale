package atmodem

import (
	"testing"

	"safehome/internal/adapter/atmodem/attest"
)

func TestDecodeDeliver(t *testing.T) {
	for _, c := range []struct {
		name, pdu, from, text string
	}{
		// The example of the usual references: GSM 7-bit, international sender.
		{"7-bit", "07911326040000F0040B911346610089F60000208062917314080CC8F71D14969741F977FD07", "+31641600986", "How are you?"},
		// UCS-2, a national number, no service centre.
		{"ucs2", "00040881028967450008000000000000000E004A00650020006700E800720065", "20987654", "Je g\u00e8re"},
		// An alphanumeric sender ("Info"), and a part of a long SMS behind its header.
		{"header", "004407D049B7F90D00000000000000000008050003CC020162", "Info", "1"},
		{"simulator", attest.DeliverPDU("+22890123456", "OK\n+CMT: \"+22890000000\",,\"x\"\n1"), "+22890123456", "OK\n+CMT: \"+22890000000\",,\"x\"\n1"},
		{"simulator ucs2", attest.DeliverPDU("+22890123456", "d\u00e9j\u00e0 \u20ac"), "+22890123456", "d\u00e9j\u00e0 \u20ac"},
	} {
		t.Run(c.name, func(t *testing.T) {
			from, text, err := decodeDeliver(c.pdu)
			if err != nil || from != c.from || text != c.text {
				t.Fatalf("got %q %q %v, want %q %q", from, text, err, c.from, c.text)
			}
		})
	}
	for _, bad := range []string{"", "zz", "00", "0001", "07911326040000F0040B911346610089F6000020806291731408FF"} {
		if _, _, err := decodeDeliver(bad); err == nil {
			t.Errorf("%q: an unreadable PDU must fail", bad)
		}
	}
	if len(gsmBasic) != 128 {
		t.Fatalf("the GSM alphabet has %d characters, want 128", len(gsmBasic))
	}
}
