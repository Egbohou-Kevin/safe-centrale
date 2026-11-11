// Package sealcode seals the access code on the phone for the hub's link agent, so
// that the cloud which carries the order never sees it. It is ECIES on P-256: the
// phone draws a key pair for each order, derives a key from its secret and the hub's
// public key (ECDH, then HKDF-SHA256) and encrypts with AES-256-GCM. The format is in
// docs/contract.md; the standard library is all it needs.
package sealcode

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	version  = 1
	pubLen   = 65 // an uncompressed P-256 point
	nonceLen = 12
	plainLen = 16 // the phone's time (8 bytes), then the digits padded to 8
	info     = "safehome/code/v1"
)

// Window is how far the phone's time may be from the hub's: wider than the clock
// drift of a phone, short enough that a sealed code kept by the cloud goes stale.
const Window = 5 * time.Minute

// ErrBad is returned for a seal that cannot be opened: wrong key, other site or
// command, altered, stale, or not a code. It says no more, to leak nothing.
var ErrBad = errors.New("sealcode: bad seal")

var codeForm = regexp.MustCompile(`^[0-9]{4,8}$`)

// ValidCode reports whether code has the form of the hub's codes: 4 to 8 ASCII digits.
func ValidCode(code string) bool { return codeForm.MatchString(code) }

// Key is the hub's key pair.
type Key struct{ priv *ecdh.PrivateKey }

// Generate draws a new key.
func Generate() (*Key, error) {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sealcode: %w", err)
	}
	return &Key{k}, nil
}

// Parse reads the private key written by Encode. Spaces around it are ignored: a file
// written by hand often ends with a newline. Its errors never quote the key.
func Parse(s string) (*Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, errors.New("sealcode: the key is not base64")
	}
	k, err := ecdh.P256().NewPrivateKey(b)
	if err != nil {
		return nil, errors.New("sealcode: not a P-256 private key")
	}
	return &Key{k}, nil
}

// Encode writes the private key, for the file that keeps it (mode 0600).
func (k *Key) Encode() string { return base64.StdEncoding.EncodeToString(k.priv.Bytes()) }

// Public is the public key, 65 bytes in base64: what the cloud gives the phones.
func (k *Key) Public() string { return base64.StdEncoding.EncodeToString(k.priv.PublicKey().Bytes()) }

// Opened is a code taken out of a seal.
type Opened struct {
	Code string
	// Ephemeral is the phone's one-time public key, which names the seal: the same one
	// seen twice is a replay.
	Ephemeral string
	// At is the phone's time in the seal: the seal opens until At+Window on the hub's
	// clock, so its Ephemeral must be remembered until then.
	At time.Time
}

func aad(site, command string) []byte { return []byte(info + "|" + site + "|" + command) }

func derive(secret, ephemeral, hub []byte) (cipher.AEAD, error) {
	key, err := hkdf.Key(sha256.New, secret, append(append([]byte{}, ephemeral...), hub...), info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Open opens a seal made for this hub, this site and this command, at time now. It
// does not remember the seal: a replay is for the caller to refuse (Opened.Ephemeral).
func (k *Key) Open(sealed, site, command string, now time.Time) (Opened, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) != 1+pubLen+nonceLen+plainLen+16 || raw[0] != version {
		return Opened{}, ErrBad
	}
	eph, err := ecdh.P256().NewPublicKey(raw[1 : 1+pubLen])
	if err != nil {
		return Opened{}, ErrBad
	}
	secret, err := k.priv.ECDH(eph)
	if err != nil {
		return Opened{}, ErrBad
	}
	gcm, err := derive(secret, eph.Bytes(), k.priv.PublicKey().Bytes())
	if err != nil {
		return Opened{}, ErrBad
	}
	nonce, body := raw[1+pubLen:1+pubLen+nonceLen], raw[1+pubLen+nonceLen:]
	plain, err := gcm.Open(nil, nonce, body, aad(site, command))
	if err != nil {
		return Opened{}, ErrBad
	}
	at := time.Unix(int64(binary.BigEndian.Uint64(plain[:8])), 0)
	if d := now.Sub(at); d > Window || d < -Window {
		return Opened{}, ErrBad
	}
	code := string(trimZeros(plain[8:]))
	if !ValidCode(code) {
		return Opened{}, ErrBad
	}
	return Opened{Code: code, Ephemeral: base64.StdEncoding.EncodeToString(raw[1 : 1+pubLen]), At: at}, nil
}

func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// Seal is what the phone does: it seals code for the hub whose public key is hubPublic,
// for this site and command, stamped with now. The app has its own copy of this code
// (Kotlin); a test vector in this package's tests ties the two.
func Seal(hubPublic, code, site, command string, now time.Time) (string, error) {
	return sealWith(rand.Reader, hubPublic, code, site, command, now)
}

func sealWith(rnd io.Reader, hubPublic, code, site, command string, now time.Time) (string, error) {
	if !ValidCode(code) {
		return "", errors.New("sealcode: not a code")
	}
	hb, err := base64.StdEncoding.DecodeString(hubPublic)
	if err != nil {
		return "", errors.New("sealcode: the hub key is not base64")
	}
	hub, err := ecdh.P256().NewPublicKey(hb)
	if err != nil {
		return "", errors.New("sealcode: not a P-256 public key")
	}
	eph, err := ecdh.P256().GenerateKey(rnd)
	if err != nil {
		return "", err
	}
	secret, err := eph.ECDH(hub)
	if err != nil {
		return "", err
	}
	gcm, err := derive(secret, eph.PublicKey().Bytes(), hub.Bytes())
	if err != nil {
		return "", err
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return "", err
	}
	plain := make([]byte, plainLen)
	binary.BigEndian.PutUint64(plain, uint64(now.Unix()))
	copy(plain[8:], code)
	out := append([]byte{version}, eph.PublicKey().Bytes()...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plain, aad(site, command))
	return base64.StdEncoding.EncodeToString(out), nil
}
