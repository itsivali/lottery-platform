-- +goose Up
CREATE TABLE game.odds_config (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  slot_count      smallint NOT NULL CHECK (slot_count >= 2),
  lucky_slots     smallint NOT NULL CHECK (lucky_slots >= 1),
  effective_from  date NOT NULL,      -- first play date to which this version applies
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT odds_config_lucky_lt_slots CHECK (lucky_slots < slot_count),
  CONSTRAINT odds_config_effective_from_key UNIQUE (effective_from)
);
-- PLACEHOLDER defaults (docs/OPEN_DECISIONS.md #6): 12 slots, 1 lucky. Effective from the epoch so they always apply.
INSERT INTO game.odds_config (slot_count, lucky_slots, effective_from, created_by) VALUES (12, 1, '2000-01-01', 'migration:placeholder');

CREATE TABLE game.chance_tiers (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  effective_from  date NOT NULL,
  plays           smallint NOT NULL CHECK (plays BETWEEN 1 AND 4),
  chances         smallint NOT NULL CHECK (chances > 0),
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT chance_tiers_from_plays_key UNIQUE (effective_from, plays)
);
-- PLACEHOLDER defaults (OPEN_DECISIONS.md #9): 1->1, 2->2, 3->3, 4->5.
INSERT INTO game.chance_tiers (effective_from, plays, chances, created_by) VALUES
  ('2000-01-01',1,1,'migration:placeholder'), ('2000-01-01',2,2,'migration:placeholder'),
  ('2000-01-01',3,3,'migration:placeholder'), ('2000-01-01',4,5,'migration:placeholder');

CREATE TABLE game.seed_chains (
  id           uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id      uuid NOT NULL REFERENCES identity.users (id),
  seed_enc     bytea NOT NULL,                                   -- encrypted server seed
  commitment   text NOT NULL CHECK (commitment ~ '^[0-9a-f]{64}$'),
  client_seed  text NOT NULL,
  next_nonce   bigint NOT NULL DEFAULT 0 CHECK (next_nonce >= 0),
  status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active','rotated')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  revealed_at  timestamptz,
  CONSTRAINT seed_chains_revealed_when_rotated CHECK ((status = 'rotated') = (revealed_at IS NOT NULL))
);
CREATE UNIQUE INDEX seed_chains_one_active_per_user ON game.seed_chains (user_id) WHERE status = 'active';

CREATE TABLE tickets.player_days (
  user_id      uuid NOT NULL REFERENCES identity.users (id),
  play_date    date NOT NULL,
  paid_plays   smallint NOT NULL DEFAULT 0 CHECK (paid_plays BETWEEN 0 AND 4),
  lucky        boolean NOT NULL DEFAULT false,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, play_date)
);

CREATE TABLE tickets.tickets (
  id               uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  user_id          uuid NOT NULL,
  play_date        date NOT NULL,
  seq              smallint NOT NULL,
  price_kes        bigint NOT NULL CHECK (price_kes > 0),
  status           text NOT NULL DEFAULT 'paid' CHECK (status IN ('paid','refunded')),
  idempotency_key  text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  -- invariant 6: the daily cap lives in the database
  CONSTRAINT tickets_seq_range CHECK (seq BETWEEN 1 AND 4),
  CONSTRAINT tickets_user_day_seq_key UNIQUE (user_id, play_date, seq),
  CONSTRAINT tickets_user_idempotency_key UNIQUE (user_id, idempotency_key),
  CONSTRAINT tickets_player_day_fk FOREIGN KEY (user_id) REFERENCES identity.users (id)
);
CREATE INDEX tickets_play_date_idx ON tickets.tickets (play_date);

CREATE TABLE game.spins (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  ticket_id       uuid NOT NULL REFERENCES tickets.tickets (id),
  chain_id        uuid NOT NULL REFERENCES game.seed_chains (id),
  client_seed     text NOT NULL,
  nonce           bigint NOT NULL CHECK (nonce >= 0),
  slot            smallint NOT NULL CHECK (slot >= 0),
  lucky           boolean NOT NULL,
  odds_config_id  uuid NOT NULL REFERENCES game.odds_config (id),
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT spins_ticket_key UNIQUE (ticket_id),
  CONSTRAINT spins_chain_nonce_key UNIQUE (chain_id, nonce)
);
CREATE TRIGGER spins_append_only BEFORE UPDATE OR DELETE ON game.spins
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER spins_no_truncate BEFORE TRUNCATE ON game.spins
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE draws.prize_structures (
  id              uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  draw_type       text NOT NULL CHECK (draw_type IN ('daily','weekly','monthly','quarterly')),
  effective_from  date NOT NULL,
  kind            text NOT NULL CHECK (kind IN ('fixed','pool_share')),
  rounding        text NOT NULL DEFAULT 'half_up' CHECK (rounding IN ('half_up','down','up')),
  prizes          jsonb NOT NULL,       -- ordered tiers; fixed: amount_kes, pool_share: share as numeric string
  created_by      text NOT NULL,
  created_at      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT prize_structures_type_from_key UNIQUE (draw_type, effective_from),
  CONSTRAINT prize_structures_prizes_array CHECK (jsonb_typeof(prizes) = 'array' AND jsonb_array_length(prizes) > 0)
);

CREATE TABLE draws.draws (
  id                  uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  draw_type           text NOT NULL CHECK (draw_type IN ('daily','weekly','monthly','quarterly')),
  period_start        date NOT NULL,
  period_end          date NOT NULL,
  status              text NOT NULL DEFAULT 'open'
                        CHECK (status IN ('open','frozen','signed','conducted','approved','paid','voided')),
  seed_commit         text CHECK (seed_commit ~ '^[0-9a-f]{64}$'),
  seed_enc            bytea,
  revealed_seed       text,
  beacon_round        bigint CHECK (beacon_round > 0),
  beacon_value        text,
  entry_list_hash     text CHECK (entry_list_hash ~ '^[0-9a-f]{64}$'),
  certificate_json    jsonb,
  certificate_hash    text,
  frozen_at           timestamptz,
  voided_reason       text,
  rerun_of            uuid REFERENCES draws.draws (id),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draws_period_order CHECK (period_end >= period_start),
  CONSTRAINT draws_voided_has_reason CHECK (status <> 'voided' OR voided_reason IS NOT NULL),
  CONSTRAINT draws_seed_pair CHECK ((seed_enc IS NULL) = (seed_commit IS NULL))
);
-- One live draw per period; a voided draw may be re-run into a new row (rerun_of links them).
CREATE UNIQUE INDEX draws_one_live_per_period ON draws.draws (draw_type, period_start) WHERE status <> 'voided';

-- A draw's seed may be set once. A second seed is an attack or a bug and must fail loudly (audit alarm raised by the service).
-- +goose StatementBegin
CREATE FUNCTION draws.forbid_seed_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.seed_enc IS NOT NULL AND (NEW.seed_enc IS DISTINCT FROM OLD.seed_enc OR NEW.seed_commit IS DISTINCT FROM OLD.seed_commit) THEN
    RAISE EXCEPTION 'draw % already has a seed; refusing to replace it', OLD.id USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER draws_seed_immutable BEFORE UPDATE ON draws.draws
  FOR EACH ROW EXECUTE FUNCTION draws.forbid_seed_change();

CREATE TABLE draws.draw_entries (
  id         uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  draw_id    uuid NOT NULL REFERENCES draws.draws (id),
  entry_no   integer NOT NULL CHECK (entry_no >= 1),
  user_id    uuid NOT NULL REFERENCES identity.users (id),
  play_date  date NOT NULL,
  ticket_id  uuid NOT NULL REFERENCES tickets.tickets (id),
  CONSTRAINT draw_entries_draw_no_key UNIQUE (draw_id, entry_no)
);
CREATE TRIGGER draw_entries_append_only BEFORE UPDATE OR DELETE ON draws.draw_entries
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER draw_entries_no_truncate BEFORE TRUNCATE ON draws.draw_entries
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

CREATE TABLE draws.draw_signoffs (
  id            uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  draw_id       uuid NOT NULL REFERENCES draws.draws (id),
  step          text NOT NULL CHECK (step IN ('sign','approve','void')),
  organisation  text NOT NULL CHECK (organisation IN ('company','gra')),
  official      text NOT NULL,
  signature     text NOT NULL,
  signed_at     timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draw_signoffs_step_org_key UNIQUE (draw_id, step, organisation)
);

CREATE TABLE draws.draw_results (
  id            uuid PRIMARY KEY DEFAULT platform.uuid_v7(),
  draw_id       uuid NOT NULL REFERENCES draws.draws (id),
  entry_id      uuid NOT NULL REFERENCES draws.draw_entries (id),
  prize_rank    integer NOT NULL CHECK (prize_rank >= 1),
  amount_kes    bigint NOT NULL CHECK (amount_kes > 0),
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT draw_results_draw_entry_key UNIQUE (draw_id, entry_id),   -- one entry wins at most once per draw
  CONSTRAINT draw_results_draw_rank_key UNIQUE (draw_id, prize_rank)
);
CREATE TRIGGER draw_results_append_only BEFORE UPDATE OR DELETE ON draws.draw_results
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER draw_results_no_truncate BEFORE TRUNCATE ON draws.draw_results
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

ALTER TABLE payouts.payouts ADD CONSTRAINT payouts_draw_result_fk FOREIGN KEY (draw_result_id) REFERENCES draws.draw_results (id);

-- +goose Down
ALTER TABLE payouts.payouts DROP CONSTRAINT IF EXISTS payouts_draw_result_fk;
DROP TABLE IF EXISTS draws.draw_results, draws.draw_signoffs, draws.draw_entries, draws.draws, draws.prize_structures,
  game.spins, tickets.tickets, tickets.player_days, game.seed_chains, game.chance_tiers, game.odds_config;
DROP FUNCTION IF EXISTS draws.forbid_seed_change();
