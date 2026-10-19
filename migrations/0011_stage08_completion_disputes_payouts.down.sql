-- Stage 08 revert assumes no rows exist in the new booking states and no
-- payout requests were created; active data with new states must be resolved
-- before reverting.

DROP TRIGGER payout_requests_transition_guard ON payout_requests;
DROP FUNCTION enforce_payout_request_transition();
DROP INDEX payout_requests_active_unique;
DROP INDEX idx_payout_requests_provider;
DROP TABLE payout_requests;

ALTER TABLE audit_events ALTER COLUMN actor_user_id SET NOT NULL;

ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_kind_check;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_kind_check
    CHECK (kind IN (
        'customer_available', 'customer_held', 'provider_earnings',
        'platform_commission', 'funding_clearing', 'refund_clearing'
    ));

ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_check;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_check
    CHECK (owner_user_id IS NULL OR kind IN (
        'customer_available', 'customer_held', 'provider_earnings'
    ));

ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_check1;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_check1
    CHECK (owner_user_id IS NOT NULL OR kind IN (
        'platform_commission', 'funding_clearing', 'refund_clearing'
    ));

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_kind_check
    CHECK (kind IN ('funding', 'hold_place', 'hold_release', 'refund_release', 'hold_settle', 'reversal'));

DROP TRIGGER booking_disputes_transition_guard ON booking_disputes;
DROP FUNCTION enforce_booking_dispute_transition();
DROP TABLE booking_disputes;

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
            (booking_status IN ('confirmed', 'in_progress') AND hold_state = 'active') OR
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

ALTER TABLE bookings DROP CONSTRAINT bookings_no_overlapping_active_provider_slots;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_no_overlapping_active_provider_slots
    EXCLUDE USING gist (
        provider_id WITH =,
        booking_service_slot_range(starts_at, ends_at, buffer_minutes) WITH &&
    ) WHERE (status IN ('confirmed', 'in_progress'));

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

ALTER TABLE bookings DROP CONSTRAINT bookings_completion_requested_check;
ALTER TABLE bookings DROP CONSTRAINT bookings_completed_stage_check;
ALTER TABLE bookings
    DROP COLUMN completion_requested_at,
    DROP COLUMN completed_at;

-- Restore the pre-Stage-08 status allow-lists on the history table.
ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_from_status_check;
ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_to_status_check;
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_from_status_check
    CHECK (from_status IS NULL OR from_status IN (
        'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completed', 'cancelled', 'expired'
    ));
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_to_status_check
    CHECK (to_status IN (
        'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completed', 'cancelled', 'expired'
    ));

ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN ('confirmed', 'in_progress', 'completed', 'cancelled', 'expired'));
