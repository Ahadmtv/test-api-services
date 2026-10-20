-- Revert direct gateway booking payments. This downgrade treats already
-- captured gateway holds as wallet-funded holds and cancels unpaid reservations.
UPDATE bookings
SET status = 'cancelled', updated_at = now()
WHERE status = 'pending_payment';

UPDATE bookings
SET payment_method = 'wallet'
WHERE payment_method = 'zarinpal';

DROP INDEX IF EXISTS payment_attempts_one_active_booking;
ALTER TABLE payment_attempts DROP CONSTRAINT payment_attempts_purpose_target_check;
ALTER TABLE payment_attempts
    DROP COLUMN booking_id,
    DROP COLUMN purpose;

DROP INDEX IF EXISTS idx_bookings_pending_payment_expiration;
ALTER TABLE bookings DROP CONSTRAINT bookings_pending_payment_deadline_check;
ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN (
        'confirmed', 'in_progress', 'completion_pending', 'disputed',
        'completed', 'cancelled', 'expired'
    ));
ALTER TABLE bookings DROP COLUMN payment_expires_at;
ALTER TABLE bookings DROP CONSTRAINT bookings_payment_method_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_payment_method_check
    CHECK (payment_method IN ('none', 'wallet'));

ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_from_status_check;
ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_to_status_check;
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_from_status_check
    CHECK (from_status IS NULL OR from_status IN (
        'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completion_pending', 'disputed', 'completed', 'cancelled', 'expired'
    ));
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_to_status_check
    CHECK (to_status IN (
        'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completion_pending', 'disputed', 'completed', 'cancelled', 'expired'
    ));

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
        (OLD.status = 'confirmed' AND NEW.status IN ('in_progress', 'cancelled')) OR
        (OLD.status = 'in_progress' AND NEW.status = 'completion_pending') OR
        (OLD.status = 'completion_pending' AND NEW.status IN ('completed', 'disputed')) OR
        (OLD.status = 'disputed' AND NEW.status IN ('completed', 'cancelled'))
    ) THEN
        RAISE EXCEPTION 'invalid booking state transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_state_transition_check';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE bookings DROP CONSTRAINT bookings_no_overlapping_active_provider_slots;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_no_overlapping_active_provider_slots
    EXCLUDE USING gist (
        provider_id WITH =,
        booking_service_slot_range(starts_at, ends_at, buffer_minutes) WITH &&
    ) WHERE (status IN ('confirmed', 'in_progress', 'completion_pending', 'disputed'));

CREATE OR REPLACE FUNCTION enforce_booking_wallet_consistency() RETURNS trigger AS $$
DECLARE
    target_booking_id BIGINT;
    booking_status VARCHAR(32);
    booking_payment VARCHAR(16);
    booking_client_id BIGINT;
    booking_provider_id BIGINT;
    booking_amount_irr BIGINT;
    hold_state VARCHAR(16);
BEGIN
    IF TG_TABLE_NAME = 'bookings' THEN
        target_booking_id := NEW.id;
    ELSE
        target_booking_id := NEW.booking_id;
    END IF;

    SELECT b.status, b.payment_method, b.client_id, b.provider_id, b.price_irr, h.status
    INTO booking_status, booking_payment, booking_client_id, booking_provider_id, booking_amount_irr, hold_state
    FROM bookings b
    LEFT JOIN wallet_holds h ON h.booking_id = b.id
    WHERE b.id = target_booking_id;

    IF booking_payment = 'none' THEN
        IF hold_state IS NOT NULL THEN
            RAISE EXCEPTION 'booking without wallet payment cannot have a wallet hold'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    ELSE
        IF hold_state IS NULL OR NOT EXISTS (
            SELECT 1 FROM wallet_holds h
            WHERE h.booking_id = target_booking_id
              AND h.customer_id = booking_client_id
              AND h.provider_id = booking_provider_id
              AND h.amount_irr = booking_amount_irr
        ) OR NOT (
            (booking_status IN ('confirmed', 'in_progress', 'completion_pending', 'disputed') AND hold_state = 'active') OR
            (booking_status = 'cancelled' AND hold_state = 'released') OR
            (booking_status = 'completed' AND hold_state IN ('settled', 'refunded'))
        ) THEN
            RAISE EXCEPTION 'wallet hold state % does not match booking status %', hold_state, booking_status
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION guard_payment_attempt_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment attempts cannot be deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_immutable';
    END IF;
    IF NEW.client_id IS DISTINCT FROM OLD.client_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.request_fingerprint IS DISTINCT FROM OLD.request_fingerprint
        OR NEW.amount_irr IS DISTINCT FROM OLD.amount_irr
        OR NEW.callback_url IS DISTINCT FROM OLD.callback_url
        OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.request_started_at IS DISTINCT FROM OLD.request_started_at THEN
        RAISE EXCEPTION 'payment request terms are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_terms_immutable';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'created' AND NEW.status IN ('pending', 'failed', 'verification_unknown')) OR
        (OLD.status = 'pending' AND NEW.status IN ('failed', 'succeeded', 'verification_unknown')) OR
        (OLD.status = 'verification_unknown' AND NEW.status IN ('failed', 'succeeded', 'pending'))
    ) THEN
        RAISE EXCEPTION 'invalid payment state transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_state_transition';
    END IF;
    IF OLD.authority IS NOT NULL AND NEW.authority IS DISTINCT FROM OLD.authority THEN
        RAISE EXCEPTION 'payment authority is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_authority_immutable';
    END IF;
    IF OLD.gateway_reference_id IS NOT NULL AND NEW.gateway_reference_id IS DISTINCT FROM OLD.gateway_reference_id THEN
        RAISE EXCEPTION 'payment reference is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_reference_immutable';
    END IF;
    IF OLD.ledger_transaction_id IS NOT NULL AND NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id THEN
        RAISE EXCEPTION 'payment ledger transaction is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_ledger_immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION enforce_payment_ledger_consistency() RETURNS trigger AS $$
DECLARE
    posting_reference TEXT;
    debit_amount NUMERIC;
    credit_amount NUMERIC;
    entry_count BIGINT;
BEGIN
    IF NEW.status <> 'succeeded' THEN
        RETURN NULL;
    END IF;
    SELECT t.reference,
           COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'funding_clearing' AND e.side = 'debit'), 0),
           COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'customer_available' AND a.owner_user_id = NEW.client_id AND e.side = 'credit'), 0),
           COUNT(*)
    INTO posting_reference, debit_amount, credit_amount, entry_count
    FROM ledger_transactions t
    JOIN ledger_entries e ON e.transaction_id = t.id
    JOIN wallet_accounts a ON a.id = e.account_id
    WHERE t.id = NEW.ledger_transaction_id AND t.kind = 'funding'
    GROUP BY t.reference;
    IF posting_reference IS DISTINCT FROM 'payment:' || NEW.id || ':funding'
        OR entry_count <> 2 OR debit_amount <> NEW.amount_irr OR credit_amount <> NEW.amount_irr THEN
        RAISE EXCEPTION 'payment does not match its wallet funding transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_consistency';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
