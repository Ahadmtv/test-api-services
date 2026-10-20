-- Stage 10: direct gateway payment for bookings.
-- A paid booking reserves its slot while the client completes Zarinpal payment.
-- Verified funds are captured directly into the booking hold, never into the
-- customer's spendable wallet balance.

ALTER TABLE bookings
    ADD COLUMN payment_expires_at TIMESTAMPTZ;

ALTER TABLE bookings DROP CONSTRAINT bookings_payment_method_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_payment_method_check
    CHECK (payment_method IN ('none', 'wallet', 'zarinpal'));

ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN (
        'pending_payment', 'confirmed', 'in_progress', 'completion_pending',
        'disputed', 'completed', 'cancelled', 'expired'
    ));

ALTER TABLE bookings
    ADD CONSTRAINT bookings_pending_payment_deadline_check
    CHECK (
        status <> 'pending_payment'
        OR (payment_method = 'zarinpal' AND payment_expires_at IS NOT NULL)
    );

CREATE INDEX idx_bookings_pending_payment_expiration
    ON bookings (payment_expires_at, id)
    WHERE status = 'pending_payment';

ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_from_status_check;
ALTER TABLE booking_state_transitions DROP CONSTRAINT booking_state_transitions_to_status_check;
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_from_status_check
    CHECK (from_status IS NULL OR from_status IN (
        'pending_payment', 'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completion_pending', 'disputed', 'completed', 'cancelled', 'expired'
    ));
ALTER TABLE booking_state_transitions
    ADD CONSTRAINT booking_state_transitions_to_status_check
    CHECK (to_status IN (
        'pending_payment', 'pending_provider_acceptance', 'confirmed', 'in_progress',
        'completion_pending', 'disputed', 'completed', 'cancelled', 'expired'
    ));

CREATE OR REPLACE FUNCTION enforce_booking_state_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('confirmed', 'pending_payment') THEN
            RAISE EXCEPTION 'new bookings must start confirmed or pending_payment'
                USING ERRCODE = '23514', CONSTRAINT = 'bookings_initial_status_check';
        END IF;
        IF NEW.status = 'pending_payment' AND (
            NEW.payment_method <> 'zarinpal' OR NEW.payment_expires_at IS NULL
        ) THEN
            RAISE EXCEPTION 'pending payment booking requires gateway payment and deadline'
                USING ERRCODE = '23514', CONSTRAINT = 'bookings_pending_payment_deadline_check';
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

    IF OLD.status = 'pending_payment' AND NEW.status = 'confirmed'
        AND OLD.payment_method <> 'zarinpal' THEN
        RAISE EXCEPTION 'only gateway payment bookings can confirm from pending_payment'
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_pending_payment_confirm_check';
    END IF;

    IF NOT (
        (OLD.status = 'pending_payment' AND NEW.status IN ('confirmed', 'cancelled', 'expired')) OR
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
    ) WHERE (status IN ('pending_payment', 'confirmed', 'in_progress', 'completion_pending', 'disputed'));

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
            RAISE EXCEPTION 'booking without payment cannot have a wallet hold'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
        RETURN NULL;
    END IF;

    IF booking_status = 'pending_payment' THEN
        IF booking_payment <> 'zarinpal' OR hold_state IS NOT NULL THEN
            RAISE EXCEPTION 'pending payment booking cannot have a wallet hold'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
        RETURN NULL;
    END IF;

    IF hold_state IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM wallet_holds h
        WHERE h.booking_id = target_booking_id
          AND h.customer_id = booking_client_id
          AND h.provider_id = booking_provider_id
          AND h.amount_irr = booking_amount_irr
    ) THEN
        RAISE EXCEPTION 'wallet hold does not match booking terms'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
    END IF;

    IF booking_status IN ('confirmed', 'in_progress', 'completion_pending', 'disputed') THEN
        IF hold_state IS DISTINCT FROM 'active' THEN
            RAISE EXCEPTION 'wallet hold state % does not match booking status %', hold_state, booking_status
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    ELSIF booking_status = 'cancelled' THEN
        IF hold_state IS NOT NULL AND hold_state <> 'released' THEN
            RAISE EXCEPTION 'cancelled booking has hold state %', hold_state
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    ELSIF booking_status = 'completed' THEN
        IF hold_state NOT IN ('settled', 'refunded') THEN
            RAISE EXCEPTION 'completed booking has hold state %', hold_state
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    ELSIF booking_status = 'expired' THEN
        IF hold_state IS NOT NULL THEN
            RAISE EXCEPTION 'expired booking cannot have a wallet hold'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE payment_attempts
    ADD COLUMN purpose VARCHAR(16) NOT NULL DEFAULT 'wallet_topup'
        CHECK (purpose IN ('wallet_topup', 'booking')),
    ADD COLUMN booking_id BIGINT REFERENCES bookings(id) ON DELETE RESTRICT;

ALTER TABLE payment_attempts
    ADD CONSTRAINT payment_attempts_purpose_target_check
    CHECK ((purpose = 'booking') = (booking_id IS NOT NULL));

CREATE UNIQUE INDEX payment_attempts_one_active_booking
    ON payment_attempts (booking_id)
    WHERE purpose = 'booking' AND status IN ('created', 'pending', 'verification_unknown');

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
        OR NEW.purpose IS DISTINCT FROM OLD.purpose
        OR NEW.booking_id IS DISTINCT FROM OLD.booking_id
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
    posting_kind TEXT;
    debit_amount NUMERIC;
    credit_amount NUMERIC;
    entry_count BIGINT;
BEGIN
    IF NEW.status <> 'succeeded' THEN
        RETURN NULL;
    END IF;

    IF NEW.purpose = 'booking' THEN
        SELECT t.reference,
               t.kind,
               COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'funding_clearing' AND e.side = 'debit'), 0),
               COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'customer_held' AND a.owner_user_id = NEW.client_id AND e.side = 'credit'), 0),
               COUNT(*)
        INTO posting_reference, posting_kind, debit_amount, credit_amount, entry_count
        FROM ledger_transactions t
        JOIN ledger_entries e ON e.transaction_id = t.id
        JOIN wallet_accounts a ON a.id = e.account_id
        WHERE t.id = NEW.ledger_transaction_id
        GROUP BY t.reference, t.kind;

        IF posting_reference IS DISTINCT FROM 'payment:' || NEW.id || ':capture'
            OR posting_kind IS DISTINCT FROM 'hold_place'
            OR entry_count <> 2
            OR debit_amount <> NEW.amount_irr
            OR credit_amount <> NEW.amount_irr
            OR NOT EXISTS (
                SELECT 1 FROM wallet_holds h
                WHERE h.booking_id = NEW.booking_id
                  AND h.place_transaction_id = NEW.ledger_transaction_id
                  AND h.status = 'active'
            ) THEN
            RAISE EXCEPTION 'booking payment does not match its captured hold transaction'
                USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_consistency';
        END IF;
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
