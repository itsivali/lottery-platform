//go:build integration

package db_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/itsivali/lottery-platform/internal/platform/clock"
	"github.com/itsivali/lottery-platform/internal/platform/testpg"
)

const (
	codeCheckViolation   = "23514"
	codeUniqueViolation  = "23505"
	codeRestrict         = "23001"
	codeInsufficientPriv = "42501"
)

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func connect(t *testing.T) (context.Context, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, testpg.Migrated(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	return ctx, conn
}

func mustExec(t *testing.T, ctx context.Context, q execer, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func wantState(t *testing.T, err error, want string) {
	t.Helper()
	if got := sqlState(err); got != want {
		t.Fatalf("SQLSTATE = %q (err %v), want %q", got, err, want)
	}
}

func newUser(t *testing.T, ctx context.Context, c *pgx.Conn, msisdn string) string {
	t.Helper()
	var id string
	if err := c.QueryRow(ctx,
		`INSERT INTO identity.users (msisdn) VALUES ($1) RETURNING id::text`, msisdn).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func accountID(t *testing.T, ctx context.Context, c *pgx.Conn, code string) string {
	t.Helper()
	var id string
	if err := c.QueryRow(ctx, `SELECT id::text FROM ledger.ledger_accounts WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatalf("account %s: %v", code, err)
	}
	return id
}

func newJournal(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, ref string) string {
	t.Helper()
	var id string
	if err := q.QueryRow(ctx,
		`INSERT INTO ledger.ledger_journals (kind, reference_type, reference_id) VALUES ('test','test',$1::uuid) RETURNING id::text`,
		ref).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

const randomUUID = "00000000-0000-7000-8000-000000000001"

func TestLedger_UnbalancedJournalIsRejectedAtCommit(t *testing.T) {
	ctx, c := connect(t)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j := newJournal(t, ctx, tx, randomUUID)
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',100)`,
		j, accountID(t, ctx, c, "MPESA_CLEARING"))
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'C',99)`,
		j, accountID(t, ctx, c, "WALLET_LIABILITY"))
	wantState(t, tx.Commit(ctx), codeCheckViolation)
}

func TestLedger_BalancedJournalCommits(t *testing.T) {
	ctx, c := connect(t)
	tx, _ := c.Begin(ctx)
	j := newJournal(t, ctx, tx, randomUUID)
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',100)`,
		j, accountID(t, ctx, c, "MPESA_CLEARING"))
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'C',100)`,
		j, accountID(t, ctx, c, "WALLET_LIABILITY"))
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("balanced journal rejected: %v", err)
	}
}

func TestLedger_JournalWithoutEntriesIsRejectedAtCommit(t *testing.T) {
	ctx, c := connect(t)
	tx, _ := c.Begin(ctx)
	newJournal(t, ctx, tx, randomUUID)
	wantState(t, tx.Commit(ctx), codeCheckViolation)
}

func postBalanced(t *testing.T, ctx context.Context, c *pgx.Conn, ref string, kes int64) error {
	t.Helper()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	j := newJournal(t, ctx, tx, ref)
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',$3)`, j, accountID(t, ctx, c, "MPESA_CLEARING"), kes)
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'C',$3)`, j, accountID(t, ctx, c, "WALLET_LIABILITY"), kes)
	return tx.Commit(ctx)
}

func TestLedger_PostingTheSameEventTwiceIsRejected(t *testing.T) {
	ctx, c := connect(t)
	if err := postBalanced(t, ctx, c, randomUUID, 10); err != nil {
		t.Fatal(err)
	}
	tx, _ := c.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_journals (kind, reference_type, reference_id) VALUES ('test','test',$1::uuid)`, randomUUID)
	wantState(t, err, codeUniqueViolation)
}

func TestLedger_EntryAmountMustBePositive(t *testing.T) {
	ctx, c := connect(t)
	tx, _ := c.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	j := newJournal(t, ctx, tx, randomUUID)
	_, err := tx.Exec(ctx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',0)`,
		j, accountID(t, ctx, c, "MPESA_CLEARING"))
	wantState(t, err, codeCheckViolation)
}

func TestAppendOnly_AppRolesCannotUpdateOrDeleteHistory(t *testing.T) {
	ctx, c := connect(t)
	uid := newUser(t, ctx, c, "254712345678")
	mustExec(t, ctx, c, `INSERT INTO audit.audit_log (actor_type, action, entity_type) VALUES ('system','x','y')`)
	tx, _ := c.Begin(ctx)
	j := newJournal(t, ctx, tx, randomUUID)
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',5)`, j, accountID(t, ctx, c, "MPESA_CLEARING"))
	mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'C',5)`, j, accountID(t, ctx, c, "WALLET_LIABILITY"))
	mustExec(t, ctx, tx, `INSERT INTO wallet.wallets (user_id, balance_kes) VALUES ($1,5)`, uid)
	mustExec(t, ctx, tx, `INSERT INTO wallet.wallet_transactions (user_id, journal_id, kind, amount_kes) VALUES ($1,$2,'deposit',5)`, uid, j)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	stmts := []string{
		`UPDATE ledger.ledger_entries SET amount_kes = amount_kes + 1`,
		`DELETE FROM ledger.ledger_entries`,
		`UPDATE ledger.ledger_journals SET kind = 'x'`,
		`DELETE FROM ledger.ledger_journals`,
		`UPDATE wallet.wallet_transactions SET amount_kes = 9`,
		`DELETE FROM wallet.wallet_transactions`,
		`UPDATE audit.audit_log SET action = 'z'`,
		`DELETE FROM audit.audit_log`,
		`TRUNCATE audit.audit_log`,
	}
	for _, role := range []string{"app_api", "app_worker"} {
		for _, s := range stmts {
			mustExec(t, ctx, c, "SET ROLE "+role)
			_, err := c.Exec(ctx, s)
			mustExec(t, ctx, c, "RESET ROLE")
			if sqlState(err) != codeInsufficientPriv {
				t.Errorf("%s: %s -> %v, want permission denied", role, s, err)
			}
		}
	}
}

func TestAppendOnly_OwnerAndSuperuserAreBlockedByTriggers(t *testing.T) {
	ctx, c := connect(t) // connected as the bootstrap superuser: grants do not apply, triggers still must
	mustExec(t, ctx, c, `INSERT INTO audit.audit_log (actor_type, action, entity_type) VALUES ('system','x','y')`)
	for _, s := range []string{
		`UPDATE audit.audit_log SET action = 'z'`,
		`DELETE FROM audit.audit_log`,
		`TRUNCATE audit.audit_log`,
	} {
		_, err := c.Exec(ctx, s)
		if sqlState(err) != codeRestrict {
			t.Errorf("%s -> %v, want restrict_violation", s, err)
		}
	}
}

func TestWallet_NegativeBalanceIsRejected(t *testing.T) {
	ctx, c := connect(t)
	uid := newUser(t, ctx, c, "254712345678")
	_, err := c.Exec(ctx, `INSERT INTO wallet.wallets (user_id, balance_kes) VALUES ($1,-1)`, uid)
	wantState(t, err, codeCheckViolation)
}

func TestWallet_BalanceMustEqualSumOfTransactionsAtCommit(t *testing.T) {
	ctx, c := connect(t)
	uid := newUser(t, ctx, c, "254712345678")
	post := func(balance, amount int64) error {
		tx, _ := c.Begin(ctx)
		defer func() { _ = tx.Rollback(ctx) }()
		j := newJournal(t, ctx, tx, randomUUID)
		mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'D',$3)`, j, accountID(t, ctx, c, "MPESA_CLEARING"), amount)
		mustExec(t, ctx, tx, `INSERT INTO ledger.ledger_entries (journal_id, account_id, direction, amount_kes) VALUES ($1,$2,'C',$3)`, j, accountID(t, ctx, c, "WALLET_LIABILITY"), amount)
		mustExec(t, ctx, tx, `INSERT INTO wallet.wallets (user_id, balance_kes) VALUES ($1,$2) ON CONFLICT (user_id) DO UPDATE SET balance_kes = EXCLUDED.balance_kes`, uid, balance)
		mustExec(t, ctx, tx, `INSERT INTO wallet.wallet_transactions (user_id, journal_id, kind, amount_kes) VALUES ($1,$2,'deposit',$3)`, uid, j, amount)
		return tx.Commit(ctx)
	}
	wantState(t, post(150, 100), codeCheckViolation) // cached balance drifted from the transactions
	if err := post(100, 100); err != nil {
		t.Fatalf("consistent wallet rejected: %v", err)
	}
}

func TestTickets_PlayCapIsEnforcedByTheDatabase(t *testing.T) {
	ctx, c := connect(t)
	uid := newUser(t, ctx, c, "254712345678")
	ins := func(seq int, price int64) error {
		_, err := c.Exec(ctx, `INSERT INTO tickets.tickets (user_id, play_date, seq, price_kes, idempotency_key) VALUES ($1,'2026-10-18',$2,$3,gen_random_uuid()::text)`, uid, seq, price)
		return err
	}
	for seq := 1; seq <= 4; seq++ {
		if err := ins(seq, 100); err != nil {
			t.Fatalf("seq %d: %v", seq, err)
		}
	}
	wantState(t, ins(5, 100), codeCheckViolation)
	wantState(t, ins(3, 100), codeUniqueViolation)
	wantState(t, ins(0, 100), codeCheckViolation)
	_, err := c.Exec(ctx, `INSERT INTO tickets.tickets (user_id, play_date, seq, price_kes, idempotency_key) VALUES ($1,'2026-10-19',1,0,'k')`, uid)
	wantState(t, err, codeCheckViolation)
}

func TestDraws_SecondSeedForTheSameDrawIsRejected(t *testing.T) {
	ctx, c := connect(t)
	var id string
	if err := c.QueryRow(ctx, `INSERT INTO draws.draws (draw_type, period_start, period_end) VALUES ('daily','2026-10-18','2026-10-18') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, c, `UPDATE draws.draws SET seed_enc = '\x01', seed_commit = repeat('a',64) WHERE id = $1`, id)
	_, err := c.Exec(ctx, `UPDATE draws.draws SET seed_enc = '\x02', seed_commit = repeat('b',64) WHERE id = $1`, id)
	if err == nil {
		t.Fatal("second seed accepted")
	}
}

func TestDraws_OneLiveDrawPerPeriodButVoidedOnesMayBeRerun(t *testing.T) {
	ctx, c := connect(t)
	ins := func() error {
		_, err := c.Exec(ctx, `INSERT INTO draws.draws (draw_type, period_start, period_end) VALUES ('daily','2026-10-18','2026-10-18')`)
		return err
	}
	if err := ins(); err != nil {
		t.Fatal(err)
	}
	wantState(t, ins(), codeUniqueViolation)
	mustExec(t, ctx, c, `UPDATE draws.draws SET status = 'voided', voided_reason = 'late sign-off'`)
	if err := ins(); err != nil {
		t.Fatalf("rerun after void: %v", err)
	}
}

func TestPayments_ReceiptAndCheckoutRequestAreUnique(t *testing.T) {
	ctx, c := connect(t)
	uid := newUser(t, ctx, c, "254712345678")
	ins := func(checkout, receipt string) error {
		_, err := c.Exec(ctx, `INSERT INTO payments.payments (user_id, channel, amount_requested_kes, checkout_request_id, mpesa_receipt, idempotency_key)
			VALUES ($1,'stk',100,$2,$3,gen_random_uuid()::text)`, uid, checkout, receipt)
		return err
	}
	if err := ins("ws_CO_1", "QWE123"); err != nil {
		t.Fatal(err)
	}
	wantState(t, ins("ws_CO_2", "QWE123"), codeUniqueViolation)
	wantState(t, ins("ws_CO_1", "RTY456"), codeUniqueViolation)
}

func TestIdempotencyKeys_ArePerPrincipal(t *testing.T) {
	ctx, c := connect(t)
	ins := func(principal, key string) error {
		_, err := c.Exec(ctx, `INSERT INTO platform.idempotency_keys (principal, key, request_hash, expires_at) VALUES ($1,$2,'\x00', now() + interval '1 day')`, principal, key)
		return err
	}
	if err := ins("user:a", "k1"); err != nil {
		t.Fatal(err)
	}
	if err := ins("user:b", "k1"); err != nil {
		t.Fatalf("same key for another principal must be allowed: %v", err)
	}
	wantState(t, ins("user:a", "k1"), codeUniqueViolation)
}

func TestKraOutbox_WorkerMayOnlyMarkExported(t *testing.T) {
	ctx, c := connect(t)
	mustExec(t, ctx, c, `INSERT INTO audit.kra_outbox (event_type, payload) VALUES ('stake','{}')`)
	asRole := func(role, sql string) error {
		mustExec(t, ctx, c, "SET ROLE "+role)
		defer mustExec(t, ctx, c, "RESET ROLE")
		_, err := c.Exec(ctx, sql)
		return err
	}
	if err := asRole("app_worker", `UPDATE audit.kra_outbox SET exported_at = now()`); err != nil {
		t.Fatalf("worker cannot mark exported: %v", err)
	}
	wantState(t, asRole("app_worker", `UPDATE audit.kra_outbox SET payload = '{"x":1}'`), codeInsufficientPriv)
	wantState(t, asRole("app_api", `UPDATE audit.kra_outbox SET exported_at = now()`), codeInsufficientPriv)
	wantState(t, asRole("app_worker", `DELETE FROM audit.kra_outbox`), codeInsufficientPriv)
}

func TestRoles_AppRolesCannotChangeSchema(t *testing.T) {
	ctx, c := connect(t)
	for _, role := range []string{"app_api", "app_worker", "app_readonly"} {
		for _, s := range []string{`CREATE TABLE wallet.evil (id int)`, `DROP TABLE wallet.wallets`, `ALTER TABLE wallet.wallets DROP COLUMN balance_kes`} {
			mustExec(t, ctx, c, "SET ROLE "+role)
			_, err := c.Exec(ctx, s)
			mustExec(t, ctx, c, "RESET ROLE")
			if sqlState(err) == "" {
				t.Errorf("%s: %q succeeded", role, s)
			}
		}
	}
}

func TestRoles_ReadonlyCannotWriteAnything(t *testing.T) {
	ctx, c := connect(t)
	mustExec(t, ctx, c, "SET ROLE app_readonly")
	_, err := c.Exec(ctx, `INSERT INTO identity.users (msisdn) VALUES ('254700000000')`)
	wantState(t, err, codeInsufficientPriv)
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM identity.users`).Scan(&n); err != nil {
		t.Fatalf("readonly cannot read: %v", err)
	}
}

func TestRoles_OnlyTheMigratorOwnsTables(t *testing.T) {
	ctx, c := connect(t)
	rows, err := c.Query(ctx, `SELECT schemaname || '.' || tablename, tableowner FROM pg_tables
		WHERE schemaname IN ('identity','kyc','rg','wallet','payments','ledger','tickets','game','draws','payouts','audit','platform')
		AND tableowner <> 'app_migrator'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var tbl, owner string
		_ = rows.Scan(&tbl, &owner)
		t.Errorf("%s is owned by %s, want app_migrator", tbl, owner)
	}
}

func TestSchema_MoneyIsIntegerAndTimeIsTimestamptz(t *testing.T) {
	ctx, c := connect(t)
	rows, err := c.Query(ctx, `SELECT table_schema||'.'||table_name||'.'||column_name, data_type FROM information_schema.columns
		WHERE table_schema IN ('identity','kyc','rg','wallet','payments','ledger','tickets','game','draws','payouts','audit','platform')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	kes := regexp.MustCompile(`_kes$`)
	for rows.Next() {
		var col, typ string
		_ = rows.Scan(&col, &typ)
		switch {
		case typ == "real" || typ == "double precision" || typ == "money":
			t.Errorf("%s is %s: floats and money are banned", col, typ)
		case typ == "timestamp without time zone":
			t.Errorf("%s is timestamp without time zone", col)
		case kes.MatchString(col) && typ != "bigint":
			t.Errorf("%s is %s, want bigint", col, typ)
		}
	}
}

func TestSchema_UUIDv7DefaultProducesVersion7(t *testing.T) {
	ctx, c := connect(t)
	var ver int
	if err := c.QueryRow(ctx, `SELECT (get_byte(uuid_send(platform.uuid_v7()), 6) >> 4)`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != 7 {
		t.Fatalf("uuid version = %d, want 7", ver)
	}
}

func TestPostgresClock_IsStableInsideATransaction(t *testing.T) {
	ctx, c := connect(t)
	tx, _ := c.Begin(ctx)
	defer func() { _ = tx.Rollback(ctx) }()
	var cl clock.Postgres
	a, err := cl.Now(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) //nolint:forbidigo // proves now() does not advance inside a transaction
	b, _ := cl.Now(ctx, tx)
	if !a.Equal(b) {
		t.Fatalf("clock moved inside a transaction: %v then %v", a, b)
	}
}
