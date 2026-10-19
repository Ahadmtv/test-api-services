ALTER TABLE bookings
    ADD COLUMN payment_method VARCHAR(16) NOT NULL DEFAULT 'none'
        CHECK (payment_method IN ('none', 'wallet'));

CREATE OR REPLACE FUNCTION enforce_booking_state_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'confirmed' THEN
            RAISE EXCEPTION 'new bookings must start confirmed'
                USING ERRCODE = '23514', CONSTRAINT = 'bookings_initial_status_check';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF OLD.status = 'confirmed' AND NEW.status = 'in_progress' AND OLD.starts_at > now() THEN
        RAISE EXCEPTION 'service cannot start before its scheduled time'
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_start_deadline_check';
    END IF;

    IF NOT (
        (OLD.status = 'confirmed' AND NEW.status IN ('in_progress', 'cancelled')) OR
        (OLD.status = 'in_progress' AND NEW.status = 'completed')
    ) THEN
        RAISE EXCEPTION 'invalid booking state transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'bookings_state_transition_check';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE commission_policies (
    version INTEGER PRIMARY KEY CHECK (version > 0),
    rate_bps INTEGER NOT NULL CHECK (rate_bps BETWEEN 0 AND 10000),
    rounding VARCHAR(16) NOT NULL DEFAULT 'floor' CHECK (rounding = 'floor'),
    effective_from TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE wallet_accounts (
    id BIGSERIAL PRIMARY KEY,
    owner_user_id BIGINT REFERENCES users(id) ON DELETE RESTRICT,
    owner_key BIGINT GENERATED ALWAYS AS (COALESCE(owner_user_id, 0)) STORED,
    kind VARCHAR(32) NOT NULL CHECK (kind IN (
        'customer_available', 'customer_held', 'provider_earnings',
        'platform_commission', 'funding_clearing', 'refund_clearing'
    )),
    normal_side VARCHAR(6) NOT NULL CHECK (normal_side IN ('debit', 'credit')),
    balance_irr BIGINT NOT NULL DEFAULT 0 CHECK (balance_irr >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (owner_user_id IS NULL OR kind IN ('customer_available', 'customer_held', 'provider_earnings')),
    CHECK (owner_user_id IS NOT NULL OR kind IN ('platform_commission', 'funding_clearing', 'refund_clearing')),
    CHECK ((normal_side = 'debit') = (kind = 'funding_clearing')),
    UNIQUE (owner_key, kind)
);

CREATE TABLE ledger_transactions (
    id BIGSERIAL PRIMARY KEY,
    reference VARCHAR(160) NOT NULL UNIQUE,
    kind VARCHAR(32) NOT NULL CHECK (kind IN ('funding', 'hold_place', 'hold_release', 'refund_release', 'hold_settle', 'reversal')),
    reverses_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    actor_user_id BIGINT REFERENCES users(id) ON DELETE RESTRICT,
    request_id VARCHAR(128) NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'reversal') = (reverses_transaction_id IS NOT NULL))
);

CREATE TABLE ledger_entries (
    id BIGSERIAL PRIMARY KEY,
    transaction_id BIGINT NOT NULL REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    account_id BIGINT NOT NULL REFERENCES wallet_accounts(id) ON DELETE RESTRICT,
    side VARCHAR(6) NOT NULL CHECK (side IN ('debit', 'credit')),
    amount_irr BIGINT NOT NULL CHECK (amount_irr > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_ledger_entries_transaction ON ledger_entries (transaction_id);
CREATE INDEX idx_ledger_entries_account ON ledger_entries (account_id, id);

CREATE TABLE wallet_holds (
    id BIGSERIAL PRIMARY KEY,
    booking_id BIGINT NOT NULL UNIQUE REFERENCES bookings(id) ON DELETE RESTRICT,
    customer_id BIGINT NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    provider_id BIGINT NOT NULL REFERENCES provider_profiles(provider_id) ON DELETE RESTRICT,
    amount_irr BIGINT NOT NULL CHECK (amount_irr > 0),
    commission_policy_version INTEGER NOT NULL REFERENCES commission_policies(version) ON DELETE RESTRICT,
    commission_rate_bps INTEGER NOT NULL CHECK (commission_rate_bps BETWEEN 0 AND 10000),
    status VARCHAR(16) NOT NULL CHECK (status IN ('active', 'released', 'settled', 'refunded')),
    place_transaction_id BIGINT NOT NULL UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    closing_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    refund_transaction_id BIGINT UNIQUE REFERENCES ledger_transactions(id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((status = 'active') = (closing_transaction_id IS NULL)),
    CHECK ((status = 'refunded') = (refund_transaction_id IS NOT NULL))
);

CREATE INDEX idx_wallet_holds_active_customer ON wallet_holds (customer_id) WHERE status = 'active';

CREATE FUNCTION prevent_wallet_record_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable', TG_TABLE_NAME
        USING ERRCODE = '23514', CONSTRAINT = 'wallet_records_immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_transactions_immutable
    BEFORE UPDATE OR DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION prevent_wallet_record_mutation();

CREATE TRIGGER ledger_entries_immutable
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION prevent_wallet_record_mutation();

CREATE TRIGGER commission_policies_immutable
    BEFORE UPDATE OR DELETE ON commission_policies
    FOR EACH ROW EXECUTE FUNCTION prevent_wallet_record_mutation();

CREATE FUNCTION enforce_ledger_entry_same_transaction() RETURNS trigger AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM ledger_transactions
        WHERE id = NEW.transaction_id AND created_at = now()
    ) THEN
        RAISE EXCEPTION 'ledger entries must be posted in the transaction that creates their ledger transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_entries_same_transaction';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_same_transaction_guard
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION enforce_ledger_entry_same_transaction();

CREATE FUNCTION guard_wallet_account_mutation() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.balance_irr <> 0 THEN
            RAISE EXCEPTION 'wallet accounts must start with a zero balance'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_accounts_ledger_only';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallet accounts can only change through ledger entries'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_accounts_ledger_only';
    END IF;
    IF NEW.owner_user_id IS DISTINCT FROM OLD.owner_user_id
        OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.normal_side IS DISTINCT FROM OLD.normal_side
        OR (NEW.balance_irr IS DISTINCT FROM OLD.balance_irr AND pg_trigger_depth() < 2) THEN
        RAISE EXCEPTION 'wallet accounts can only change through ledger entries'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_accounts_ledger_only';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_accounts_ledger_only
    BEFORE INSERT OR UPDATE OR DELETE ON wallet_accounts
    FOR EACH ROW EXECUTE FUNCTION guard_wallet_account_mutation();

CREATE FUNCTION apply_ledger_entry_to_balance() RETURNS trigger AS $$
BEGIN
    UPDATE wallet_accounts
    SET balance_irr = balance_irr + CASE WHEN normal_side = NEW.side THEN NEW.amount_irr ELSE -NEW.amount_irr END,
        updated_at = now()
    WHERE id = NEW.account_id;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_apply_balance
    AFTER INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION apply_ledger_entry_to_balance();

CREATE FUNCTION enforce_ledger_transaction_balance() RETURNS trigger AS $$
DECLARE
    target_transaction_id BIGINT;
    debit_total NUMERIC;
    credit_total NUMERIC;
    entry_count BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'ledger_transactions' THEN
        target_transaction_id := NEW.id;
    ELSE
        target_transaction_id := NEW.transaction_id;
    END IF;

    SELECT COALESCE(SUM(amount_irr) FILTER (WHERE side = 'debit'), 0),
           COALESCE(SUM(amount_irr) FILTER (WHERE side = 'credit'), 0),
           COUNT(*)
    INTO debit_total, credit_total, entry_count
    FROM ledger_entries
    WHERE transaction_id = target_transaction_id;

    IF entry_count < 2 OR debit_total <> credit_total THEN
        RAISE EXCEPTION 'ledger transaction % is not balanced', target_transaction_id
            USING ERRCODE = '23514', CONSTRAINT = 'ledger_transactions_balanced';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ledger_transactions_balanced_check
    AFTER INSERT ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_ledger_transaction_balance();

CREATE CONSTRAINT TRIGGER ledger_entries_balanced_check
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_ledger_transaction_balance();

CREATE FUNCTION enforce_wallet_hold_transition() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wallet holds cannot be deleted'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_holds_immutable';
    END IF;

    IF NEW.booking_id IS DISTINCT FROM OLD.booking_id
        OR NEW.customer_id IS DISTINCT FROM OLD.customer_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.amount_irr IS DISTINCT FROM OLD.amount_irr
        OR NEW.commission_policy_version IS DISTINCT FROM OLD.commission_policy_version
        OR NEW.commission_rate_bps IS DISTINCT FROM OLD.commission_rate_bps
        OR NEW.place_transaction_id IS DISTINCT FROM OLD.place_transaction_id
        OR (NEW.status = OLD.status AND (
            NEW.closing_transaction_id IS DISTINCT FROM OLD.closing_transaction_id
            OR NEW.refund_transaction_id IS DISTINCT FROM OLD.refund_transaction_id
        ))
        OR (OLD.status = 'refunded' AND (
            NEW.closing_transaction_id IS DISTINCT FROM OLD.closing_transaction_id
            OR NEW.refund_transaction_id IS DISTINCT FROM OLD.refund_transaction_id
        )) THEN
        RAISE EXCEPTION 'wallet hold financial terms are immutable'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_holds_terms_immutable';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'active' AND NEW.status IN ('released', 'settled')) OR
        (OLD.status = 'settled' AND NEW.status = 'refunded')
    ) THEN
        RAISE EXCEPTION 'invalid wallet hold transition: % -> %', OLD.status, NEW.status
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_holds_state_transition_check';
    END IF;

    IF NEW.status = 'released' AND NOT EXISTS (
        SELECT 1 FROM ledger_transactions release
        WHERE release.id = NEW.closing_transaction_id AND release.kind = 'hold_release'
    ) THEN
        RAISE EXCEPTION 'released hold requires its ledger release transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_hold_release_ledger_consistency';
    END IF;
    IF NEW.status = 'settled' AND NOT EXISTS (
        SELECT 1 FROM ledger_transactions settlement
        WHERE settlement.id = NEW.closing_transaction_id AND settlement.kind = 'hold_settle'
    ) THEN
        RAISE EXCEPTION 'settled hold requires its settlement transaction'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_hold_settle_ledger_consistency';
    END IF;

    IF NEW.status = 'refunded' AND OLD.status <> 'refunded' AND (
        NOT EXISTS (
            SELECT 1 FROM ledger_transactions reversal
            WHERE reversal.id = NEW.refund_transaction_id
              AND reversal.kind = 'reversal'
              AND reversal.reverses_transaction_id = OLD.closing_transaction_id
        ) OR NOT EXISTS (
            SELECT 1 FROM ledger_transactions release
            WHERE release.id = NEW.closing_transaction_id
              AND release.kind = 'refund_release'
        )
    ) THEN
        RAISE EXCEPTION 'refunded hold requires a settlement reversal and hold release'
            USING ERRCODE = '23514', CONSTRAINT = 'wallet_hold_refund_ledger_consistency';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_holds_transition_guard
    BEFORE UPDATE OR DELETE ON wallet_holds
    FOR EACH ROW EXECUTE FUNCTION enforce_wallet_hold_transition();

CREATE FUNCTION enforce_booking_wallet_consistency() RETURNS trigger AS $$
DECLARE
    target_booking_id BIGINT;
    booking_status VARCHAR(32);
    booking_payment VARCHAR(16);
    booking_client_id BIGINT;
    booking_provider_id BIGINT;
    booking_amount_irr BIGINT;
    hold_state VARCHAR(16);
BEGIN
    IF TG_TABLE_NAME = 'bookings' THEN
        target_booking_id := NEW.id;
    ELSE
        target_booking_id := NEW.booking_id;
    END IF;

    SELECT b.status, b.payment_method, b.client_id, b.provider_id, b.price_irr, h.status
    INTO booking_status, booking_payment, booking_client_id, booking_provider_id, booking_amount_irr, hold_state
    FROM bookings b
    LEFT JOIN wallet_holds h ON h.booking_id = b.id
    WHERE b.id = target_booking_id;

    IF booking_payment = 'none' THEN
        IF hold_state IS NOT NULL THEN
            RAISE EXCEPTION 'booking without wallet payment cannot have a wallet hold'
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    ELSE
        IF hold_state IS NULL OR NOT EXISTS (
            SELECT 1 FROM wallet_holds h
            WHERE h.booking_id = target_booking_id
              AND h.customer_id = booking_client_id
              AND h.provider_id = booking_provider_id
              AND h.amount_irr = booking_amount_irr
        ) OR NOT (
            (booking_status IN ('confirmed', 'in_progress') AND hold_state = 'active') OR
            (booking_status = 'cancelled' AND hold_state = 'released') OR
            (booking_status = 'completed' AND hold_state IN ('settled', 'refunded'))
        ) THEN
            RAISE EXCEPTION 'wallet hold state % does not match booking status %', hold_state, booking_status
                USING ERRCODE = '23514', CONSTRAINT = 'wallet_booking_hold_consistency';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER bookings_wallet_consistency_check
    AFTER INSERT OR UPDATE OF status, payment_method, client_id, provider_id, price_irr ON bookings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_booking_wallet_consistency();

CREATE CONSTRAINT TRIGGER wallet_holds_booking_consistency_check
    AFTER INSERT OR UPDATE ON wallet_holds
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION enforce_booking_wallet_consistency();
