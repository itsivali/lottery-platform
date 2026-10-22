//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

func TestRun_PaymentsReconcilePrintsACleanReportOnAFreshDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", testpg.Migrated(t))
	for _, k := range []string{"AUTH_KEY", "TOKEN_SEED", "DATA_KEY", "DATA_KEYS", "IDENTITY_KEY", "VALKEY_ADDR", "DARAJA_CONSUMER_KEY"} {
		t.Setenv(k, "")
	}
	var out bytes.Buffer
	if err := run(context.Background(), command{name: "payments reconcile"}, &out); err != nil {
		t.Fatalf("reconcile: %v\n%s", err, out.String())
	}
	var r struct {
		OK         bool                `json:"ok"`
		Mismatches map[string][]string `json:"mismatches"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.OK || len(r.Mismatches) != 0 {
		t.Fatalf("report = %s (%v)", out.String(), err)
	}
	if err := run(context.Background(), command{name: "payments reverse", receipt: "NOPE", reason: "x", operator: "operator:t"}, &out); err == nil {
		t.Fatal("reversing an unknown receipt succeeded")
	}
}
