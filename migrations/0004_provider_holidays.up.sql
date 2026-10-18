ALTER TABLE provider_time_off
    DROP CONSTRAINT provider_time_off_kind_check;

ALTER TABLE provider_time_off
    ADD CONSTRAINT provider_time_off_kind_check
    CHECK (kind IN ('time_off', 'blocked', 'holiday'));

ALTER TABLE provider_time_off
    ADD COLUMN holiday_date DATE;

ALTER TABLE provider_time_off
    ADD CONSTRAINT provider_time_off_holiday_date_check
    CHECK ((kind = 'holiday' AND holiday_date IS NOT NULL) OR (kind <> 'holiday' AND holiday_date IS NULL));

CREATE UNIQUE INDEX idx_provider_time_off_holiday_date
    ON provider_time_off (provider_id, holiday_date)
    WHERE kind = 'holiday';
