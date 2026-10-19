CREATE OR REPLACE FUNCTION enforce_booking_state_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'pending_provider_acceptance' THEN
            RAISE EXCEPTION 'bookings must start in pending_provider_acceptance'
                USING ERRCODE = '23514', CONSTRAINT = 'bookings_initial_status_check';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF OLD.status = 'pending_provider_acceptance' AND NEW.status = 'expired' AND OLD.expires_at > now() THEN
        RAISE EXCEPTION 'pending booking cannot expire before expires_at'
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_expiration_deadline_check';
    END IF;
    IF OLD.status = 'pending_provider_acceptance' AND NEW.status = 'confirmed' AND OLD.expires_at <= now() THEN
        RAISE EXCEPTION 'expired pending booking cannot be accepted'
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_acceptance_deadline_check';
    END IF;

    IF NOT (
        (OLD.status = 'pending_provider_acceptance' AND NEW.status IN ('confirmed', 'cancelled', 'expired')) OR
        (OLD.status = 'confirmed' AND NEW.status IN ('in_progress', 'cancelled'))
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
    ) WHERE (status IN ('pending_provider_acceptance', 'confirmed', 'in_progress'));

ALTER TABLE bookings DROP CONSTRAINT bookings_status_check;
ALTER TABLE bookings
    ADD CONSTRAINT bookings_status_check
    CHECK (status IN ('pending_provider_acceptance', 'confirmed', 'in_progress', 'completed', 'cancelled', 'expired'));
ALTER TABLE bookings ALTER COLUMN status DROP DEFAULT;
ALTER TABLE bookings ALTER COLUMN status SET DEFAULT 'pending_provider_acceptance';

UPDATE bookings SET legacy_expires_at = starts_at WHERE legacy_expires_at IS NULL;
ALTER TABLE bookings ALTER COLUMN legacy_expires_at SET NOT NULL;
ALTER TABLE bookings RENAME COLUMN legacy_expires_at TO expires_at;
CREATE INDEX idx_bookings_pending_expiration ON bookings (expires_at, id)
    WHERE status = 'pending_provider_acceptance';
