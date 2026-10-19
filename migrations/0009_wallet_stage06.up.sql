-- Stage 06 needs a commission snapshot for every wallet hold. Product pricing is not
-- specified yet, therefore seed only an absent policy with an effective zero rate.
INSERT INTO commission_policies (version, rate_bps, rounding, effective_from)
SELECT 1, 0, 'floor', now()
WHERE NOT EXISTS (SELECT 1 FROM commission_policies);
