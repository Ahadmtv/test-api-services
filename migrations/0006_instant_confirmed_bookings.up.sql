SELECT set_config('app.actor_type', 'system', true),
       set_config('app.actor_user_id', '', true),
       set_config('app.request_id', '', true),
       set_config('app.transition_reason', 'instant_booking_policy_migration', true);

UPDATE bookings
SET status = 'cancelled', updated_at = now()
WHERE status = 'pending_provider_acceptance';

-- Fire any pending deferrable trigger events before the ALTER TABLE statements;
-- during a clean upgrade there is nothing to fire and this is a no-op.
SET CONSTRAINTS ALL IMMEDIATE;

DROP INDEX idx_bookings_pending_expiration;
ALTER TABLE bookings RENAME COLUMN expires_at TO legacy_expires_at;
ALTER TABLE bookings ALTER COLUMN legacy_expires_at DROP NOT NULL;
ALTER TABLE bookings ALTER COLUMN status DROP DEFAULT;
ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN ('confirmed', 'in_progress', 'completed', 'cancelled', 'expired'));
ALTER TABLE bookings ALTER COLUMN status SET DEFAULT 'confirmed';

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
