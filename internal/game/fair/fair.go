// Package fair is the provably fair spin: the specification players can check, kept free of storage and HTTP.
//
// A player's seed chain has a secret server seed (32 random bytes), shown up front only as its commitment
// (hex SHA-256 of the seed bytes), the player's client seed, and a nonce that increases by one per spin. A spin over n
// slots is derived as follows:
//
//	for round = 0, 1, 2, ...:
//	    mac = HMAC-SHA256(key = server seed bytes, message = client seed + ":" + nonce + ":" + round)   (decimal numbers)
//	    for each of the 8 big-endian 32-bit words w of mac:
//	        if w < 2^32 - (2^32 mod n): the slot is w mod n
//
// Words at or above the limit are rejected, so every slot is exactly equally likely (no modulo bias). When the chain is
// rotated the server seed is revealed: anyone can then check that its SHA-256 matches the commitment shown before the
// spins and recompute every slot (tools/verifier does this without using this package).
package fair

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
)

// SeedBytes is the length of a server seed.
const SeedBytes = 32

// MaxSlots bounds the slot count (the odds table allows far fewer).
const MaxSlots = 1 << 16

// Errors.
var (
	ErrBadSeed       = errors.New("fair: server seed must be 32 bytes")
	ErrBadClientSeed = errors.New("fair: client seed must be 1 to 64 letters, digits, '-' or '_'")
	ErrBadSlots      = errors.New("fair: slot count out of range")
	ErrBadNonce      = errors.New("fair: nonce must not be negative")
	ErrMismatch      = errors.New("fair: the server seed does not match the commitment")
)

var clientSeedRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewServerSeed returns a fresh random server seed.
func NewServerSeed() ([]byte, error) {
	seed := make([]byte, SeedBytes)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	return seed, nil
}

// NewClientSeed returns a random client seed for players who do not choose one.
func NewClientSeed() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidClientSeed reports whether s may be used as a client seed.
func ValidClientSeed(s string) bool { return clientSeedRE.MatchString(s) }

// Commitment is the hex SHA-256 of the server seed, published before the seed is used.
func Commitment(seed []byte) string {
	sum := sha256.Sum256(seed)
	return hex.EncodeToString(sum[:])
}

// Slot derives the slot of one spin.
func Slot(seed []byte, clientSeed string, nonce int64, slots int) (int, error) {
	switch {
	case len(seed) != SeedBytes:
		return 0, ErrBadSeed
	case !ValidClientSeed(clientSeed):
		return 0, ErrBadClientSeed
	case slots < 2 || slots > MaxSlots:
		return 0, ErrBadSlots
	case nonce < 0:
		return 0, ErrBadNonce
	}
	n := uint64(slots)
	limit := (1 << 32) - (1<<32)%n
	prefix := clientSeed + ":" + strconv.FormatInt(nonce, 10) + ":"
	for round := 0; ; round++ {
		m := hmac.New(sha256.New, seed)
		m.Write([]byte(prefix + strconv.Itoa(round)))
		sum := m.Sum(nil)
		for i := 0; i < len(sum); i += 4 {
			if w := uint64(binary.BigEndian.Uint32(sum[i:])); w < limit {
				return int(w % n), nil //nolint:gosec // w % n < n <= MaxSlots fits in an int
			}
		}
	}
}

// Verify checks a revealed server seed against its commitment and returns the slot of one spin.
func Verify(seedHex, commitment, clientSeed string, nonce int64, slots int) (int, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil || len(seed) != SeedBytes {
		return 0, ErrBadSeed
	}
	if !hmac.Equal([]byte(Commitment(seed)), []byte(commitment)) {
		return 0, ErrMismatch
	}
	return Slot(seed, clientSeed, nonce, slots)
}
