-- +goose Up
-- Phase 0: extensions only. The real schema arrives in Phase 1.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- +goose Down
DROP EXTENSION IF EXISTS pgcrypto;
