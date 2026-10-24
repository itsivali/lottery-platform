// Command verifier checks provably fair spins without trusting the server's code. It re-implements the specification
// in internal/game/fair (on purpose it does not import it): after a seed chain is rotated, give it the revealed server
// seed, the commitment shown before the spins, the client seed and the nonces, and it prints each spin's slot.
//
//	go run ./tools/verifier -server-seed HEX -commitment HEX -client-seed SEED -nonce 0-3 -slots 12 -lucky 1
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "verifier:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("verifier", flag.ContinueOnError)
	seedHex := fs.String("server-seed", "", "revealed server seed (64 hex characters)")
	commitment := fs.String("commitment", "", "commitment shown before the spins (64 hex characters)")
	clientSeed := fs.String("client-seed", "", "client seed of the chain")
	nonces := fs.String("nonce", "0", "a nonce, or a range like 0-3")
	slots := fs.Int("slots", 12, "slot count of the odds version recorded on the spin")
	lucky := fs.Int("lucky", 1, "lucky slots of that odds version (slots below this number are lucky)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seed, err := hex.DecodeString(*seedHex)
	if err != nil || len(seed) != 32 {
		return errors.New("server seed must be 64 hex characters")
	}
	sum := sha256.Sum256(seed)
	if hex.EncodeToString(sum[:]) != strings.ToLower(*commitment) {
		return errors.New("MISMATCH: this server seed is not the one committed to")
	}
	if *slots < 2 || *slots > 1<<16 || *lucky < 1 || *lucky >= *slots {
		return errors.New("slots must be 2 to 65536 and lucky between 1 and slots-1")
	}
	from, to, err := nonceRange(*nonces)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out, "commitment ok"); err != nil {
		return err
	}
	for n := from; n <= to; n++ {
		s := slot(seed, *clientSeed, n, uint64(*slots))
		if _, err := fmt.Fprintf(out, "nonce %d: slot %d lucky %t\n", n, s, s < uint64(*lucky)); err != nil {
			return err
		}
	}
	return nil
}

func nonceRange(s string) (int64, int64, error) {
	a, b, isRange := strings.Cut(s, "-")
	from, err := strconv.ParseInt(a, 10, 64)
	if err != nil || from < 0 {
		return 0, 0, errors.New("bad nonce")
	}
	if !isRange {
		return from, from, nil
	}
	to, err := strconv.ParseInt(b, 10, 64)
	if err != nil || to < from || to-from > 100_000 {
		return 0, 0, errors.New("bad nonce range (at most 100000 spins)")
	}
	return from, to, nil
}

// slot: HMAC-SHA256(seed, client:nonce:round), 32-bit big-endian words, reject words >= 2^32 - (2^32 mod n).
func slot(seed []byte, client string, nonce int64, n uint64) uint64 {
	limit := uint64(1)<<32 - (uint64(1)<<32)%n
	for round := 0; ; round++ {
		m := hmac.New(sha256.New, seed)
		_, _ = fmt.Fprintf(m, "%s:%d:%d", client, nonce, round) // a hash never fails to write
		mac := m.Sum(nil)
		for i := 0; i < 32; i += 4 {
			if w := uint64(binary.BigEndian.Uint32(mac[i : i+4])); w < limit {
				return w % n
			}
		}
	}
}
