-- Stage 08: controlado completion workflow, disputes and provider payout requests.
-- New booking states keep wallet funds on hold until the client confirms
-- completion, an administrator resolves a dispute, or the dispute window
-- expires. Payouts reserve provider earnings with dedicated ledger kinds so a
-- single reserved amount cannot fund two withdrawal requests.

ALTER TABLE bookings
    ADD COLUMN completion_requested_at TIMESTAMPTZ,
    ADD COLUMN completed_at TIMESTAMPTZ;

-- Existing completed rows predate the completion-audit column; backfill from
-- the last known update before enforcing the constraint.
UPDATE bookings SET completed_at = updated_at
WHERE status = 'completed' AND completed_at IS NULL;

ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN (
        'confirmed', 'in_progress', 'completion_pending', 'disputed',
        'completed', 'cancelled', 'expired'
    ));

ALTER TABLE bookings
    ADD CONSTRAINT bookings_completion_requested_check
    CHECK (status <> 'completion_pending' OR completion_requested_at IS NOT NULL);

ALTER TABLE bookings
    ADD CONSTRAINT bookings_completed_stage_check
    CHECK (status <> 'completed' OR completed_at IS NOT NULL);

-- The state-history table carries its own status allow-lists.
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

-- Services in completion or dispute still occupy their provider slot.
ALTER TABLE bookings DROP CONSTRAINT bookings_no_overlapping_active_provider_slots;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_no_overlapping_active_provider_slots
    EXCLUDE USING gist (
        provider_id WITH =,
        booking_service_slot_range(starts_at, ends_at, buffer_minutes) WITH &&
    ) WHERE (status IN ('confirmed', 'in_progress', 'completion_pending', 'disputed'));

-- System actors (dispute-window expiry job) write audit events without a user;
-- booking transitions already model actor_type 'system' with NULL actor.
ALTER TABLE audit_events ALTER COLUMN actor_user_id DROP NOT NULL;

-- One dispute per booking; an open dispute holds the booking funds in place.
CREATE TABLE booking_disputes (
    id BIGSERIAL PRIMARY KEY,
    booking_id BIGINT NOT NULL UNIQUE REFERENCES bookings(id) ON DELETE RESTRICT,
    opened_by BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    opened_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason VARCHAR(1000) NOT NULL DEFAULT '',
    resolved_at TIMESTAMPTZ,
    resolved_by BIGINT REFERENCES users(id) ON DELETE RESTRICT,
    resolution VARCHAR(16) CHECK (resolution IN ('settle', 'cancel')),
    resolution_note VARCHAR(1000) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((resolved_at IS NULL) = (resolution IS NULL)),
    CHECK (resolved_by IS NULL OR resolved_at IS NOT NULL)
);

CREATE FUNCTION enforce_booking_dispute_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'booking disputes cannot be deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'booking_disputes_immutable';
    END IF;

    IF NEW.booking_id IS DISTINCT FROM OLD.booking_id
        OR NEW.opened_by IS DISTINCT FROM OLD.opened_by
        OR NEW.opened_at IS DISTINCT FROM OLD.opened_at
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR (NEW.resolved_at IS NOT NULL AND OLD.resolved_at IS NOT NULL) THEN
        RAISE EXCEPTION 'booking dispute opening data is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'booking_dispute_opening_immutable';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER booking_disputes_transition_guard
    BEFORE UPDATE OR DELETE ON booking_disputes
    FOR EACH ROW EXECUTE FUNCTION enforce_booking_dispute_transition();

-- New ledger kinds for provider payout reservations.
ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_kind_check;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_kind_check
    CHECK (kind IN (
        'customer_available', 'customer_held', 'provider_earnings', 'provider_payout_reserved',
        'platform_commission', 'funding_clearing', 'refund_clearing', 'payout_clearing'
    ));

ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_check;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_check
    CHECK (owner_user_id IS NULL OR kind IN (
        'customer_available', 'customer_held', 'provider_earnings', 'provider_payout_reserved'
    ));

ALTER TABLE wallet_accounts DROP CONSTRAINT wallet_accounts_check1;
ALTER TABLE wallet_accounts
    ADD CONSTRAINT wallet_accounts_check1
    CHECK (owner_user_id IS NOT NULL OR kind IN (
        'platform_commission', 'funding_clearing', 'refund_clearing', 'payout_clearing'
    ));

ALTER TABLE ledger_transactions DROP CONSTRAINT ledger_transactions_kind_check;
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_kind_check
    CHECK (kind IN (
        'funding', 'hold_place', 'hold_release', 'refund_release', 'hold_settle', 'reversal',
        'payout_reserve', 'payout_release', 'payout_paid'
    ));

CREATE TABLE payout_requests (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE RESTRICT,
    amount_irr BIGINT NOT NULL CHECK (amount_irr > 0),
    status VARCHAR(16) NOT NULL DEFAULT 'requested'
        CHECK (status IN ('requested', 'approved', 'processing', 'paid', 'rejected', 'failed')),
    request_key VARCHAR(64) NOT NULL UNIQUE,
    reserve_transaction_id BIGINT NOT NULL UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    release_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    paid_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    external_transfer_reference VARCHAR(128),
    admin_note VARCHAR(1000) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (status IN ('requested', 'approved', 'processing')
            AND release_transaction_id IS NULL AND paid_transaction_id IS NULL
            AND external_transfer_reference IS NULL)
        OR (status IN ('rejected', 'failed')
            AND release_transaction_id IS NOT NULL AND paid_transaction_id IS NULL)
        OR (status = 'paid'
            AND release_transaction_id IS NULL
            AND paid_transaction_id IS NOT NULL
            AND external_transfer_reference IS NOT NULL
            AND external_transfer_reference <> '')
    )
);

-- A provider may hold at most one outstanding payout request at a time; the
-- index also blocks two concurrent reserve transactions for the same funds.
CREATE UNIQUE INDEX payout_requests_active_unique
    ON payout_requests (provider_id)
    WHERE status IN ('requested', 'approved', 'processing');

CREATE INDEX idx_payout_requests_provider ON payout_requests (provider_id, id);

CREATE FUNCTION enforce_payout_request_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payout requests cannot be deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'payout_requests_immutable';
    END IF;

    IF NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.amount_irr IS DISTINCT FROM OLD.amount_irr
        OR NEW.reserve_transaction_id IS DISTINCT FROM OLD.reserve_transaction_id
        OR NEW.request_key IS DISTINCT FROM OLD.request_key THEN
        RAISE EXCEPTION 'payout financial terms are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payout_requests_terms_immutable';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'requested' AND NEW.status IN ('approved', 'rejected')) OR
        (OLD.status = 'approved' AND NEW.status = 'processing') OR
        (OLD.status = 'processing' AND NEW.status IN ('paid', 'failed'))
    ) THEN
        RAISE EXCEPTION 'invalid payout request transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'payout_requests_state_transition_check';
    END IF;

    IF NEW.status IN ('rejected', 'failed') AND NOT EXISTS (
        SELECT 1 FROM ledger_transactions release
        WHERE release.id = NEW.release_transaction_id AND release.kind = 'payout_release'
    ) THEN
        RAISE EXCEPTION 'rejected or failed payout requires its ledger release transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'payout_release_ledger_consistency';
    END IF;

    IF NEW.status = 'paid' AND NOT EXISTS (
        SELECT 1 FROM ledger_transactions paid
        WHERE paid.id = NEW.paid_transaction_id AND paid.kind = 'payout_paid'
    ) THEN
        RAISE EXCEPTION 'paid payout requires its ledger clearing transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'payout_paid_ledger_consistency';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payout_requests_transition_guard
    BEFORE UPDATE OR DELETE ON payout_requests
    FOR EACH ROW EXECUTE FUNCTION enforce_payout_request_transition();
