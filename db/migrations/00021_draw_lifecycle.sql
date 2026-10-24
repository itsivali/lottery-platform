-- +goose Up
-- Phase 4 slice 2: the draw lifecycle (owner decisions 2026-10-24, docs/checkpoints/phase-4.md).

-- Prizes are a share of the period's stakes: the pool is pool_share of the stakes, each rank a share of the pool.
ALTER TABLE draws.prize_structures
  ADD COLUMN pool_share numeric(6,5) CHECK (pool_share > 0 AND pool_share <= 1),
  ADD CONSTRAINT prize_structures_pool_share_needs_share CHECK (kind <> 'pool_share' OR pool_share IS NOT NULL);
-- PLACEHOLDERS until the owner sets them: daily 30%, weekly 10%, monthly 5%, quarterly 5% of stakes; ranks 50/30/20%.
-- Rounded down, so the prizes never exceed the pool.
INSERT INTO draws.prize_structures (draw_type, effective_from, kind, rounding, pool_share, prizes, created_by) VALUES
  ('daily',     '2000-01-01', 'pool_share', 'down', 0.30, '[{"rank":1,"share":"0.50"},{"rank":2,"share":"0.30"},{"rank":3,"share":"0.20"}]', 'migration:placeholder'),
  ('weekly',    '2000-01-01', 'pool_share', 'down', 0.10, '[{"rank":1,"share":"0.50"},{"rank":2,"share":"0.30"},{"rank":3,"share":"0.20"}]', 'migration:placeholder'),
  ('monthly',   '2000-01-01', 'pool_share', 'down', 0.05, '[{"rank":1,"share":"0.50"},{"rank":2,"share":"0.30"},{"rank":3,"share":"0.20"}]', 'migration:placeholder'),
  ('quarterly', '2000-01-01', 'pool_share', 'down', 0.05, '[{"rank":1,"share":"0.50"},{"rank":2,"share":"0.30"},{"rank":3,"share":"0.20"}]', 'migration:placeholder');

-- Two company officials sign each step (owner): one row per official and step, not per organisation.
ALTER TABLE draws.draw_signoffs
  DROP CONSTRAINT draw_signoffs_step_org_key,
  ADD CONSTRAINT draw_signoffs_step_official_key UNIQUE (draw_id, step, official);
CREATE TRIGGER draw_signoffs_append_only BEFORE UPDATE OR DELETE ON draws.draw_signoffs
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();

-- The certificate is published byte for byte as hashed: json keeps the text exactly (jsonb would reorder it).
ALTER TABLE draws.draws ALTER COLUMN certificate_json TYPE json USING certificate_json::json;

-- What each state must carry.
ALTER TABLE draws.draws
  ADD CONSTRAINT draws_frozen_is_fixed CHECK (status IN ('open','voided')
    OR (entry_list_hash IS NOT NULL AND seed_commit IS NOT NULL AND beacon_round IS NOT NULL AND frozen_at IS NOT NULL)),
  ADD CONSTRAINT draws_conducted_has_proof CHECK (status NOT IN ('conducted','approved','paid')
    OR (beacon_value IS NOT NULL AND revealed_seed IS NOT NULL AND certificate_json IS NOT NULL AND certificate_hash IS NOT NULL));

-- Only forward transitions: open → frozen → signed → conducted → approved → paid; open, frozen and signed may be voided.
-- +goose StatementBegin
CREATE FUNCTION draws.check_transition() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.status = OLD.status THEN
    IF OLD.status <> 'open' AND (NEW.entry_list_hash IS DISTINCT FROM OLD.entry_list_hash
        OR NEW.beacon_round IS DISTINCT FROM OLD.beacon_round OR NEW.revealed_seed IS DISTINCT FROM OLD.revealed_seed
        OR NEW.certificate_hash IS DISTINCT FROM OLD.certificate_hash
        OR NEW.certificate_json::text IS DISTINCT FROM OLD.certificate_json::text) THEN
      RAISE EXCEPTION 'draw %: a fixed draw cannot change', OLD.id USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
  END IF;
  IF (OLD.status, NEW.status) NOT IN (('open','frozen'), ('frozen','signed'), ('signed','conducted'),
      ('conducted','approved'), ('approved','paid'), ('open','voided'), ('frozen','voided'), ('signed','voided')) THEN
    RAISE EXCEPTION 'draw %: % -> % is not allowed', OLD.id, OLD.status, NEW.status USING ERRCODE = 'restrict_violation';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
CREATE TRIGGER draws_transition BEFORE UPDATE ON draws.draws
  FOR EACH ROW EXECUTE FUNCTION draws.check_transition();

-- +goose Down
DROP TRIGGER draws_transition ON draws.draws;
DROP FUNCTION draws.check_transition();
ALTER TABLE draws.draws DROP CONSTRAINT draws_conducted_has_proof, DROP CONSTRAINT draws_frozen_is_fixed;
ALTER TABLE draws.draws ALTER COLUMN certificate_json TYPE jsonb USING certificate_json::jsonb;
DROP TRIGGER draw_signoffs_append_only ON draws.draw_signoffs;
ALTER TABLE draws.draw_signoffs DROP CONSTRAINT draw_signoffs_step_official_key,
  ADD CONSTRAINT draw_signoffs_step_org_key UNIQUE (draw_id, step, organisation);
DELETE FROM draws.prize_structures WHERE created_by = 'migration:placeholder';
ALTER TABLE draws.prize_structures DROP CONSTRAINT prize_structures_pool_share_needs_share, DROP COLUMN pool_share;
