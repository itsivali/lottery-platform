package main

import (
	"strings"
	"testing"
)

func TestRenderDOT_GroupsBySchemaMarksKeysAndDrawsForeignKeys(t *testing.T) {
	tables := []table{
		{Schema: "identity", Name: "users", Columns: []column{{Name: "id", Type: "uuid", PK: true}, {Name: "msisdn", Type: "text"}}},
		{Schema: "wallet", Name: "wallets", Columns: []column{{Name: "user_id", Type: "uuid", PK: true, FK: true}, {Name: "balance_kes", Type: "bigint"}}},
	}
	fks := []foreignKey{{FromSchema: "wallet", FromTable: "wallets", FromColumn: "user_id", ToSchema: "identity", ToTable: "users"}}
	got := renderDOT(tables, fks)
	for _, want := range []string{
		`subgraph "cluster_identity"`, `subgraph "cluster_wallet"`,
		`"identity.users"`, `"wallet.wallets"`,
		`id : uuid (PK)`, `user_id : uuid (PK, FK)`, `balance_kes : bigint`,
		`"wallet.wallets" -> "identity.users"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DOT output missing %q\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "digraph") || !strings.HasSuffix(strings.TrimSpace(got), "}") {
		t.Errorf("not a complete digraph:\n%s", got)
	}
}

func TestRenderDOT_EscapesQuotes(t *testing.T) {
	got := renderDOT([]table{{Schema: "s", Name: `we"ird`, Columns: []column{{Name: "c", Type: "text"}}}}, nil)
	if strings.Contains(got, `we"ird"`) && !strings.Contains(got, `we\"ird`) {
		t.Errorf("unescaped quote:\n%s", got)
	}
}
