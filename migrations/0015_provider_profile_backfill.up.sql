-- Relax the display_name check so onboarding can leave the name empty until
-- the provider completes their profile.
ALTER TABLE provider_profiles DROP CONSTRAINT provider_profiles_display_name_check;
ALTER TABLE provider_profiles ADD CONSTRAINT provider_profiles_display_name_check
    CHECK (length(btrim(display_name::text)) <= 160);

-- Backfill: every user holding the provider role must have a profile row.
-- Safe to run twice (and idempotent with future role grants).
INSERT INTO provider_profiles (provider_id, display_name, is_active, verification_status)
SELECT r.user_id, '', false, 'pending'
FROM user_roles r
WHERE r.role = 'provider'
ON CONFLICT (provider_id) DO NOTHING;
