//go:build distribution

package fair_test

import "testing"

// Run by the nightly (-tags distribution). 10 million spins over 12 slots; chi-square critical value 31.26 (p = 0.001).
// Over 7 slots (a count that does not divide 2^32, so rejection sampling matters) the critical value is 22.46.
func TestSlot_Distribution10M(t *testing.T) {
	if x2 := chiSquare(t, 10_000_000, 12); x2 > 31.26 {
		t.Fatalf("12 slots: chi-square %.2f", x2)
	}
	if x2 := chiSquare(t, 10_000_000, 7); x2 > 22.46 {
		t.Fatalf("7 slots: chi-square %.2f", x2)
	}
}
