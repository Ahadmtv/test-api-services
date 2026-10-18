CREATE FUNCTION prevent_provider_timezone_change_with_bookings() RETURNS trigger AS $$
BEGIN
    IF OLD.time_zone IS DISTINCT FROM NEW.time_zone AND EXISTS (
        SELECT 1
        FROM bookings
        WHERE provider_id = NEW.provider_id
          AND status IN ('confirmed', 'in_progress')
          AND ends_at + buffer_minutes * INTERVAL '1 minute' > now()
    ) THEN
        RAISE EXCEPTION 'provider timezone cannot change while active bookings exist'
            USING ERRCODE = '23514', CONSTRAINT = 'provider_profiles_time_zone_booking_guard';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER provider_profiles_time_zone_booking_guard
    BEFORE UPDATE OF time_zone ON provider_profiles
    FOR EACH ROW EXECUTE FUNCTION prevent_provider_timezone_change_with_bookings();
