-- Stage 11: classify provider services by beauty-service category.
-- Existing records are not reinterpreted: ambiguous or legacy services remain
-- in the explicit "other" category until the provider edits them.

ALTER TABLE services
    ADD COLUMN category VARCHAR(32);

UPDATE services
SET category = 'other'
WHERE category IS NULL;

ALTER TABLE services
    ALTER COLUMN category SET DEFAULT 'other',
    ALTER COLUMN category SET NOT NULL;

ALTER TABLE services
    ADD CONSTRAINT services_category_check
    CHECK (category IN (
        'haircut', 'hair-color', 'hair-care', 'makeup', 'eyebrow-lash',
        'nails', 'skin-care', 'waxing', 'massage', 'spa', 'bridal',
        'barbering', 'other'
    ));

CREATE INDEX services_category_id_idx ON services (category, id);
