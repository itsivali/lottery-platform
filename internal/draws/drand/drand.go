// Package drand fetches and verifies randomness beacons from the drand network (League of Entropy). A draw fixes a
// future round before its entry list freezes; when that round is published anyone can fetch it, and this package
// accepts it only if its BLS signature verifies against the chain's public key, which is pinned in configuration
// (never taken from the network), and its randomness is the SHA-256 of that signature.
//
// The default chain is quicknet (scheme bls-unchained-g1-rfc9380, a round every 3 seconds): the signature of round r
// is a BLS signature on G1 of SHA-256(r as 8 big-endian bytes) under a public key on G2.
package drand

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	bls12381 "github.com/drand/kyber-bls12381"
	"github.com/drand/kyber/sign/bdn"
)

// Chain is what is pinned about a drand chain.
type Chain struct {
	Hash      string // hex chain hash, part of the URL
	PublicKey string // hex G2 public key
	Genesis   time.Time
	Period    time.Duration
}

// Quicknet is the League of Entropy's quicknet chain (https://api.drand.sh/<hash>/info).
var Quicknet = Chain{
	Hash:      "52db9ba70e0cc0f6eaf7803dd07447a1f5477735fd3f661792ba94600c84e971",
	PublicKey: "83cf0f2896adee7eb8b5f01fcad3912212c437e0073e911fb90022d3e760183c8c4b450b6a0a6c3ac6a5776a2d1064510d1fec758c921cc22b0e17e63aaf4bcb5ed66304de9cf809bd274ca73bab4af5a6e9c76a4bc09e76eae8991ef5ece45a",
	Genesis:   time.Unix(1692803367, 0).UTC(),
	Period:    3 * time.Second,
}

// Errors.
var (
	ErrNotYet      = errors.New("drand: round not published yet")
	ErrUnavailable = errors.New("drand: unavailable")
	ErrInvalid     = errors.New("drand: beacon does not verify")
)

// Beacon is a verified round.
type Beacon struct {
	Round      uint64 `json:"round"`
	Randomness string `json:"randomness"` // hex, SHA-256 of the signature
	Signature  string `json:"signature"`  // hex
}

// RoundAt is the first round published at or after t.
func (c Chain) RoundAt(t time.Time) uint64 {
	if !t.After(c.Genesis) {
		return 1
	}
	elapsed := t.Sub(c.Genesis)
	n := uint64(elapsed / c.Period) //nolint:gosec // t is after genesis, so elapsed is positive
	if elapsed%c.Period != 0 {
		n++
	}
	return n + 1
}

// TimeOf is when a round is published.
func (c Chain) TimeOf(round uint64) time.Time {
	return c.Genesis.Add(time.Duration(round-1) * c.Period) //nolint:gosec // rounds stay far below 2^63 periods
}

// Verify checks a beacon against the chain: the signature on the round, and the randomness derived from it.
func (c Chain) Verify(b Beacon) error {
	pub := bls12381.NewBLS12381Suite().G2().Point()
	key, err := hex.DecodeString(c.PublicKey)
	if err != nil {
		return fmt.Errorf("drand: public key: %w", err)
	}
	if err := pub.UnmarshalBinary(key); err != nil {
		return fmt.Errorf("drand: public key: %w", err)
	}
	sig, err := hex.DecodeString(b.Signature)
	if err != nil || b.Round == 0 {
		return ErrInvalid
	}
	var r [8]byte
	binary.BigEndian.PutUint64(r[:], b.Round)
	msg := sha256.Sum256(r[:])
	// BDN's single-signature verification is plain BLS (BDN only changes how signatures are aggregated)
	scheme := bdn.NewSchemeOnG1(bls12381.NewBLS12381Suite())
	if err := scheme.Verify(pub, msg[:], sig); err != nil {
		return ErrInvalid
	}
	want := sha256.Sum256(sig)
	if !strings.EqualFold(b.Randomness, hex.EncodeToString(want[:])) {
		return ErrInvalid
	}
	return nil
}

// Client fetches beacons from drand HTTP relays, trying each in turn.
type Client struct {
	chain  Chain
	relays []string
	hc     *http.Client
}

// DefaultRelays are the League of Entropy's public HTTP relays.
var DefaultRelays = []string{"https://api.drand.sh", "https://api2.drand.sh", "https://api3.drand.sh", "https://drand.cloudflare.com"}

// New returns a client for the chain through the relays.
func New(chain Chain, relays []string, hc *http.Client) *Client {
	return &Client{chain: chain, relays: relays, hc: hc}
}

// Chain returns the pinned chain.
func (c *Client) Chain() Chain { return c.chain }

// Fetch returns the verified beacon of a round. Before the round's time it answers ErrNotYet without asking.
// A beacon that does not verify is never returned, whichever relay served it.
func (c *Client) Fetch(ctx context.Context, now time.Time, round uint64) (Beacon, error) {
	if now.Before(c.chain.TimeOf(round)) {
		return Beacon{}, ErrNotYet
	}
	last := ErrUnavailable
	for _, relay := range c.relays {
		b, err := c.fetch(ctx, relay, round)
		switch {
		case err == nil:
			if verr := c.chain.Verify(b); verr != nil {
				last = fmt.Errorf("%w (from %s)", verr, relay)
				continue
			}
			return b, nil
		case errors.Is(err, ErrNotYet):
			last = err
		default:
			last = fmt.Errorf("%w: %s: %w", ErrUnavailable, relay, err)
		}
	}
	return Beacon{}, last
}

func (c *Client) fetch(ctx context.Context, relay string, round uint64) (Beacon, error) {
	url := strings.TrimRight(relay, "/") + "/" + c.chain.Hash + "/public/" + strconv.FormatUint(round, 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return Beacon{}, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return Beacon{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return Beacon{}, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusTooEarly:
		return Beacon{}, ErrNotYet
	case resp.StatusCode != http.StatusOK:
		return Beacon{}, fmt.Errorf("status %d", resp.StatusCode)
	}
	var b Beacon
	if err := json.Unmarshal(raw, &b); err != nil || b.Round != round {
		return Beacon{}, fmt.Errorf("%w: unexpected answer for round %d", ErrInvalid, round)
	}
	return b, nil
}
