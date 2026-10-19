DROP TRIGGER IF EXISTS bookings_wallet_consistency_check ON bookings;

DROP TABLE IF EXISTS wallet_holds;
DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS wallet_accounts;
DROP TABLE IF EXISTS commission_policies;

DROP FUNCTION IF EXISTS enforce_booking_wallet_consistency();
DROP FUNCTION IF EXISTS enforce_wallet_hold_transition();
DROP FUNCTION IF EXISTS enforce_ledger_transaction_balance();
DROP FUNCTION IF EXISTS apply_ledger_entry_to_balance();
DROP FUNCTION IF EXISTS guard_wallet_account_mutation();
DROP FUNCTION IF EXISTS enforce_ledger_entry_same_transaction();
DROP FUNCTION IF EXISTS prevent_wallet_record_mutation();

ALTER TABLE bookings DROP COLUMN IF EXISTS payment_method;

CREATE OR REPLACE FUNCTION enforce_booking_state_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'confirmed' THEN
            RAISE EXCEPTION 'new bookings must start confirmed'
                USING ERRCODE = '23514', CONSTRAINT = 'bookings_initial_status_check';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF OLD.status = 'confirmed' AND NEW.status = 'in_progress' AND OLD.starts_at > now() THEN
        RAISE EXCEPTION 'service cannot start before its scheduled time'
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_start_deadline_check';
    END IF;

    IF NOT (
        (OLD.status = 'confirmed' AND NEW.status IN ('in_progress', 'cancelled'))
    ) THEN
        RAISE EXCEPTION 'invalid booking state transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_state_transition_check';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
