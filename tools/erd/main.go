// Command erd prints a Graphviz DOT entity-relationship diagram of the application schemas.
// Usage: erd "$DATABASE_URL" | dot -Tsvg > docs/erd.svg   (see `make erd`).
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type column struct {
	Name, Type string
	PK, FK     bool
}

type table struct {
	Schema, Name string
	Columns      []column
}

type foreignKey struct{ FromSchema, FromTable, FromColumn, ToSchema, ToTable string }

var appSchemas = []string{"identity", "kyc", "rg", "wallet", "payments", "ledger", "tickets", "game", "draws", "payouts", "audit", "platform"}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: erd <database-url>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "erd:", err)
		os.Exit(1)
	}
}

func run(dsn string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	pks := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT n.nspname, c.relname, a.attname FROM pg_index i
		JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
		WHERE i.indisprimary AND n.nspname = ANY($1)`, appSchemas)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s, t, c string
		if err := rows.Scan(&s, &t, &c); err != nil {
			return err
		}
		pks[s+"."+t+"."+c] = true
	}
	rows.Close()

	var fks []foreignKey
	fkCols := map[string]bool{}
	rows, err = conn.Query(ctx, `SELECT n.nspname, c.relname, a.attname, fn.nspname, fc.relname
		FROM pg_constraint k JOIN pg_class c ON c.oid = k.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_class fc ON fc.oid = k.confrelid JOIN pg_namespace fn ON fn.oid = fc.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = k.conkey[1]
		WHERE k.contype = 'f' AND n.nspname = ANY($1)`, appSchemas)
	if err != nil {
		return err
	}
	for rows.Next() {
		var f foreignKey
		if err := rows.Scan(&f.FromSchema, &f.FromTable, &f.FromColumn, &f.ToSchema, &f.ToTable); err != nil {
			return err
		}
		fks = append(fks, f)
		fkCols[f.FromSchema+"."+f.FromTable+"."+f.FromColumn] = true
	}
	rows.Close()

	byTable := map[string]*table{}
	var order []string
	rows, err = conn.Query(ctx, `SELECT table_schema, table_name, column_name, data_type FROM information_schema.columns
		WHERE table_schema = ANY($1) ORDER BY table_schema, table_name, ordinal_position`, appSchemas)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s, t, c, typ string
		if err := rows.Scan(&s, &t, &c, &typ); err != nil {
			return err
		}
		key := s + "." + t
		if byTable[key] == nil {
			byTable[key] = &table{Schema: s, Name: t}
			order = append(order, key)
		}
		byTable[key].Columns = append(byTable[key].Columns, column{Name: c, Type: typ, PK: pks[key+"."+c], FK: fkCols[key+"."+c]})
	}
	rows.Close()

	tables := make([]table, 0, len(order))
	for _, k := range order {
		tables = append(tables, *byTable[k])
	}
	fmt.Print(renderDOT(tables, fks))
	return rows.Err()
}

func esc(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, `<`, `\<`, `>`, `\>`, `{`, `\{`, `}`, `\}`, `|`, `\|`).Replace(s)
}

func renderDOT(tables []table, fks []foreignKey) string {
	var b strings.Builder
	b.WriteString("digraph erd {\n  rankdir=LR; compound=true; fontname=\"Helvetica\";\n  node [shape=record, fontname=\"Helvetica\", fontsize=10];\n  edge [color=\"#888888\", arrowsize=0.6];\n")
	bySchema := map[string][]table{}
	var schemas []string
	for _, t := range tables {
		if bySchema[t.Schema] == nil {
			schemas = append(schemas, t.Schema)
		}
		bySchema[t.Schema] = append(bySchema[t.Schema], t)
	}
	sort.Strings(schemas)
	for _, s := range schemas {
		fmt.Fprintf(&b, "  subgraph \"cluster_%s\" {\n    label=\"%s\"; style=rounded; color=\"#aaaaaa\";\n", esc(s), esc(s))
		for _, t := range bySchema[s] {
			var cols []string
			for _, c := range t.Columns {
				line := c.Name + " : " + c.Type
				switch {
				case c.PK && c.FK:
					line += " (PK, FK)"
				case c.PK:
					line += " (PK)"
				case c.FK:
					line += " (FK)"
				}
				cols = append(cols, esc(line)+`\l`)
			}
			fmt.Fprintf(&b, "    \"%s.%s\" [label=\"{%s|%s}\"];\n", esc(t.Schema), esc(t.Name), esc(t.Name), strings.Join(cols, ""))
		}
		b.WriteString("  }\n")
	}
	for _, f := range fks {
		fmt.Fprintf(&b, "  \"%s.%s\" -> \"%s.%s\";\n", esc(f.FromSchema), esc(f.FromTable), esc(f.ToSchema), esc(f.ToTable))
	}
	b.WriteString("}\n")
	return b.String()
}
