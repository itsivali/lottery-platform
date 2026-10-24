-- +goose Up
-- Phase 3 slice 2: paid plays. The price is versioned with the odds (owner, 2026-10-24: KES 100 per play), so a later
-- price applies from a play date and every ticket keeps the price it was sold at.
ALTER TABLE game.odds_config ADD COLUMN price_kes bigint NOT NULL DEFAULT 100 CHECK (price_kes > 0);
ALTER TABLE game.odds_config ALTER COLUMN price_kes DROP DEFAULT;

-- A rotated chain publishes its server seed, so anyone can check the spins made with it (and keeps doing so after the
-- data keys rotate).
ALTER TABLE game.seed_chains
  ADD COLUMN revealed_seed text CHECK (revealed_seed ~ '^[0-9a-f]{64}$'),
  ADD CONSTRAINT seed_chains_revealed_seed_when_rotated CHECK ((status = 'rotated') = (revealed_seed IS NOT NULL));

-- +goose Down
ALTER TABLE game.seed_chains DROP CONSTRAINT seed_chains_revealed_seed_when_rotated, DROP COLUMN revealed_seed;
ALTER TABLE game.odds_config DROP COLUMN price_kes;
