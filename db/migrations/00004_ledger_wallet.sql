-- +goose Up
CREATE TABLE ledger.ledger_accounts (
  id         uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  code       text NOT NULL CHECK (code ~ '^[A-Z0-9_]+$'),
  name       text NOT NULL,
  type       text NOT NULL CHECK (type IN ('asset','liability','revenue','expense')),
  created_at timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_accounts_code_key UNIQUE (code)
);

-- Minimum chart of accounts (brief section 5).
INSERT INTO ledger.ledger_accounts (code, name, type) VALUES
  ('WALLET_LIABILITY',      'Player wallet liability',            'liability'),
  ('MPESA_CLEARING',        'M-Pesa paybill clearing',            'asset'),
  ('B2C_FLOAT',             'B2C float',                          'asset'),
  ('STAKES_REVENUE',        'Stakes revenue',                     'revenue'),
  ('PRIZE_POOL_DAILY',      'Prize pool liability - daily',       'liability'),
  ('PRIZE_POOL_WEEKLY',     'Prize pool liability - weekly',      'liability'),
  ('PRIZE_POOL_MONTHLY',    'Prize pool liability - monthly',     'liability'),
  ('PRIZE_POOL_QUARTERLY',  'Prize pool liability - quarterly',   'liability'),
  ('EXCISE_PAYABLE',        'Excise duty payable',                'liability'),
  ('TAX_PAYABLE',           'Tax payable on withdrawals/winnings','liability'),
  ('REFUNDS',               'Refunds',                            'expense'),
  ('SUSPENSE',              'Suspense (unmatched C2B)',           'liability'),
  ('PLAYER_DEBT',           'Player debt (reversals)',            'asset');

-- A journal is one financial event. (kind, reference_type, reference_id) makes posting the same event twice impossible.
CREATE TABLE ledger.ledger_journals (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  kind            text NOT NULL,
  reference_type  text NOT NULL,
  reference_id    uuid NOT NULL,
  description     text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ledger_journals_event_key UNIQUE (kind, reference_type, reference_id)
);

CREATE TABLE ledger.ledger_entries (
  id          uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  journal_id  uuid NOT NULL REFERENCES ledger.ledger_journals (id),
  account_id  uuid NOT NULL REFERENCES ledger.ledger_accounts (id),
  user_id     uuid REFERENCES identity.users (id),   -- set on player-wallet postings for per-player reconciliation
  direction   char(1) NOT NULL CHECK (direction IN ('D','C')),
  amount_kes  bigint NOT NULL CHECK (amount_kes > 0),
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_journal_idx ON ledger.ledger_entries (journal_id);
CREATE INDEX ledger_entries_account_idx ON ledger.ledger_entries (account_id, created_at);

-- Versioned rates. Rounding is part of the version; the remainder is posted explicitly by the posting rules.
CREATE TABLE ledger.tax_rules (
  id                  uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  code                text NOT NULL CHECK (code ~ '^[a-z0-9_]+$'),
  rate                numeric(6,5) NOT NULL CHECK (rate >= 0 AND rate <= 1),
  point_of_taxation   text NOT NULL,
  rounding            text NOT NULL DEFAULT 'half_up' CHECK (rounding IN ('half_up','down','up')),
  effective_from      date NOT NULL,
  created_by          text NOT NULL,
  created_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tax_rules_code_from_key UNIQUE (code, effective_from)
);

-- Every journal must balance (debits = credits) and contain at least two entries, checked at COMMIT.
-- +goose StatementBegin
CREATE FUNCTION ledger.assert_journal_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  j uuid;
  diff bigint;
  n bigint;
BEGIN
  IF TG_TABLE_NAME = 'ledger_journals' THEN
    j := NEW.id;
  ELSE
    j := NEW.journal_id;
  END IF;
  SELECT coalesce(sum(CASE direction WHEN 'D' THEN amount_kes ELSE -amount_kes END), 0), count(*)
    INTO diff, n FROM ledger.ledger_entries WHERE journal_id = j;
  IF n < 2 THEN
    RAISE EXCEPTION 'journal % has % entries; at least 2 required', j, n USING ERRCODE = 'check_violation';
  END IF;
  IF diff <> 0 THEN
    RAISE EXCEPTION 'journal % does not balance (debits - credits = %)', j, diff USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER journal_balanced_on_entry AFTER INSERT ON ledger.ledger_entries
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.assert_journal_balanced();
CREATE CONSTRAINT TRIGGER journal_balanced_on_journal AFTER INSERT ON ledger.ledger_journals
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ledger.assert_journal_balanced();

CREATE TRIGGER ledger_entries_append_only BEFORE UPDATE OR DELETE ON ledger.ledger_entries
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER ledger_entries_no_truncate BEFORE TRUNCATE ON ledger.ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER ledger_journals_append_only BEFORE UPDATE OR DELETE ON ledger.ledger_journals
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER ledger_journals_no_truncate BEFORE TRUNCATE ON ledger.ledger_journals
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE wallet.wallets (
  user_id      uuid PRIMARY KEY REFERENCES identity.users (id),
  balance_kes  bigint NOT NULL DEFAULT 0 CONSTRAINT wallets_balance_non_negative CHECK (balance_kes >= 0),
  updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE wallet.wallet_transactions (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id         uuid NOT NULL REFERENCES wallet.wallets (user_id),
  journal_id      uuid NOT NULL REFERENCES ledger.ledger_journals (id),
  kind            text NOT NULL CHECK (kind IN ('deposit','deposit_excise','stake','withdrawal','prize','refund','adjustment','reversal')),
  amount_kes      bigint NOT NULL CHECK (amount_kes <> 0),    -- signed: credits positive, debits negative
  reference_type  text,
  reference_id    uuid,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX wallet_transactions_user_idx ON wallet.wallet_transactions (user_id, created_at DESC);

-- wallets.balance_kes must equal the sum of the user's transactions when the transaction commits.
-- +goose StatementBegin
CREATE FUNCTION wallet.assert_wallet_reconciled() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
  u uuid := NEW.user_id;
  bal bigint;
  tx_sum bigint;
BEGIN
  SELECT balance_kes INTO bal FROM wallet.wallets WHERE user_id = u;
  SELECT coalesce(sum(amount_kes), 0) INTO tx_sum FROM wallet.wallet_transactions WHERE user_id = u;
  IF bal IS DISTINCT FROM tx_sum THEN
    RAISE EXCEPTION 'wallet % balance % differs from transaction sum %', u, bal, tx_sum USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER wallet_reconciled_on_tx AFTER INSERT ON wallet.wallet_transactions
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION wallet.assert_wallet_reconciled();
CREATE CONSTRAINT TRIGGER wallet_reconciled_on_wallet AFTER INSERT OR UPDATE OF balance_kes ON wallet.wallets
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION wallet.assert_wallet_reconciled();

CREATE TRIGGER wallet_transactions_append_only BEFORE UPDATE OR DELETE ON wallet.wallet_transactions
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER wallet_transactions_no_truncate BEFORE TRUNCATE ON wallet.wallet_transactions
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

-- +goose Down
DROP TABLE IF EXISTS wallet.wallet_transactions, wallet.wallets, ledger.tax_rules, ledger.ledger_entries,
  ledger.ledger_journals, ledger.ledger_accounts;
DROP FUNCTION IF EXISTS wallet.assert_wallet_reconciled();
DROP FUNCTION IF EXISTS ledger.assert_journal_balanced();
