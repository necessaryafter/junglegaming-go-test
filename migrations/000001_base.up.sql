CREATE TABLE wallets (
    id UUID PRIMARY KEY,
    player_id UUID NOT NULL,
    currency TEXT NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    balance BIGINT NOT NULL CHECK (balance >= 0),
    version BIGINT NOT NULL CHECK (version >= 1),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    UNIQUE (player_id, currency),
    UNIQUE (id, currency),
    UNIQUE (id, player_id, currency)
);

CREATE TABLE wager_transactions (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL,
    player_id UUID NOT NULL,
    currency TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('OPENING','BET','WIN','LOSS','REFUND','ROLLBACK')),
    provider_id TEXT,
    external_id TEXT,
    idempotency_key TEXT,
    payload_hash TEXT,
    request JSONB,
    reference_id UUID REFERENCES wager_transactions(id),
    status TEXT NOT NULL CHECK (status IN ('PENDING','PENDING_REFERENCE','PROCESSED','REJECTED','FAILED')),
    failure_code TEXT NOT NULL DEFAULT '',
    result_balance BIGINT NOT NULL CHECK (result_balance >= 0),
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL CHECK (updated_at >= created_at),
    FOREIGN KEY (wallet_id, player_id, currency) REFERENCES wallets(id, player_id, currency),
    UNIQUE (provider_id, external_id),
    UNIQUE (provider_id, idempotency_key),
    UNIQUE (id, wallet_id, currency),
    CHECK ((status IN ('REJECTED','FAILED')) = (failure_code <> '')),
    CHECK ((kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)),
    CHECK (
      (kind = 'OPENING' AND provider_id IS NULL AND external_id IS NULL AND
       idempotency_key IS NULL AND payload_hash IS NULL AND request IS NULL AND
       reference_id IS NULL AND status = 'PROCESSED') OR
      (kind <> 'OPENING' AND provider_id IS NOT NULL AND provider_id <> '' AND
       external_id IS NOT NULL AND external_id <> '' AND idempotency_key IS NOT NULL AND
       idempotency_key <> '' AND payload_hash IS NOT NULL AND length(payload_hash) = 64 AND
       request IS NOT NULL)
    )
);

CREATE UNIQUE INDEX one_opening ON wager_transactions(wallet_id) WHERE kind = 'OPENING';
-- Conservative reversal policy: REFUND and ROLLBACK compete for one reference.
CREATE UNIQUE INDEX one_reversal ON wager_transactions(reference_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND','ROLLBACK');
CREATE INDEX pending_references ON wager_transactions(next_attempt_at)
    WHERE status = 'PENDING_REFERENCE';

CREATE TABLE wallet_ledger (
    id UUID PRIMARY KEY,
    wallet_id UUID NOT NULL,
    transaction_id UUID NOT NULL,
    currency TEXT NOT NULL,
    direction TEXT NOT NULL CHECK (direction IN ('DEBIT','CREDIT')),
    amount BIGINT NOT NULL CHECK (amount > 0),
    balance_before BIGINT NOT NULL CHECK (balance_before >= 0),
    balance_after BIGINT NOT NULL CHECK (balance_after >= 0),
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (wallet_id, transaction_id),
    FOREIGN KEY (transaction_id, wallet_id, currency) REFERENCES wager_transactions(id, wallet_id, currency),
    CHECK (balance_after::numeric = balance_before::numeric +
        CASE direction WHEN 'CREDIT' THEN amount::numeric ELSE -amount::numeric END)
);
CREATE INDEX ledger_pagination ON wallet_ledger(wallet_id, created_at, id);

CREATE FUNCTION reject_financial_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER ledger_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON wallet_ledger
    FOR EACH STATEMENT EXECUTE FUNCTION reject_financial_mutation();

-- Deferred checks verify the final state, after the entire unit of work.
CREATE FUNCTION check_wallet_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    target UUID;
    stored wallets%ROWTYPE;
    calculated NUMERIC;
    changes BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN target := NEW.id; ELSE target := NEW.wallet_id; END IF;
    SELECT * INTO stored FROM wallets WHERE id = target FOR UPDATE;
    SELECT COALESCE(sum(CASE l.direction WHEN 'CREDIT' THEN l.amount::numeric ELSE -l.amount::numeric END), 0),
           count(*) FILTER (WHERE t.kind <> 'OPENING')
    INTO calculated, changes FROM wallet_ledger l
    JOIN wager_transactions t ON t.id = l.transaction_id WHERE l.wallet_id = target;
    IF stored.balance::numeric <> calculated OR stored.version <> changes + 1 THEN
        RAISE EXCEPTION 'wallet and ledger disagree' USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM wallet_ledger l JOIN wager_transactions t ON t.id = l.transaction_id
        WHERE l.wallet_id = target AND (t.status <> 'PROCESSED' OR t.kind = 'LOSS' OR t.amount <> l.amount OR
          (l.direction = 'CREDIT' AND t.kind = 'BET') OR
          (l.direction = 'DEBIT' AND t.kind IN ('OPENING','WIN','REFUND')))
    ) THEN
        RAISE EXCEPTION 'ledger and transaction disagree' USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
        SELECT 1 FROM wager_transactions t LEFT JOIN wallet_ledger l ON l.transaction_id = t.id
        WHERE t.wallet_id = target AND t.status = 'PROCESSED' AND t.kind <> 'LOSS' AND l.id IS NULL
    ) THEN
        RAISE EXCEPTION 'processed movement requires a ledger entry' USING ERRCODE = '23514';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER wallet_ledger_consistency AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_ledger();
CREATE CONSTRAINT TRIGGER ledger_wallet_consistency AFTER INSERT ON wallet_ledger
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_ledger();
CREATE CONSTRAINT TRIGGER transaction_ledger_consistency AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_wallet_ledger();

CREATE FUNCTION protect_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED','REJECTED','FAILED') THEN
        RAISE EXCEPTION 'terminal transaction is immutable' USING ERRCODE = '23514';
    END IF;
    IF ROW(NEW.id, NEW.wallet_id, NEW.player_id, NEW.currency, NEW.amount, NEW.kind,
           NEW.provider_id, NEW.external_id, NEW.idempotency_key, NEW.payload_hash, NEW.request, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.wallet_id, OLD.player_id, OLD.currency, OLD.amount, OLD.kind,
           OLD.provider_id, OLD.external_id, OLD.idempotency_key, OLD.payload_hash, OLD.request, OLD.created_at) THEN
        RAISE EXCEPTION 'transaction identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER transaction_immutable BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION protect_transaction();
CREATE TRIGGER transactions_no_delete BEFORE DELETE OR TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_financial_mutation();

CREATE TABLE inbox (
    consumer_name TEXT NOT NULL,
    message_id TEXT NOT NULL,
    payload_hash TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (consumer_name, message_id)
);

CREATE TABLE outbox (
    event_id UUID PRIMARY KEY,
    aggregate_id UUID NOT NULL REFERENCES wallets(id),
    event_type TEXT NOT NULL,
    payload JSONB NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_id UUID,
    lease_until TIMESTAMPTZ,
    published_at TIMESTAMPTZ
);
CREATE INDEX outbox_pending ON outbox(next_attempt_at) WHERE published_at IS NULL;
