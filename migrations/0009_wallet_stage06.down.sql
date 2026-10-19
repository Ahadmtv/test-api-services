-- commission_policies are immutable by design (migration 0008); an applied default
-- policy remains audit history when rolling back this additive migration.
SELECT 1;
