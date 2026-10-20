DROP INDEX IF EXISTS services_category_id_idx;

ALTER TABLE services
    DROP CONSTRAINT IF EXISTS services_category_check;

ALTER TABLE services
    DROP COLUMN IF EXISTS category;
