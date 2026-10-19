CREATE TABLE payment_attempts (
    id BIGSERIAL PRIMARY KEY,
    client_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    idempotency_key VARCHAR(128) NOT NULL,
    request_fingerprint CHAR(64) NOT NULL,
    amount_irr BIGINT NOT NULL CHECK (amount_irr >= 10000),
    callback_url TEXT NOT NULL,
    status VARCHAR(24) NOT NULL CHECK (status IN ('created', 'pending', 'succeeded', 'failed', 'verification_unknown')),
    authority VARCHAR(64) UNIQUE,
    gateway_reference_id VARCHAR(128),
    gateway_code VARCHAR(64),
    callback_status VARCHAR(32),
    ledger_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    request_id VARCHAR(128) NOT NULL,
    request_started_at TIMESTAMPTZ NOT NULL,
    verification_started_at TIMESTAMPTZ,
    verification_attempts INTEGER NOT NULL DEFAULT 0 CHECK (verification_attempts >= 0),
    succeeded_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (client_id, idempotency_key),
    CHECK ((status = 'succeeded') = (ledger_transaction_id IS NOT NULL)),
    CHECK ((status = 'succeeded') = (succeeded_at IS NOT NULL)),
    CHECK (status <> 'succeeded' OR (authority IS NOT NULL AND gateway_reference_id IS NOT NULL AND gateway_code IN ('100', '101')))
);

CREATE INDEX idx_payment_attempts_unknown ON payment_attempts (updated_at, id)
    WHERE status = 'verification_unknown' AND authority IS NOT NULL;
CREATE INDEX idx_payment_attempts_client ON payment_attempts (client_id, id DESC);

CREATE FUNCTION guard_payment_attempt_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment attempts cannot be deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_immutable';
    END IF;
    IF NEW.client_id IS DISTINCT FROM OLD.client_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.request_fingerprint IS DISTINCT FROM OLD.request_fingerprint
        OR NEW.amount_irr IS DISTINCT FROM OLD.amount_irr
        OR NEW.callback_url IS DISTINCT FROM OLD.callback_url
        OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.request_started_at IS DISTINCT FROM OLD.request_started_at THEN
        RAISE EXCEPTION 'payment request terms are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_terms_immutable';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'created' AND NEW.status IN ('pending', 'failed', 'verification_unknown')) OR
        (OLD.status = 'pending' AND NEW.status IN ('failed', 'succeeded', 'verification_unknown')) OR
        (OLD.status = 'verification_unknown' AND NEW.status IN ('failed', 'succeeded', 'pending'))
    ) THEN
        RAISE EXCEPTION 'invalid payment state transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_state_transition';
    END IF;
    IF OLD.authority IS NOT NULL AND NEW.authority IS DISTINCT FROM OLD.authority THEN
        RAISE EXCEPTION 'payment authority is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_authority_immutable';
    END IF;
    IF OLD.gateway_reference_id IS NOT NULL AND NEW.gateway_reference_id IS DISTINCT FROM OLD.gateway_reference_id THEN
        RAISE EXCEPTION 'payment reference is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_reference_immutable';
    END IF;
    IF OLD.ledger_transaction_id IS NOT NULL AND NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id THEN
        RAISE EXCEPTION 'payment ledger transaction is immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_attempts_ledger_immutable';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_attempts_guard
    BEFORE UPDATE OR DELETE ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION guard_payment_attempt_mutation();

CREATE FUNCTION enforce_payment_ledger_consistency() RETURNS trigger AS $$
DECLARE
    posting_reference TEXT;
    debit_amount NUMERIC;
    credit_amount NUMERIC;
    entry_count BIGINT;
BEGIN
    IF NEW.status <> 'succeeded' THEN
        RETURN NULL;
    END IF;
    SELECT t.reference,
           COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'funding_clearing' AND e.side = 'debit'), 0),
           COALESCE(SUM(e.amount_irr) FILTER (WHERE a.kind = 'customer_available' AND a.owner_user_id = NEW.client_id AND e.side = 'credit'), 0),
           COUNT(*)
    INTO posting_reference, debit_amount, credit_amount, entry_count
    FROM ledger_transactions t
    JOIN ledger_entries e ON e.transaction_id = t.id
    JOIN wallet_accounts a ON a.id = e.account_id
    WHERE t.id = NEW.ledger_transaction_id AND t.kind = 'funding'
    GROUP BY t.reference;
    IF posting_reference IS DISTINCT FROM 'payment:' || NEW.id || ':funding'
        OR entry_count <> 2 OR debit_amount <> NEW.amount_irr OR credit_amount <> NEW.amount_irr THEN
        RAISE EXCEPTION 'payment does not match its wallet funding transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'payment_ledger_consistency';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER payment_attempts_ledger_consistency
    AFTER INSERT OR UPDATE ON payment_attempts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_payment_ledger_consistency();
