package atmodem

import (
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf16"
)

// Stored SMS are read in PDU mode (3GPP TS 23.040): the answer to AT+CMGR or AT+CMGL
// gives each SMS as one line of hexadecimal digits, which nothing written in an SMS can
// break. In text mode its text came raw, line breaks included, and a text line reading
// OK ended the answer early: the lines after it were taken for the modem's own.

var errPDU = errors.New("atmodem: unreadable SMS")

// pduReader walks the octets of a PDU.
type pduReader struct {
	b   []byte
	err error
}

func (r *pduReader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = errPDU
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *pduReader) octet() byte {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

// Alphabets of the user data, from the data coding scheme.
const (
	alphabet7bit = iota
	alphabet8bit
	alphabetUCS2
)

// decodeDeliver reads an SMS-DELIVER PDU, as AT+CMGR gives it in PDU mode (with the
// service centre's address first), and returns the sender, the text, and when the
// service centre took it (zero if its time stamp cannot be read).
func decodeDeliver(pdu string) (from, text string, sent time.Time, err error) {
	b, err := hex.DecodeString(strings.TrimSpace(pdu))
	if err != nil {
		return "", "", time.Time{}, errPDU
	}
	r := &pduReader{b: b}
	r.take(int(r.octet())) // the service centre
	first := r.octet()
	if first&0x03 != 0 {
		return "", "", time.Time{}, errPDU // not an SMS-DELIVER
	}
	from = r.address()
	r.octet() // protocol identifier
	dcs := r.octet()
	sent = serviceCentreTime(r.take(7))
	length := int(r.octet())
	text = r.userData(dcs, length, first&0x40 != 0)
	if r.err != nil {
		return "", "", time.Time{}, r.err
	}
	return from, text, sent, nil
}

// serviceCentreTime reads the service centre's time stamp: year, month, day, hour,
// minute, second in swapped semi-octets, then the zone in quarters of an hour, its sign
// in bit 3. A stamp that cannot be read gives the zero time.
func serviceCentreTime(b []byte) time.Time {
	if len(b) != 7 {
		return time.Time{}
	}
	var v [6]int
	for i := range v {
		lo, hi := int(b[i]&0x0f), int(b[i]>>4)
		if lo > 9 || hi > 9 {
			return time.Time{}
		}
		v[i] = lo*10 + hi
	}
	quarters := int(b[6]&0x07)*10 + int(b[6]>>4)
	if b[6]&0x08 != 0 {
		quarters = -quarters
	}
	zone := time.FixedZone("", quarters*15*60)
	t := time.Date(2000+v[0], time.Month(v[1]), v[2], v[3], v[4], v[5], 0, zone)
	if v[1] < 1 || v[1] > 12 || t.Day() != v[2] {
		return time.Time{} // month or day out of range: Date would have moved it
	}
	return t.UTC()
}

// address reads the originating address: digits in swapped semi-octets, "+" first for
// an international number; an alphanumeric sender (a company name) comes as text.
func (r *pduReader) address() string {
	digits := int(r.octet())
	toa := r.octet()
	raw := r.take((digits + 1) / 2)
	if r.err != nil {
		return ""
	}
	if toa&0x70 == 0x50 { // alphanumeric
		return gsmText(unpack7(raw, digits*4/7))
	}
	var sb strings.Builder
	if toa&0x70 == 0x10 {
		sb.WriteByte('+')
	}
	for i := 0; i < digits; i++ {
		d := raw[i/2] >> (4 * uint(i%2)) & 0x0f
		if d > 9 {
			r.err = errPDU
			return ""
		}
		sb.WriteByte('0' + d)
	}
	return sb.String()
}

// userData reads the text: length counts septets in the GSM alphabet, octets
// otherwise. A user data header (a part of a long SMS) is skipped.
func (r *pduReader) userData(dcs byte, length int, header bool) string {
	alphabet, ok := alphabetOf(dcs)
	if !ok {
		r.err = errPDU
		return ""
	}
	if alphabet != alphabet7bit {
		ud := r.take(length)
		if header && len(ud) > 0 {
			skip := min(int(ud[0])+1, len(ud))
			ud = ud[skip:]
		}
		if alphabet == alphabetUCS2 {
			return ucs2(ud)
		}
		return string(ud)
	}
	ud := r.take((length*7 + 7) / 8)
	if r.err != nil {
		return ""
	}
	septets := unpack7(ud, length)
	if header && len(ud) > 0 {
		skip := min((8*(int(ud[0])+1)+6)/7, len(septets))
		septets = septets[skip:]
	}
	return gsmText(septets)
}

// alphabetOf reads the alphabet of a data coding scheme. Compressed text is not read.
func alphabetOf(dcs byte) (int, bool) {
	switch dcs >> 4 {
	case 0x0, 0x1, 0x2, 0x3, 0x4, 0x5, 0x6, 0x7:
		if dcs&0x20 != 0 {
			return 0, false
		}
		switch (dcs >> 2) & 0x03 {
		case 1:
			return alphabet8bit, true
		case 2:
			return alphabetUCS2, true
		}
		return alphabet7bit, true
	case 0xe:
		return alphabetUCS2, true
	case 0xf:
		if dcs&0x04 != 0 {
			return alphabet8bit, true
		}
	}
	return alphabet7bit, true
}

// unpack7 unpacks n septets.
func unpack7(b []byte, n int) []byte {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		bit := i * 7
		k, shift := bit/8, uint(bit%8)
		if k >= len(b) {
			break
		}
		v := int(b[k]) >> shift
		if shift > 1 && k+1 < len(b) {
			v |= int(b[k+1]) << (8 - shift)
		}
		out = append(out, byte(v&0x7f))
	}
	return out
}

func ucs2(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(units))
}

// gsmBasic is the GSM 7-bit default alphabet; 0x1b escapes to gsmExtension.
var gsmBasic = []rune("@\u00a3$\u00a5\u00e8\u00e9\u00f9\u00ec\u00f2\u00c7\n\u00d8\u00f8\r\u00c5\u00e5" +
	"\u0394_\u03a6\u0393\u039b\u03a9\u03a0\u03a8\u03a3\u0398\u039e\x1b\u00c6\u00e6\u00df\u00c9" +
	" !\"#\u00a4%&'()*+,-./0123456789:;<=>?" +
	"\u00a1ABCDEFGHIJKLMNOPQRSTUVWXYZ\u00c4\u00d6\u00d1\u00dc\u00a7" +
	"\u00bfabcdefghijklmnopqrstuvwxyz\u00e4\u00f6\u00f1\u00fc\u00e0")

var gsmExtension = map[byte]rune{
	0x0a: '\f', 0x14: '^', 0x28: '{', 0x29: '}', 0x2f: '\\',
	0x3c: '[', 0x3d: '~', 0x3e: ']', 0x40: '|', 0x65: '\u20ac',
}

func gsmText(septets []byte) string {
	var sb strings.Builder
	for i := 0; i < len(septets); i++ {
		c := septets[i]
		if c == 0x1b && i+1 < len(septets) {
			i++
			if r, ok := gsmExtension[septets[i]]; ok {
				sb.WriteRune(r)
				continue
			}
			c = septets[i]
		}
		sb.WriteRune(gsmBasic[c&0x7f])
	}
	return sb.String()
}
