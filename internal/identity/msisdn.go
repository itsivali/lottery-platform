package identity

import (
	"errors"
	"strings"
)

// ErrInvalidMSISDN means the phone number is not a Kenyan mobile number.
var ErrInvalidMSISDN = errors.New("identity: invalid Kenyan mobile number")

// NormalizeMSISDN converts 07xx, 01xx, +254..., 254... or 7xx/1xx forms to the canonical 2547XXXXXXXX / 2541XXXXXXXX.
func NormalizeMSISDN(raw string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '+' && b.Len() == 0:
		default:
			return "", ErrInvalidMSISDN
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "254") && len(d) == 12:
	case strings.HasPrefix(d, "0") && len(d) == 10:
		d = "254" + d[1:]
	case len(d) == 9 && (d[0] == '7' || d[0] == '1'):
		d = "254" + d
	default:
		return "", ErrInvalidMSISDN
	}
	if d[3] != '7' && d[3] != '1' {
		return "", ErrInvalidMSISDN
	}
	return d, nil
}
