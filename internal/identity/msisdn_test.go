package identity_test

import (
	"testing"

	"github.com/itsivali/lottery-platform/internal/identity"
)

func TestNormalizeMSISDN(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"0712345678", "254712345678", true},
		{"0112345678", "254112345678", true},
		{"+254712345678", "254712345678", true},
		{"254712345678", "254712345678", true},
		{"712345678", "254712345678", true},
		{" 0712 345 678 ", "254712345678", true},
		{"0712-345-678", "254712345678", true},
		{"254212345678", "", false}, // 2xx is not a mobile range
		{"07123456", "", false},
		{"0712345678901", "", false},
		{"255712345678", "", false}, // Tanzania
		{"abcdefghij", "", false},
		{"", "", false},
		{"+254 7123 45678; DROP TABLE users", "", false},
	}
	for _, c := range cases {
		got, err := identity.NormalizeMSISDN(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("NormalizeMSISDN(%q) = %q, %v; want %q ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
}
