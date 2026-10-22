DELETE FROM provider_profiles
WHERE btrim(display_name) = ''
  AND verification_status = 'pending'
  AND NOT EXISTS (SELECT 1 FROM services s WHERE s.provider_id = provider_profiles.provider_id)
  AND NOT EXISTS (SELECT 1 FROM provider_time_off t WHERE t.provider_id = provider_profiles.provider_id)
  AND NOT EXISTS (SELECT 1 FROM weekly_working_hours w WHERE w.provider_id = provider_profiles.provider_id)
  AND NOT EXISTS (SELECT 1 FROM wallet_holds h WHERE h.provider_id = provider_profiles.provider_id)
  AND NOT EXISTS (SELECT 1 FROM payout_requests p WHERE p.provider_id = provider_profiles.provider_id);

ALTER TABLE provider_profiles DROP CONSTRAINT provider_profiles_display_name_check;
ALTER TABLE provider_profiles ADD CONSTRAINT provider_profiles_display_name_check
    CHECK (length(btrim(display_name::text)) >= 1 AND length(btrim(display_name::text)) <= 160);
