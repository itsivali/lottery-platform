-- +goose Up
-- River (Postgres-backed jobs) keeps its tables in their own schema. The tables themselves are created by River's own
-- migrator, which db.MigrateUp runs after goose; this just reserves the schema so grants and ownership stay visible.
CREATE SCHEMA IF NOT EXISTS river;

-- +goose Down
DROP SCHEMA IF EXISTS river CASCADE;
