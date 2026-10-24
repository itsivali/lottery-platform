package drand_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/itsivali/lottery-platform/internal/draws/drand"
)

// Real quicknet beacons recorded from api.drand.sh.
func beacon(t *testing.T, round int) drand.Beacon {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("testdata/round-%d.json", round))
	if err != nil {
		t.Fatal(err)
	}
	var b drand.Beacon
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerify_AcceptsRealBeaconsAndRefusesAnyChange(t *testing.T) {
	c := drand.Quicknet
	for _, r := range []int{1000000, 1000001, 12345678} {
		if err := c.Verify(beacon(t, r)); err != nil {
			t.Errorf("round %d: %v", r, err)
		}
	}
	good := beacon(t, 1000000)
	other := beacon(t, 1000001)
	for name, b := range map[string]drand.Beacon{
		"another round's signature": {Round: good.Round, Randomness: other.Randomness, Signature: other.Signature},
		"round relabelled":          {Round: good.Round + 1, Randomness: good.Randomness, Signature: good.Signature},
		"randomness swapped":        {Round: good.Round, Randomness: other.Randomness, Signature: good.Signature},
		"not hex":                   {Round: good.Round, Randomness: good.Randomness, Signature: "zz"},
		"round zero":                {Round: 0, Randomness: good.Randomness, Signature: good.Signature},
	} {
		if err := c.Verify(b); !errors.Is(err, drand.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	wrongKey := c
	wrongKey.PublicKey = strings.Repeat("00", 96)
	if err := wrongKey.Verify(good); err == nil {
		t.Error("verified under another key")
	}
}

func TestRounds_MapToTimesAndBack(t *testing.T) {
	c := drand.Quicknet
	if c.RoundAt(c.Genesis) != 1 || c.RoundAt(c.Genesis.Add(-time.Hour)) != 1 {
		t.Fatal("genesis is round 1")
	}
	if c.RoundAt(c.Genesis.Add(3*time.Second)) != 2 || c.RoundAt(c.Genesis.Add(3*time.Second+time.Millisecond)) != 3 {
		t.Fatal("round boundaries")
	}
	at := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)
	r := c.RoundAt(at)
	if got := c.TimeOf(r); got.Before(at) || got.Sub(at) >= c.Period {
		t.Fatalf("round %d at %s for %s", r, got, at)
	}
}

func relay(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

func TestFetch_VerifiesWhateverRelayAnswersAndFallsBack(t *testing.T) {
	good := beacon(t, 1000000)
	other := beacon(t, 1000001)
	var asked atomic.Int32
	lying := relay(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_ = json.NewEncoder(w).Encode(drand.Beacon{Round: good.Round, Randomness: other.Randomness, Signature: other.Signature})
	})
	down := relay(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	honest := relay(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, drand.Quicknet.Hash+"/public/1000000") {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(good)
	})
	c := drand.New(drand.Quicknet, []string{lying.URL, down.URL, honest.URL}, http.DefaultClient)
	now := drand.Quicknet.TimeOf(good.Round).Add(time.Minute)
	b, err := c.Fetch(context.Background(), now, good.Round)
	if err != nil || b.Randomness != good.Randomness || asked.Load() != 1 {
		t.Fatalf("fetch: %+v %v", b, err)
	}
	// only liars and outages: no beacon
	c = drand.New(drand.Quicknet, []string{lying.URL, down.URL}, http.DefaultClient)
	if _, err := c.Fetch(context.Background(), now, good.Round); err == nil {
		t.Fatal("accepted a beacon that does not verify")
	}
}

func TestFetch_NotYet(t *testing.T) {
	asked := false
	early := relay(t, func(w http.ResponseWriter, _ *http.Request) { asked = true; w.WriteHeader(http.StatusTooEarly) })
	c := drand.New(drand.Quicknet, []string{early.URL}, http.DefaultClient)
	round := uint64(1000000)
	if _, err := c.Fetch(context.Background(), drand.Quicknet.TimeOf(round).Add(-time.Second), round); !errors.Is(err, drand.ErrNotYet) || asked {
		t.Fatalf("before its time: %v (asked %v)", err, asked)
	}
	if _, err := c.Fetch(context.Background(), drand.Quicknet.TimeOf(round), round); !errors.Is(err, drand.ErrNotYet) || !asked {
		t.Fatalf("relay not ready: %v", err)
	}
	unreachable := drand.New(drand.Quicknet, []string{"http://127.0.0.1:1"}, &http.Client{Timeout: time.Second})
	if _, err := unreachable.Fetch(context.Background(), drand.Quicknet.TimeOf(round), round); !errors.Is(err, drand.ErrUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
	if drand.New(drand.Quicknet, nil, nil).Chain().Hash != drand.Quicknet.Hash {
		t.Fatal("chain")
	}
}
