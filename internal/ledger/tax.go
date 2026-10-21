package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// Rounding is the rule stored with a tax_rules / prize_structures version.
type Rounding string

// Supported rounding rules.
const (
	HalfUp Rounding = "half_up"
	Down   Rounding = "down"
	Up     Rounding = "up"
)

const e5Scale = 100_000

// Rate is a fraction in [0,1] held as an integer count of 1/100000 (matches numeric(6,5)); never a float.
type Rate struct{ e5 int64 }

// RateFromE5 builds a Rate from hundred-thousandths.
func RateFromE5(e5 int64) Rate { return Rate{e5: e5} }

// E5 returns the rate in hundred-thousandths.
func (r Rate) E5() int64 { return r.e5 }

// ParseRate parses a decimal such as "0.15" or "0.16000" (the text form of numeric(6,5)).
func ParseRate(s string) (Rate, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole != "0" && whole != "1" {
		return Rate{}, fmt.Errorf("ledger: bad rate %q", s)
	}
	if len(frac) > 5 || (strings.Contains(s, ".") && frac == "") {
		return Rate{}, fmt.Errorf("ledger: bad rate %q", s)
	}
	var e5 int64
	if whole == "1" {
		e5 = e5Scale
	}
	scale := int64(e5Scale / 10)
	for _, ch := range frac {
		if ch < '0' || ch > '9' {
			return Rate{}, fmt.Errorf("ledger: bad rate %q", s)
		}
		e5 += int64(ch-'0') * scale
		scale /= 10
	}
	if e5 > e5Scale {
		return Rate{}, fmt.Errorf("ledger: rate %q exceeds 1", s)
	}
	return Rate{e5: e5}, nil
}

// TaxRule is the version of a rate and rounding rule in force for an event.
type TaxRule struct {
	Code     string
	Rate     Rate
	Rounding Rounding
}

// Apply returns amount * rate rounded to whole KES by mode, using integer arithmetic only.
func Apply(amount int64, r Rate, mode Rounding) (int64, error) {
	if amount < 0 || amount > MaxAmountKES {
		return 0, errors.New("ledger: amount out of range")
	}
	product := amount * r.e5
	floor, rem := product/e5Scale, product%e5Scale
	switch mode {
	case Down:
		return floor, nil
	case Up:
		if rem > 0 {
			floor++
		}
		return floor, nil
	case HalfUp:
		if rem*2 >= e5Scale {
			floor++
		}
		return floor, nil
	default:
		return 0, fmt.Errorf("ledger: unknown rounding %q", mode)
	}
}
