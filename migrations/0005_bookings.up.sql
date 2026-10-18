CREATE EXTENSION IF NOT EXISTS btree_gist;

ALTER TABLE services
    ADD CONSTRAINT services_provider_id_id_key UNIQUE (provider_id, id);

CREATE TABLE bookings (
    id BIGSERIAL PRIMARY KEY,
    client_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE RESTRICT,
    service_id BIGINT NOT NULL,
    service_name VARCHAR(160) NOT NULL,
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    duration_minutes INTEGER NOT NULL CHECK (duration_minutes BETWEEN 1 AND 1440),
    price_irr BIGINT NOT NULL CHECK (price_irr >= 0),
    buffer_minutes INTEGER NOT NULL CHECK (buffer_minutes BETWEEN 0 AND 1440),
    status VARCHAR(32) NOT NULL DEFAULT 'pending_provider_acceptance'
        CHECK (status IN ('pending_provider_acceptance', 'confirmed', 'in_progress', 'completed', 'cancelled', 'expired')),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (starts_at < ends_at),
    CHECK (ends_at = starts_at + duration_minutes * INTERVAL '1 minute'),
    CONSTRAINT bookings_service_provider_fk FOREIGN KEY (provider_id, service_id)
        REFERENCES services(provider_id, id) ON DELETE RESTRICT,
    CONSTRAINT bookings_no_overlapping_active_provider_slots
        EXCLUDE USING gist (
            provider_id WITH =,
            tstzrange(starts_at, ends_at + buffer_minutes * INTERVAL '1 minute', '[)') WITH &&
        ) WHERE (status IN ('pending_provider_acceptance', 'confirmed', 'in_progress'))
);

CREATE INDEX idx_bookings_client_id ON bookings (client_id, id);
CREATE INDEX idx_bookings_provider_id ON bookings (provider_id, id);
CREATE INDEX idx_bookings_pending_expiration ON bookings (expires_at, id)
    WHERE status = 'pending_provider_acceptance';

CREATE TABLE booking_state_transitions (
    id BIGSERIAL PRIMARY KEY,
    booking_id BIGINT NOT NULL REFERENCES bookings(id) ON DELETE RESTRICT,
    from_status VARCHAR(32),
    to_status VARCHAR(32) NOT NULL,
    actor_type VARCHAR(8) NOT NULL CHECK (actor_type IN ('user', 'system')),
    actor_user_id BIGINT REFERENCES users(id) ON DELETE RESTRICT,
    request_id VARCHAR(128) NOT NULL DEFAULT '',
    reason VARCHAR(96) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (from_status IS NULL OR from_status IN ('pending_provider_acceptance', 'confirmed', 'in_progress', 'completed', 'cancelled', 'expired')),
    CHECK (to_status IN ('pending_provider_acceptance', 'confirmed', 'in_progress', 'completed', 'cancelled', 'expired')),
    CHECK ((actor_type = 'user' AND actor_user_id IS NOT NULL) OR (actor_type = 'system' AND actor_user_id IS NULL))
);

CREATE INDEX idx_booking_state_transitions_booking_id ON booking_state_transitions (booking_id, id);

CREATE FUNCTION enforce_booking_state_transition() RETURNS trigger AS $$
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

CREATE TRIGGER bookings_state_transition_guard
    BEFORE INSERT OR UPDATE OF status ON bookings
    FOR EACH ROW EXECUTE FUNCTION enforce_booking_state_transition();

CREATE FUNCTION audit_booking_state_transition() RETURNS trigger AS $$
DECLARE
    actor_id BIGINT;
    actor_kind TEXT;
    request_identifier TEXT;
BEGIN
    actor_id := NULLIF(current_setting('app.actor_user_id', TRUE), '')::BIGINT;
    actor_kind := COALESCE(NULLIF(current_setting('app.actor_type', TRUE), ''), 'system');
    request_identifier := COALESCE(NULLIF(current_setting('app.request_id', TRUE), ''), '');

    IF TG_OP = 'INSERT' THEN
        INSERT INTO booking_state_transitions (booking_id, from_status, to_status, actor_type, actor_user_id, request_id, reason)
        VALUES (NEW.id, NULL, NEW.status, actor_kind, actor_id, request_identifier, 'created');
    ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
        INSERT INTO booking_state_transitions (booking_id, from_status, to_status, actor_type, actor_user_id, request_id, reason)
        VALUES (NEW.id, OLD.status, NEW.status, actor_kind, actor_id, request_identifier, COALESCE(NULLIF(current_setting('app.transition_reason', TRUE), ''), ''));
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bookings_state_transition_audit
    AFTER INSERT OR UPDATE OF status ON bookings
    FOR EACH ROW EXECUTE FUNCTION audit_booking_state_transition();

CREATE FUNCTION prevent_booking_transition_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'booking state transitions are immutable'
        USING ERRCODE = '23514', CONSTRAINT = 'booking_state_transitions_immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER booking_state_transitions_immutable
    BEFORE UPDATE OR DELETE ON booking_state_transitions
    FOR EACH ROW EXECUTE FUNCTION prevent_booking_transition_mutation();

CREATE TABLE idempotency_records (
    id BIGSERIAL PRIMARY KEY,
    principal_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    operation VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    request_fingerprint CHAR(64) NOT NULL,
    booking_id BIGINT REFERENCES bookings(id) ON DELETE RESTRICT,
    result JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (principal_id, operation, idempotency_key),
    CHECK (jsonb_typeof(result) = 'object')
);
