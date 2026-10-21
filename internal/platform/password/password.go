// Package password hashes and checks account passwords.
//
// Passwords are Unicode-normalised (NFKC), run through HMAC-SHA256 with a secret pepper that lives only in the
// deployment's secret store, and then hashed with argon2id at the OWASP minimum parameters (19 MiB, 2 passes, 1 lane).
// A leaked database alone is therefore not enough to start cracking, and hashes can be upgraded in place by NeedsRehash.
package password

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const (
	memoryKiB = 19456
	passes    = 2
	lanes     = 1
	saltLen   = 16
	keyLen    = 32

	// bounds applied to parameters read back from a stored hash, so a corrupted or hostile value cannot make
	// verification allocate gigabytes or run for minutes
	maxMemoryKiB = 128 * 1024
	maxPasses    = 6
	maxLanes     = 4

	minRunes = 8
	maxRunes = 128
)

// ErrWeak is wrapped by every policy refusal.
var ErrWeak = errors.New("password: too weak")

func prepare(pw string, pepper []byte) []byte {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte(norm.NFKC.String(pw)))
	return mac.Sum(nil)
}

// Hash returns an encoded argon2id hash of pw.
func Hash(pw string, pepper []byte) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(prepare(pw, pepper), salt, passes, memoryKiB, lanes, keyLen)
	return "$argon2id$v=" + strconv.Itoa(argon2.Version) + "$m=" + strconv.Itoa(memoryKiB) + ",t=" + strconv.Itoa(passes) +
		",p=" + strconv.Itoa(lanes) + "$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

type params struct {
	m, t, p uint32
	salt    []byte
	key     []byte
}

func parse(encoded string) (params, error) {
	var p params
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return p, errors.New("password: unsupported hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, errors.New("password: unsupported argon2 version")
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.m, &p.t, &p.p); err != nil {
		return p, errors.New("password: unreadable parameters")
	}
	if p.m == 0 || p.m > maxMemoryKiB || p.t == 0 || p.t > maxPasses || p.p == 0 || p.p > maxLanes {
		return p, errors.New("password: parameters out of range")
	}
	var err error
	if p.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(p.salt) == 0 {
		return p, errors.New("password: unreadable salt")
	}
	if p.key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil || len(p.key) == 0 {
		return p, errors.New("password: unreadable hash")
	}
	return p, nil
}

// Verify reports whether pw matches the encoded hash. A malformed hash is an error, never a match.
func Verify(pw, encoded string, pepper []byte) (bool, error) {
	p, err := parse(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey(prepare(pw, pepper), p.salt, p.t, p.m, uint8(p.p), uint32(len(p.key))) //nolint:gosec // p.p is bounded by maxLanes
	return subtle.ConstantTimeCompare(got, p.key) == 1, nil
}

// NeedsRehash reports whether the stored hash uses weaker parameters than today's, so it can be upgraded at next login.
func NeedsRehash(encoded string) bool {
	p, err := parse(encoded)
	return err != nil || p.m < memoryKiB || p.t < passes || p.p < lanes
}

var common = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`password passw0rd p@ssword p@ssw0rd qwerty qwertyui qwertyuiop asdfgh asdfghjk asdfghjkl zxcvbn
		zxcvbnm letmein welcome admin administrator login iloveyou monkey dragon master sunshine princess football baseball
		superman batman trustno1 abc123 abcdefgh abcd1234 changeme default secret mpesa safaricom kenya nairobi mombasa
		lottery jackpot lucky harambee simba hakuna matata 11111111 00000000 12345678 123456789 1234567890 87654321 qazwsx
		1q2w3e4r 1qaz2wsx password123 welcome1 football1 michael jennifer jordan hunter shadow`) {
		common[w] = true
	}
}

// CheckPolicy applies the password rules: 8 to 128 characters, nothing from a list of commonly used passwords, not a single
// repeated character, and not the player's own phone number. There are no composition rules (they push people toward
// predictable patterns); length and a deny-list are what matter.
func CheckPolicy(pw, msisdn string) error {
	pw = norm.NFKC.String(pw)
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minRunes:
		return fmt.Errorf("%w: use at least %d characters", ErrWeak, minRunes)
	case n > maxRunes:
		return fmt.Errorf("%w: use at most %d characters", ErrWeak, maxRunes)
	}
	lower := strings.ToLower(pw)
	if common[lower] || common[strings.TrimRightFunc(lower, func(r rune) bool { return unicode.IsDigit(r) || unicode.IsPunct(r) })] {
		return fmt.Errorf("%w: that password is too common", ErrWeak)
	}
	if strings.Count(lower, string([]rune(lower)[0])) == n {
		return fmt.Errorf("%w: do not repeat one character", ErrWeak)
	}
	if local := strings.TrimPrefix(msisdn, "254"); len(local) >= 9 && (strings.Contains(lower, local) || strings.Contains(lower, "0"+local)) {
		return fmt.Errorf("%w: do not use your phone number", ErrWeak)
	}
	return nil
}
