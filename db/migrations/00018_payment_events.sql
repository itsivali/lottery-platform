-- +goose Up
-- Phase 2 slice 5: realtime confirmation. Every change a player could see on a payment (a new payment, its status,
-- its confirmed amount) is announced on the payment_events channel when the transaction commits. The payload names
-- only the user and the payment; listeners read the payment back, so nothing travels outside the database that a
-- GET would not show, and a rolled-back change is never announced.
-- +goose StatementBegin
CREATE FUNCTION payments.notify_payment_event() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status
     OR OLD.amount_confirmed_kes IS DISTINCT FROM NEW.amount_confirmed_kes THEN
    PERFORM pg_notify('payment_events', NEW.user_id::text || ':' || NEW.id::text);
  END IF;
  RETURN NULL;
END $$;
-- +goose StatementEnd

CREATE TRIGGER payments_notify AFTER INSERT OR UPDATE ON payments.payments
  FOR EACH ROW EXECUTE FUNCTION payments.notify_payment_event();

-- +goose Down
DROP TRIGGER payments_notify ON payments.payments;
DROP FUNCTION payments.notify_payment_event();
