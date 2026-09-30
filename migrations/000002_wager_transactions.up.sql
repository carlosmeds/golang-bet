-- Wager transactions: every financial operation (internal OPENING or external
-- BET/WIN/LOSS/REFUND/ROLLBACK), its idempotency identity, lifecycle state, the
-- result observed at processing time and the durable takeover lease.

CREATE TABLE wager_transactions (
    id                                 uuid        NOT NULL,
    origin                             text        NOT NULL,
    -- Stable identity of internal operations ('opening:<walletId>'); NULL when external.
    internal_key                       text,
    -- External identity; NULL for internal operations.
    provider_id                        text,
    external_transaction_id            text,
    idempotency_key                    text,
    request_hash                       text,
    wallet_id                          uuid        NOT NULL,
    player_id                          text        NOT NULL,
    kind                               text        NOT NULL,
    round_id                           text,
    game_id                            text,
    amount                             bigint      NOT NULL,
    currency                           char(3)     NOT NULL,
    -- Reference by external id (as sent) and, once resolved, by internal id.
    reference_external_transaction_id  text,
    reference_transaction_id           uuid,
    reference_kind                     text,
    status                             text        NOT NULL,
    failure_code                       text,
    failure_message                    text,
    -- Result observed when the operation completed; replays return exactly this.
    result_balance_amount              bigint,
    result_balance_currency            char(3),
    result_wallet_version              bigint,
    -- Durable takeover (PENDING / PENDING_REFERENCE): retry schedule, TTL and lease.
    attempt_count                      integer     NOT NULL DEFAULT 0,
    next_attempt_at                    timestamptz,
    reference_deadline_at              timestamptz,
    lease_owner                        text,
    lease_expires_at                   timestamptz,
    created_at                         timestamptz NOT NULL DEFAULT now(),
    updated_at                         timestamptz NOT NULL DEFAULT now(),
    completed_at                       timestamptz,

    CONSTRAINT wager_transactions_pkey PRIMARY KEY (id),
    CONSTRAINT wager_transactions_wallet_fkey
        FOREIGN KEY (wallet_id) REFERENCES wallets (id) ON DELETE RESTRICT,
    CONSTRAINT wager_transactions_reference_fkey
        FOREIGN KEY (reference_transaction_id, reference_kind)
        REFERENCES wager_transactions (id, kind) ON DELETE RESTRICT,

    -- Targets for composite foreign keys (ledger entries, reference kind).
    CONSTRAINT wager_transactions_id_wallet_key UNIQUE (id, wallet_id),
    CONSTRAINT wager_transactions_id_kind_key UNIQUE (id, kind),

    -- Idempotency: one key and one external id per provider, never applied twice.
    CONSTRAINT wager_transactions_provider_idempotency_key UNIQUE (provider_id, idempotency_key),
    CONSTRAINT wager_transactions_provider_external_key UNIQUE (provider_id, external_transaction_id),
    CONSTRAINT wager_transactions_internal_key_key UNIQUE (internal_key),

    CONSTRAINT wager_transactions_origin_check CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wager_transactions_kind_check
        CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_transactions_status_check
        CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    CONSTRAINT wager_transactions_currency_check CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wager_transactions_result_currency_check
        CHECK (result_balance_currency IS NULL OR result_balance_currency ~ '^[A-Z]{3}$'),
    CONSTRAINT wager_transactions_player_id_check CHECK (char_length(btrim(player_id)) BETWEEN 1 AND 255),
    CONSTRAINT wager_transactions_text_check CHECK (
        (provider_id IS NULL OR char_length(btrim(provider_id)) BETWEEN 1 AND 255)
        AND (external_transaction_id IS NULL OR char_length(btrim(external_transaction_id)) BETWEEN 1 AND 255)
        AND (idempotency_key IS NULL OR char_length(btrim(idempotency_key)) BETWEEN 1 AND 255)
        AND (round_id IS NULL OR char_length(btrim(round_id)) BETWEEN 1 AND 255)
        AND (game_id IS NULL OR char_length(btrim(game_id)) BETWEEN 1 AND 255)
        AND (reference_external_transaction_id IS NULL
             OR char_length(btrim(reference_external_transaction_id)) BETWEEN 1 AND 255)
        AND (failure_code IS NULL OR char_length(btrim(failure_code)) BETWEEN 1 AND 100)
    ),
    -- SHA-256 as lower-case hex.
    CONSTRAINT wager_transactions_request_hash_check
        CHECK (request_hash IS NULL OR request_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT wager_transactions_amount_nonnegative_check CHECK (amount >= 0),
    CONSTRAINT wager_transactions_amount_by_kind_check CHECK (
        (kind = 'LOSS' AND amount = 0) OR (kind <> 'LOSS' AND amount > 0)
    ),

    -- Origin split: OPENING is internal only and carries no external fields;
    -- external operations carry the full external identity.
    CONSTRAINT wager_transactions_origin_shape_check CHECK (
        (origin = 'INTERNAL'
            AND kind = 'OPENING'
            AND internal_key = 'opening:' || wallet_id::text
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND request_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL
            AND reference_transaction_id IS NULL
            AND reference_kind IS NULL
            AND status = 'PROCESSED')
        OR
        (origin = 'EXTERNAL'
            AND kind <> 'OPENING'
            AND internal_key IS NULL
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND request_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL)
    ),

    -- References: REFUND and ROLLBACK require one, WIN may carry one, others none.
    CONSTRAINT wager_transactions_reference_shape_check CHECK (
        (kind IN ('REFUND', 'ROLLBACK') AND reference_external_transaction_id IS NOT NULL)
        OR (kind = 'WIN')
        OR (kind NOT IN ('REFUND', 'ROLLBACK', 'WIN') AND reference_external_transaction_id IS NULL)
    ),
    CONSTRAINT wager_transactions_reference_resolved_check CHECK (
        (reference_transaction_id IS NULL) = (reference_kind IS NULL)
        AND (reference_transaction_id IS NULL OR reference_external_transaction_id IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_reference_target_check CHECK (
        reference_kind IS NULL
        OR (kind = 'WIN' AND reference_kind = 'BET')
        OR (kind = 'REFUND' AND reference_kind = 'BET')
        OR (kind = 'ROLLBACK' AND reference_kind IN ('BET', 'WIN', 'REFUND'))
    ),
    -- A processed operation with a reference must have resolved it.
    CONSTRAINT wager_transactions_processed_reference_check CHECK (
        status <> 'PROCESSED'
        OR ((reference_external_transaction_id IS NULL) = (reference_transaction_id IS NULL))
    ),

    -- State shape.
    CONSTRAINT wager_transactions_failure_check CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_failure_message_check CHECK (
        failure_message IS NULL OR status IN ('REJECTED', 'FAILED')
    ),
    CONSTRAINT wager_transactions_result_pair_check CHECK (
        (result_balance_amount IS NULL) = (result_balance_currency IS NULL)
        AND (result_wallet_version IS NULL OR result_balance_amount IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_result_range_check CHECK (
        (result_balance_amount IS NULL OR result_balance_amount >= 0)
        AND (result_wallet_version IS NULL OR result_wallet_version >= 1)
    ),
    CONSTRAINT wager_transactions_processed_result_check CHECK (
        status <> 'PROCESSED' OR result_balance_amount IS NOT NULL
    ),
    CONSTRAINT wager_transactions_unresolved_result_check CHECK (
        status NOT IN ('PENDING', 'PENDING_REFERENCE') OR result_balance_amount IS NULL
    ),
    CONSTRAINT wager_transactions_completed_check CHECK (
        (status IN ('PROCESSED', 'REJECTED', 'FAILED')) = (completed_at IS NOT NULL)
    ),
    CONSTRAINT wager_transactions_attempt_count_check CHECK (attempt_count >= 0),
    -- Non-terminal rows are always schedulable by another instance; PENDING_REFERENCE
    -- additionally carries its 24h-style deadline. Terminal rows drop schedule and lease.
    CONSTRAINT wager_transactions_schedule_check CHECK (
        (status IN ('PENDING', 'PENDING_REFERENCE')
            AND next_attempt_at IS NOT NULL
            AND (status <> 'PENDING_REFERENCE' OR reference_deadline_at IS NOT NULL))
        OR
        (status IN ('PROCESSED', 'REJECTED', 'FAILED')
            AND next_attempt_at IS NULL
            AND lease_owner IS NULL
            AND lease_expires_at IS NULL)
    ),
    CONSTRAINT wager_transactions_lease_check CHECK (
        (lease_owner IS NULL) = (lease_expires_at IS NULL)
        AND (lease_owner IS NULL OR char_length(btrim(lease_owner)) > 0)
    )
);

-- Opening is unique per wallet: the initial credit can never be duplicated.
CREATE UNIQUE INDEX wager_transactions_opening_wallet_key
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- At most one processed reversal (REFUND or ROLLBACK) per referenced transaction.
-- REFUND only targets BET and ROLLBACK targets BET, WIN or REFUND, so this gives
-- REFUND/ROLLBACK mutual exclusion on a BET and a single rollback of a WIN/REFUND,
-- and a rolled-back REFUND never makes the BET refundable again.
CREATE UNIQUE INDEX wager_transactions_processed_reversal_key
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Workers claim due non-terminal rows (lease expiry is filtered in the claim query).
CREATE INDEX wager_transactions_claim_idx
    ON wager_transactions (next_attempt_at, id)
    WHERE status IN ('PENDING', 'PENDING_REFERENCE');

-- Waiting operations are resolved when their referenced transaction is stored.
CREATE INDEX wager_transactions_waiting_reference_idx
    ON wager_transactions (provider_id, reference_external_transaction_id)
    WHERE status = 'PENDING_REFERENCE';

CREATE INDEX wager_transactions_reference_idx
    ON wager_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL;

CREATE INDEX wager_transactions_wallet_idx
    ON wager_transactions (wallet_id, created_at, id);

CREATE FUNCTION wager_transactions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    ref wager_transactions%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'wager transactions cannot be deleted'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_no_delete';
    END IF;

    IF TG_OP = 'UPDATE' THEN
        -- Terminal rows are final.
        IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
            RAISE EXCEPTION 'transaction % is in terminal status % and cannot change', OLD.id, OLD.status
                USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_terminal';
        END IF;

        -- The business identity captured at insertion never changes.
        IF NEW.id IS DISTINCT FROM OLD.id
           OR NEW.origin IS DISTINCT FROM OLD.origin
           OR NEW.internal_key IS DISTINCT FROM OLD.internal_key
           OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
           OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
           OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
           OR NEW.request_hash IS DISTINCT FROM OLD.request_hash
           OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
           OR NEW.player_id IS DISTINCT FROM OLD.player_id
           OR NEW.kind IS DISTINCT FROM OLD.kind
           OR NEW.round_id IS DISTINCT FROM OLD.round_id
           OR NEW.game_id IS DISTINCT FROM OLD.game_id
           OR NEW.amount IS DISTINCT FROM OLD.amount
           OR NEW.currency IS DISTINCT FROM OLD.currency
           OR NEW.reference_external_transaction_id IS DISTINCT FROM OLD.reference_external_transaction_id
           OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
            RAISE EXCEPTION 'transaction identity columns are immutable'
                USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_immutable_identity';
        END IF;

        -- A resolved reference is never replaced.
        IF OLD.reference_transaction_id IS NOT NULL
           AND NEW.reference_transaction_id IS DISTINCT FROM OLD.reference_transaction_id THEN
            RAISE EXCEPTION 'resolved reference cannot change'
                USING ERRCODE = 'restrict_violation', CONSTRAINT = 'wager_transactions_immutable_reference';
        END IF;

        -- Attempts only grow.
        IF NEW.attempt_count < OLD.attempt_count THEN
            RAISE EXCEPTION 'attempt_count cannot decrease'
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_transactions_attempt_count_monotonic';
        END IF;

        NEW.updated_at := GREATEST(clock_timestamp(), OLD.updated_at, NEW.created_at);
    END IF;

    -- A resolved reference must be the very transaction the external id names
    -- (same provider) and cannot be the row itself.
    IF NEW.reference_transaction_id IS NOT NULL THEN
        SELECT * INTO ref FROM wager_transactions WHERE id = NEW.reference_transaction_id;
        IF NOT FOUND OR ref.id = NEW.id
           OR ref.provider_id IS DISTINCT FROM NEW.provider_id
           OR ref.external_transaction_id IS DISTINCT FROM NEW.reference_external_transaction_id THEN
            RAISE EXCEPTION 'reference of transaction % does not match the referenced transaction', NEW.id
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_transactions_reference_identity';
        END IF;

        -- A successful dependent operation needs a processed reference with the same
        -- wallet, player, currency and round; reversals repeat its amount exactly.
        IF NEW.status = 'PROCESSED' THEN
            IF ref.status <> 'PROCESSED'
               OR ref.wallet_id <> NEW.wallet_id
               OR ref.player_id <> NEW.player_id
               OR ref.currency <> NEW.currency
               OR ref.round_id IS DISTINCT FROM NEW.round_id
               OR (NEW.kind IN ('REFUND', 'ROLLBACK') AND ref.amount <> NEW.amount) THEN
                RAISE EXCEPTION 'processed transaction % is incompatible with its reference', NEW.id
                    USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_transactions_reference_compatible';
            END IF;
        END IF;
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_guard_write
    BEFORE INSERT OR UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();

CREATE TRIGGER wager_transactions_guard_delete
    BEFORE DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
