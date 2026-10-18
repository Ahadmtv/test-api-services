CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;

CREATE TABLE audit_events (
    id BIGSERIAL PRIMARY KEY,
    actor_user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    action VARCHAR(96) NOT NULL,
    target_type VARCHAR(64) NOT NULL,
    target_id BIGINT NOT NULL,
    request_id VARCHAR(128) NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_events_actor_created ON audit_events (actor_user_id, created_at DESC);
CREATE INDEX idx_audit_events_target_created ON audit_events (target_type, target_id, created_at DESC);

CREATE TABLE provider_profiles (
    provider_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    display_name VARCHAR(160) NOT NULL CHECK (length(btrim(display_name)) BETWEEN 1 AND 160),
    description TEXT NOT NULL DEFAULT '',
    time_zone VARCHAR(64) NOT NULL DEFAULT 'Asia/Tehran',
    is_active BOOLEAN NOT NULL DEFAULT FALSE,
    verification_status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (verification_status IN ('pending', 'verified', 'rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_provider_profiles_public
    ON provider_profiles (is_active, verification_status, provider_id);

CREATE TABLE services (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE RESTRICT,
    name VARCHAR(160) NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 160),
    description TEXT NOT NULL DEFAULT '',
    duration_minutes INTEGER NOT NULL CHECK (duration_minutes BETWEEN 1 AND 1440),
    price_irr BIGINT NOT NULL CHECK (price_irr >= 0),
    buffer_minutes INTEGER NOT NULL DEFAULT 0 CHECK (buffer_minutes BETWEEN 0 AND 1440),
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_services_provider ON services (provider_id, id);
CREATE INDEX idx_services_public ON services (is_active, provider_id, id);

CREATE TABLE weekly_working_hours (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE CASCADE,
    weekday SMALLINT NOT NULL CHECK (weekday BETWEEN 1 AND 7),
    opens_at TIME WITHOUT TIME ZONE NOT NULL,
    closes_at TIME WITHOUT TIME ZONE NOT NULL,
    CHECK (opens_at < closes_at),
    EXCLUDE USING gist (
        provider_id WITH =,
        weekday WITH =,
        tsrange(DATE '2000-01-01' + opens_at, DATE '2000-01-01' + closes_at, '[)') WITH &&
    )
);

CREATE INDEX idx_weekly_working_hours_provider_day
    ON weekly_working_hours (provider_id, weekday, opens_at);

CREATE TABLE provider_time_off (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE CASCADE,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('time_off', 'blocked')),
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,
    reason VARCHAR(500) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (starts_at < ends_at),
    EXCLUDE USING gist (
        provider_id WITH =,
        tstzrange(starts_at, ends_at, '[)') WITH &&
    )
);

CREATE INDEX idx_provider_time_off_provider_start
    ON provider_time_off (provider_id, starts_at);
