-- +goose Up
-- Only these two response headers are stored and replayed; arbitrary handler headers never are.
ALTER TABLE platform.idempotency_keys
  ADD COLUMN response_content_type text,
  ADD COLUMN response_location text;

-- +goose Down
ALTER TABLE platform.idempotency_keys DROP COLUMN IF EXISTS response_location, DROP COLUMN IF EXISTS response_content_type;
