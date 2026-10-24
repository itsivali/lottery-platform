package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type vector struct {
	ServerSeed string `json:"server_seed"`
	Commitment string `json:"commitment"`
	ClientSeed string `json:"client_seed"`
	Nonce      int64  `json:"nonce"`
	Slots      int    `json:"slots"`
	Slot       int    `json:"slot"`
}

// The same independent vectors the server's package is tested against.
func TestRun_ReproducesEveryVector(t *testing.T) {
	raw, err := os.ReadFile("../../internal/game/fair/testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vs []vector
	if err := json.Unmarshal(raw, &vs); err != nil || len(vs) == 0 {
		t.Fatalf("vectors: %v", err)
	}
	for _, v := range vs {
		var out bytes.Buffer
		err := run([]string{"-server-seed", v.ServerSeed, "-commitment", v.Commitment, "-client-seed", v.ClientSeed,
			"-nonce", fmt.Sprint(v.Nonce), "-slots", fmt.Sprint(v.Slots), "-lucky", "1"}, &out)
		want := fmt.Sprintf("nonce %d: slot %d lucky %t", v.Nonce, v.Slot, v.Slot < 1)
		if err != nil || !strings.Contains(out.String(), want) {
			t.Errorf("%+v: %v\n%s", v, err, out.String())
		}
	}
}

func TestRun_RefusesASeedThatWasNotCommittedAndBadInput(t *testing.T) {
	seed := strings.Repeat("ab", 32)
	for name, args := range map[string][]string{
		"wrong commitment": {"-server-seed", seed, "-commitment", strings.Repeat("0", 64)},
		"short seed":       {"-server-seed", "abcd", "-commitment", strings.Repeat("0", 64)},
	} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var out bytes.Buffer
	if err := run([]string{"-server-seed", seed, "-commitment", "x"}, &out); err == nil || !strings.Contains(err.Error(), "MISMATCH") {
		t.Errorf("mismatch not reported: %v", err)
	}
}

func TestNonceRange(t *testing.T) {
	if a, b, err := nonceRange("3-5"); err != nil || a != 3 || b != 5 {
		t.Fatalf("%d %d %v", a, b, err)
	}
	for _, bad := range []string{"-1", "x", "5-3", "0-200000"} {
		if _, _, err := nonceRange(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
