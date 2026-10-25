package attest

import (
	"encoding/hex"
	"strings"
	"unicode/utf16"
)

// DeliverPDU encodes an SMS from a number as the modem gives it in PDU mode: an
// SMS-DELIVER behind its service centre's address. The text goes in the GSM 7-bit
// alphabet when every character is one ASCII shares with it, in UCS-2 otherwise.
func DeliverPDU(from, text string) string {
	var b []byte
	b = append(b, 0x07, 0x91, 0x22, 0x98, 0x00, 0x00, 0x00, 0xf0) // service centre +22890000000
	b = append(b, 0x04)                                           // SMS-DELIVER, no more messages
	b = append(b, address(from)...)
	b = append(b, 0x00) // protocol identifier
	if septets, ok := gsm7(text); ok {
		b = append(b, 0x00)
		b = append(b, 0x62, 0x01, 0x71, 0x12, 0x50, 0x00, 0x00) // time stamp
		b = append(b, byte(len(septets)))
		b = append(b, pack7(septets)...)
	} else {
		units := utf16.Encode([]rune(text))
		b = append(b, 0x08)
		b = append(b, 0x62, 0x01, 0x71, 0x12, 0x50, 0x00, 0x00)
		b = append(b, byte(2*len(units)))
		for _, u := range units {
			b = append(b, byte(u>>8), byte(u))
		}
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// address encodes a number: its digits in swapped semi-octets, international with a
// leading "+".
func address(number string) []byte {
	toa := byte(0x81)
	if strings.HasPrefix(number, "+") {
		toa, number = 0x91, number[1:]
	}
	out := []byte{byte(len(number)), toa}
	for i := 0; i < len(number); i += 2 {
		lo, hi := number[i]-'0', byte(0x0f)
		if i+1 < len(number) {
			hi = number[i+1] - '0'
		}
		out = append(out, hi<<4|lo)
	}
	return out
}

// gsm7 gives the septets of a text whose characters ASCII and the GSM alphabet share.
func gsm7(text string) ([]byte, bool) {
	const shared = " !\"#%&'()*+,-./:;<=>?\n\r"
	out := make([]byte, 0, len(text))
	for _, r := range text {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', strings.ContainsRune(shared, r):
			out = append(out, byte(r))
		default:
			return nil, false
		}
	}
	return out, true
}

func pack7(septets []byte) []byte {
	var out []byte
	var acc, bits uint
	for _, s := range septets {
		acc |= uint(s) << bits
		for bits += 7; bits >= 8; bits -= 8 {
			out = append(out, byte(acc))
			acc >>= 8
		}
	}
	if bits > 0 {
		out = append(out, byte(acc))
	}
	return out
}
