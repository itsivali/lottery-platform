-- +goose Up
CREATE TABLE platform.idempotency_keys (
  principal        text NOT NULL,     -- 'user:<uuid>' for authenticated calls, 'anon:<ip-or-device>' otherwise
  key              text NOT NULL CHECK (key <> '' AND length(key) <= 255),
  request_hash     bytea NOT NULL,    -- SHA-256 of method, path and body
  response_status  smallint,
  response_body    bytea,
  created_at       timestamptz NOT NULL DEFAULT now(),
  completed_at     timestamptz,
  expires_at       timestamptz NOT NULL,
  PRIMARY KEY (principal, key)
);
CREATE INDEX idempotency_keys_expiry_idx ON platform.idempotency_keys (expires_at);

CREATE TABLE audit.audit_log (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at           timestamptz NOT NULL DEFAULT now(),
  actor_type   text NOT NULL CHECK (actor_type IN ('system','player','staff')),
  actor_id     text,
  action       text NOT NULL,
  entity_type  text NOT NULL,
  entity_id    text,
  details      jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX audit_log_entity_idx ON audit.audit_log (entity_type, entity_id, at);
CREATE TRIGGER audit_log_append_only BEFORE UPDATE OR DELETE ON audit.audit_log
  FOR EACH ROW EXECUTE FUNCTION platform.forbid_mutation();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit.audit_log
  FOR EACH STATEMENT EXECUTE FUNCTION platform.forbid_mutation();

-- Revenue events for the KRA monitoring feed. Append-only except exported_at (column-level UPDATE grant).
CREATE TABLE audit.kra_outbox (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_type   text NOT NULL,
  payload      jsonb NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  exported_at  timestamptz
);
CREATE INDEX kra_outbox_pending_idx ON audit.kra_outbox (id) WHERE exported_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS audit.kra_outbox, audit.audit_log, platform.idempotency_keys;
