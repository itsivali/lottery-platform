-- name: InsertAuditEvent :exec
INSERT INTO audit.audit_log (actor_type, actor_id, action, entity_type, entity_id, details)
VALUES ($1, $2, $3, $4, $5, $6);
