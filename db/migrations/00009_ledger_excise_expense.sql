-- +goose Up
-- Excise absorbed by the operator (deposit excise flag = absorbed) is an expense, not a reduction of stakes revenue.
INSERT INTO ledger.ledger_accounts (code, name, type) VALUES ('EXCISE_EXPENSE', 'Excise duty absorbed by operator', 'expense');

-- +goose Down
DELETE FROM ledger.ledger_accounts WHERE code = 'EXCISE_EXPENSE';
