package crypto_test

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/crypto"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEnvelope_RoundTrip(t *testing.T) {
	kr, err := crypto.NewKeyring(1, map[byte][]byte{1: key(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range [][]byte{[]byte("12345678"), {}, bytes.Repeat([]byte("x"), 10_000)} {
		ct, err := kr.Encrypt(pt, []byte("kyc:id"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := kr.Decrypt(ct, []byte("kyc:id"))
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("round trip failed: %v", err)
		}
	}
}

func TestEnvelope_CiphertextIsRandomisedAndHidesPlaintext(t *testing.T) {
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: key(t)})
	a, _ := kr.Encrypt([]byte("12345678"), nil)
	b, _ := kr.Encrypt([]byte("12345678"), nil)
	if bytes.Equal(a, b) {
		t.Fatal("same plaintext produced identical ciphertexts")
	}
	if bytes.Contains(a, []byte("12345678")) {
		t.Fatal("plaintext visible in ciphertext")
	}
}

func TestEnvelope_RejectsTamperingWrongContextAndWrongKey(t *testing.T) {
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: key(t)})
	ct, _ := kr.Encrypt([]byte("secret"), []byte("ctx-a"))
	if _, err := kr.Decrypt(ct, []byte("ctx-b")); err == nil {
		t.Error("decrypt with a different context succeeded")
	}
	for i := range ct {
		bad := append([]byte(nil), ct...)
		bad[i] ^= 0x01
		if _, err := kr.Decrypt(bad, []byte("ctx-a")); err == nil {
			t.Fatalf("flipping byte %d went undetected", i)
		}
	}
	other, _ := crypto.NewKeyring(1, map[byte][]byte{1: key(t)})
	if _, err := other.Decrypt(ct, []byte("ctx-a")); err == nil {
		t.Error("decrypt with a different key succeeded")
	}
	if _, err := kr.Decrypt(ct[:5], []byte("ctx-a")); err == nil {
		t.Error("truncated ciphertext accepted")
	}
}

func TestKeyring_SupportsRotation(t *testing.T) {
	k1, k2 := key(t), key(t)
	old, _ := crypto.NewKeyring(1, map[byte][]byte{1: k1})
	ct, _ := old.Encrypt([]byte("legacy"), nil)
	rotated, err := crypto.NewKeyring(2, map[byte][]byte{1: k1, 2: k2})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Decrypt(ct, nil); err != nil || string(got) != "legacy" {
		t.Fatalf("old ciphertext unreadable after rotation: %v", err)
	}
	fresh, _ := rotated.Encrypt([]byte("new"), nil)
	if fresh[1] != 2 {
		t.Fatalf("new data used key id %d, want 2", fresh[1])
	}
}

func TestKeyring_RejectsBadConfiguration(t *testing.T) {
	if _, err := crypto.NewKeyring(1, map[byte][]byte{1: []byte("short")}); err == nil {
		t.Error("short key accepted")
	}
	if _, err := crypto.NewKeyring(2, map[byte][]byte{1: key(t)}); err == nil {
		t.Error("missing active key accepted")
	}
}

func TestMAC_IsDeterministicKeyedAndDomainSeparated(t *testing.T) {
	k := key(t)
	a := crypto.MAC(k, "otp", "254712345678", "123456")
	if !bytes.Equal(a, crypto.MAC(k, "otp", "254712345678", "123456")) {
		t.Fatal("not deterministic")
	}
	if bytes.Equal(a, crypto.MAC(key(t), "otp", "254712345678", "123456")) {
		t.Fatal("different keys gave the same MAC")
	}
	if bytes.Equal(a, crypto.MAC(k, "nid", "254712345678", "123456")) {
		t.Fatal("different domains gave the same MAC")
	}
	if bytes.Equal(crypto.MAC(k, "d", "ab", "c"), crypto.MAC(k, "d", "a", "bc")) {
		t.Fatal("field boundaries are ambiguous")
	}
}

func TestRandomDigitsAndToken(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		d, err := crypto.RandomDigits(6)
		if err != nil || len(d) != 6 {
			t.Fatalf("digits %q, %v", d, err)
		}
		for _, c := range d {
			if c < '0' || c > '9' {
				t.Fatalf("non-digit in %q", d)
			}
		}
		seen[d] = true
	}
	if len(seen) < 150 {
		t.Fatalf("only %d distinct codes in 200 draws", len(seen))
	}
	a, _ := crypto.RandomToken(32)
	b, _ := crypto.RandomToken(32)
	if a == b || len(a) < 43 {
		t.Fatalf("tokens %q %q", a, b)
	}
}

func TestEqual_ConstantTimeComparison(t *testing.T) {
	if !crypto.Equal([]byte("a"), []byte("a")) || crypto.Equal([]byte("a"), []byte("b")) || crypto.Equal([]byte("a"), []byte("ab")) {
		t.Fatal("Equal wrong")
	}
}

func TestDecrypt_EveryTooShortBlobIsRefusedWithoutPanicking(t *testing.T) {
	kr, _ := crypto.NewKeyring(1, map[byte][]byte{1: key(t)})
	whole, _ := kr.Encrypt([]byte{}, nil) // the shortest valid value: header + tag only
	for n := range len(whole) {
		blob := whole[:n]
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("length %d panicked: %v", n, r)
				}
			}()
			if _, err := kr.Decrypt(blob, nil); err == nil {
				t.Errorf("a %d-byte prefix of a %d-byte value was accepted", n, len(whole))
			}
		}()
	}
	if _, err := kr.Decrypt(whole, nil); err != nil {
		t.Fatalf("the full minimal value must decrypt: %v", err)
	}
}
