-- +goose Up
-- A submission is stored as 'pending' in the same transaction that enqueues its verification job.
ALTER TABLE kyc.kyc_checks DROP CONSTRAINT kyc_checks_status_check;
ALTER TABLE kyc.kyc_checks ADD CONSTRAINT kyc_checks_status_check
  CHECK (status IN ('pending','verified','mismatch','manual_review','rejected','error'));

-- +goose Down
DELETE FROM kyc.kyc_checks WHERE status = 'pending';
ALTER TABLE kyc.kyc_checks DROP CONSTRAINT kyc_checks_status_check;
ALTER TABLE kyc.kyc_checks ADD CONSTRAINT kyc_checks_status_check
  CHECK (status IN ('verified','mismatch','manual_review','rejected','error'));
