-- +goose Up
-- A new limit request voids any pending (future-effective) increase instead of racing it.
ALTER TABLE rg.limits ADD COLUMN cancelled_at timestamptz;

-- +goose Down
ALTER TABLE rg.limits DROP COLUMN IF EXISTS cancelled_at;
