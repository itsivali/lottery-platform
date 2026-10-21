package password_test

import (
	"strings"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/password"
)

var pepper = []byte("0123456789abcdef0123456789abcdef")

func TestHashAndVerify_RoundTrip(t *testing.T) {
	h, err := password.Hash("correct horse battery", pepper)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q is not argon2id with the OWASP minimum parameters", h)
	}
	if ok, err := password.Verify("correct horse battery", h, pepper); err != nil || !ok {
		t.Fatalf("right password: %v %v", ok, err)
	}
	if ok, _ := password.Verify("correct horse batterY", h, pepper); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestHash_IsSaltedAndPepperDependent(t *testing.T) {
	a, _ := password.Hash("same password 1", pepper)
	b, _ := password.Hash("same password 1", pepper)
	if a == b {
		t.Fatal("two hashes of one password are identical: no salt")
	}
	if ok, _ := password.Verify("same password 1", a, []byte("a-different-pepper-0123456789ab")); ok {
		t.Fatal("verified with the wrong pepper: a database leak alone would be enough to crack hashes")
	}
}

func TestVerify_RejectsMalformedHashesWithoutPanicking(t *testing.T) {
	for _, bad := range []string{"", "plain", "$argon2id$", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=x,t=2,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!$aGFzaA", "$argon2id$v=19$m=99999999,t=99,p=99$c2FsdA$aGFzaA"} {
		if ok, err := password.Verify("whatever 123", bad, pepper); ok || err == nil {
			t.Errorf("hash %q: ok=%v err=%v, want a refusal with an error", bad, ok, err)
		}
	}
}

func TestPolicy(t *testing.T) {
	cases := []struct {
		pw, msisdn string
		ok         bool
	}{
		{"correct horse battery", "254712345678", true},
		{"a1b2c3d4", "254712345678", true},
		{"short7!", "254712345678", false},      // under 8
		{"password", "254712345678", false},     // common
		{"Password1", "254712345678", false},    // common once case and a trailing digit are ignored
		{"12345678", "254712345678", false},     // common
		{"aaaaaaaaaa", "254712345678", false},   // one repeated character
		{"0712345678", "254712345678", false},   // their own number
		{"254712345678", "254712345678", false}, // their own number
		{"my number is 712345678", "254712345678", false},
		{strings.Repeat("x", 129), "254712345678", false}, // absurdly long: also a hashing-cost attack
		{"pässwörd mit ümlaut", "254712345678", true},
	}
	for _, c := range cases {
		err := password.CheckPolicy(c.pw, c.msisdn)
		if (err == nil) != c.ok {
			t.Errorf("CheckPolicy(%q) = %v, want ok=%v", c.pw, err, c.ok)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	h, _ := password.Hash("some password 99", pepper)
	if password.NeedsRehash(h) {
		t.Error("a fresh hash needs rehashing")
	}
	if !password.NeedsRehash("$argon2id$v=19$m=4096,t=1,p=1$c2FsdA$aGFzaA") {
		t.Error("a weaker parameter set must be flagged for upgrade")
	}
}
