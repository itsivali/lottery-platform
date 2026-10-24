package fair_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"

	"github.com/itsivali/lottery-platform/internal/game/fair"
)

type vector struct {
	ServerSeed string `json:"server_seed"`
	Commitment string `json:"commitment"`
	ClientSeed string `json:"client_seed"`
	Nonce      int64  `json:"nonce"`
	Slots      int    `json:"slots"`
	Slot       int    `json:"slot"`
}

// The vectors were computed by an independent implementation of the specification in the package comment.
func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v []vector
	if err := json.Unmarshal(raw, &v); err != nil || len(v) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	return v
}

func TestSlot_MatchesTheIndependentVectors(t *testing.T) {
	for _, v := range loadVectors(t) {
		seed, _ := hex.DecodeString(v.ServerSeed)
		if c := fair.Commitment(seed); c != v.Commitment {
			t.Errorf("commitment %s, want %s", c, v.Commitment)
		}
		got, err := fair.Slot(seed, v.ClientSeed, v.Nonce, v.Slots)
		if err != nil || got != v.Slot {
			t.Errorf("%+v: slot %d err %v", v, got, err)
		}
		if got, err := fair.Verify(v.ServerSeed, v.Commitment, v.ClientSeed, v.Nonce, v.Slots); err != nil || got != v.Slot {
			t.Errorf("verify %+v: slot %d err %v", v, got, err)
		}
	}
}

func TestVerify_RefusesASeedThatWasNotCommitted(t *testing.T) {
	v := loadVectors(t)
	if _, err := fair.Verify(v[1].ServerSeed, v[0].Commitment, "x", 0, 12); !errors.Is(err, fair.ErrMismatch) {
		t.Fatalf("err = %v", err)
	}
	if _, err := fair.Verify("zz", v[0].Commitment, "x", 0, 12); !errors.Is(err, fair.ErrBadSeed) {
		t.Fatalf("err = %v", err)
	}
}

func TestSlot_RefusesBadInput(t *testing.T) {
	seed := make([]byte, fair.SeedBytes)
	for name, tc := range map[string]struct {
		seed  []byte
		cs    string
		nonce int64
		slots int
		want  error
	}{
		"short seed":      {seed[:31], "a", 0, 12, fair.ErrBadSeed},
		"empty client":    {seed, "", 0, 12, fair.ErrBadClientSeed},
		"colon in client": {seed, "a:1", 0, 12, fair.ErrBadClientSeed},
		"long client":     {seed, string(make([]byte, 65)), 0, 12, fair.ErrBadClientSeed},
		"one slot":        {seed, "a", 0, 1, fair.ErrBadSlots},
		"too many slots":  {seed, "a", 0, fair.MaxSlots + 1, fair.ErrBadSlots},
		"negative nonce":  {seed, "a", -1, 12, fair.ErrBadNonce},
	} {
		if _, err := fair.Slot(tc.seed, tc.cs, tc.nonce, tc.slots); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNewSeeds_AreFreshAndValid(t *testing.T) {
	a, _ := fair.NewServerSeed()
	b, _ := fair.NewServerSeed()
	if len(a) != fair.SeedBytes || hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Fatal("server seeds are not fresh")
	}
	cs, _ := fair.NewClientSeed()
	if !fair.ValidClientSeed(cs) {
		t.Fatalf("generated client seed %q is not valid", cs)
	}
}

// chiSquare spins one chain `spins` times over `slots` and returns the chi-square statistic of the slot counts.
func chiSquare(t *testing.T, spins, slots int) float64 {
	t.Helper()
	seed, _ := fair.NewServerSeed()
	counts := make([]int, slots)
	for nonce := range spins {
		s, err := fair.Slot(seed, "dist", int64(nonce), slots)
		if err != nil {
			t.Fatal(err)
		}
		counts[s]++
	}
	want := float64(spins) / float64(slots)
	var x2 float64
	for _, c := range counts {
		d := float64(c) - want
		x2 += d * d / want
	}
	return x2
}

// With 11 degrees of freedom, chi-square exceeds 31.26 with probability 0.001: a fair generator fails 1 run in 1000.
func TestSlot_IsUniformOver100kSpins(t *testing.T) {
	if x2 := chiSquare(t, 100_000, 12); x2 > 31.26 || math.IsNaN(x2) {
		t.Fatalf("chi-square %.2f over 12 slots: not uniform", x2)
	}
}
