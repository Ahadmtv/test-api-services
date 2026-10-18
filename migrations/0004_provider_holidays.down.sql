UPDATE provider_time_off
SET kind = 'time_off', holiday_date = NULL
WHERE kind = 'holiday';

DROP INDEX IF EXISTS idx_provider_time_off_holiday_date;

ALTER TABLE provider_time_off
    DROP CONSTRAINT provider_time_off_holiday_date_check;

ALTER TABLE provider_time_off
    DROP COLUMN holiday_date;

ALTER TABLE provider_time_off
    DROP CONSTRAINT provider_time_off_kind_check;

ALTER TABLE provider_time_off
    ADD CONSTRAINT provider_time_off_kind_check
    CHECK (kind IN ('time_off', 'blocked'));
