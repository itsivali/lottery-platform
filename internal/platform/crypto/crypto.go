// Package crypto holds the primitives for field-level encryption and keyed hashing.
//
// Envelope format: version(1) | key id(1) | wrapped DEK (nonce12 + 32 + tag16) | nonce(12) | ciphertext+tag.
// A fresh random data key encrypts each value; a key-encryption key (KEK) from the keyring wraps the data key.
// KEKs come from deploy-time secrets and never enter the database, so no database role can decrypt on its own.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
)

const (
	version    = 1
	keySize    = 32
	nonceSize  = 12
	tagSize    = 16
	wrappedLen = nonceSize + keySize + tagSize
	headerLen  = 2 + wrappedLen + nonceSize
)

// Keyring holds the KEKs by id and the id used for new encryptions.
type Keyring struct {
	active byte
	keys   map[byte][]byte
}

// NewKeyring builds a Keyring. Every key must be 32 bytes and the active id must be present.
func NewKeyring(active byte, keys map[byte][]byte) (*Keyring, error) {
	kr := &Keyring{active: active, keys: make(map[byte][]byte, len(keys))}
	for id, k := range keys {
		if len(k) != keySize {
			return nil, fmt.Errorf("crypto: key %d must be %d bytes", id, keySize)
		}
		kr.keys[id] = append([]byte(nil), k...)
	}
	if _, ok := kr.keys[active]; !ok {
		return nil, fmt.Errorf("crypto: active key %d is not in the keyring", active)
	}
	return kr, nil
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Encrypt seals plaintext, binding it to aad (for example "kyc.id_number") so a value cannot be moved to another field.
func (k *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, keySize)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	kek, err := gcm(k.keys[k.active])
	if err != nil {
		return nil, err
	}
	wrapNonce := make([]byte, nonceSize)
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, err
	}
	out := []byte{version, k.active}
	out = append(out, wrapNonce...)
	out = kek.Seal(out, wrapNonce, dek, []byte{version, k.active})
	// out now holds header + wrapNonce + wrappedDEK (+tag)
	data, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out = append(out, nonce...)
	return data.Seal(out, nonce, plaintext, aad), nil
}

// Decrypt opens a value produced by Encrypt with the same aad.
func (k *Keyring) Decrypt(blob, aad []byte) ([]byte, error) {
	if len(blob) < headerLen+tagSize || blob[0] != version {
		return nil, errors.New("crypto: malformed ciphertext")
	}
	key, ok := k.keys[blob[1]]
	if !ok {
		return nil, fmt.Errorf("crypto: unknown key id %d", blob[1])
	}
	kek, err := gcm(key)
	if err != nil {
		return nil, err
	}
	wrapNonce := blob[2 : 2+nonceSize]
	dek, err := kek.Open(nil, wrapNonce, blob[2+nonceSize:2+wrappedLen], blob[:2])
	if err != nil {
		return nil, errors.New("crypto: cannot unwrap data key")
	}
	data, err := gcm(dek)
	if err != nil {
		return nil, err
	}
	nonce := blob[2+wrappedLen : headerLen]
	pt, err := data.Open(nil, nonce, blob[headerLen:], aad)
	if err != nil {
		return nil, errors.New("crypto: cannot decrypt")
	}
	return pt, nil
}

// MAC returns HMAC-SHA256 over the length-prefixed fields, domain-separated so one key can serve several purposes
// (OTP codes, refresh-token hashes, national-ID lookup hashes) without collisions.
func MAC(key []byte, domain string, fields ...string) []byte {
	h := hmac.New(sha256.New, key)
	write := func(s string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s))) //nolint:gosec // field lengths are tiny
		h.Write(n[:])
		h.Write([]byte(s))
	}
	write(domain)
	for _, f := range fields {
		write(f)
	}
	return h.Sum(nil)
}

// Equal compares two byte strings in constant time.
func Equal(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// RandomDigits returns n uniformly random decimal digits from crypto/rand.
func RandomDigits(n int) (string, error) {
	out := make([]byte, n)
	ten := big.NewInt(10)
	for i := range out {
		d, err := rand.Int(rand.Reader, ten)
		if err != nil {
			return "", err
		}
		out[i] = '0' + byte(d.Uint64()) //nolint:gosec // d is in [0,10)
	}
	return string(out), nil
}

// RandomToken returns n random bytes as unpadded URL-safe base64.
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
