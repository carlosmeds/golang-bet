-- Inbox: durable identity of consumed messages (replay detection by hash).
-- Outbox: immutable event snapshots written with the financial effect and
-- published after commit by claiming rows with a lease.

CREATE TABLE inbox_messages (
    consumer       text        NOT NULL,
    message_id     text        NOT NULL,
    payload_hash   text        NOT NULL,
    transaction_id uuid,
    received_at    timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz,

    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer, message_id),
    CONSTRAINT inbox_messages_transaction_fkey
        FOREIGN KEY (transaction_id) REFERENCES wager_transactions (id) ON DELETE RESTRICT,
    CONSTRAINT inbox_messages_consumer_check CHECK (char_length(btrim(consumer)) BETWEEN 1 AND 255),
    CONSTRAINT inbox_messages_message_id_check CHECK (char_length(btrim(message_id)) BETWEEN 1 AND 255),
    CONSTRAINT inbox_messages_hash_check CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT inbox_messages_completed_check CHECK (completed_at IS NULL OR completed_at >= received_at)
);

CREATE INDEX inbox_messages_transaction_idx
    ON inbox_messages (transaction_id) WHERE transaction_id IS NOT NULL;

-- Identity and hash are fixed; completion and the linked transaction are set once.
CREATE FUNCTION inbox_messages_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'inbox messages cannot be deleted'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'inbox_messages_no_delete';
    END IF;
    IF NEW.consumer IS DISTINCT FROM OLD.consumer
       OR NEW.message_id IS DISTINCT FROM OLD.message_id
       OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
       OR NEW.received_at IS DISTINCT FROM OLD.received_at
       OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at)
       OR (OLD.transaction_id IS NOT NULL AND NEW.transaction_id IS DISTINCT FROM OLD.transaction_id) THEN
        RAISE EXCEPTION 'inbox message identity and completion are immutable'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'inbox_messages_immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER inbox_messages_guard_update
    BEFORE UPDATE OR DELETE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard();

CREATE TABLE outbox_events (
    seq              bigint      GENERATED ALWAYS AS IDENTITY,
    event_id         uuid        NOT NULL,
    aggregate_type   text        NOT NULL,
    aggregate_id     text        NOT NULL,
    event_type       text        NOT NULL,
    event_version    integer     NOT NULL DEFAULT 1,
    correlation_id   text        NOT NULL,
    causation_id     text,
    payload          jsonb       NOT NULL,
    occurred_at      timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    attempt_count    integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz NOT NULL DEFAULT now(),
    lease_owner      text,
    lease_expires_at timestamptz,
    last_error       text,
    published_at     timestamptz,

    CONSTRAINT outbox_events_pkey PRIMARY KEY (event_id),
    CONSTRAINT outbox_events_seq_key UNIQUE (seq),
    CONSTRAINT outbox_events_text_check CHECK (
        char_length(btrim(aggregate_type)) BETWEEN 1 AND 100
        AND char_length(btrim(aggregate_id)) BETWEEN 1 AND 255
        AND char_length(btrim(event_type)) BETWEEN 1 AND 100
        AND char_length(btrim(correlation_id)) BETWEEN 1 AND 255
        AND (causation_id IS NULL OR char_length(btrim(causation_id)) BETWEEN 1 AND 255)
    ),
    CONSTRAINT outbox_events_version_check CHECK (event_version >= 1),
    CONSTRAINT outbox_events_payload_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_attempt_count_check CHECK (attempt_count >= 0),
    CONSTRAINT outbox_events_lease_check CHECK (
        (lease_owner IS NULL) = (lease_expires_at IS NULL)
        AND (lease_owner IS NULL OR char_length(btrim(lease_owner)) > 0)
    ),
    -- Published rows are final: no lease left over.
    CONSTRAINT outbox_events_published_check CHECK (
        published_at IS NULL OR lease_owner IS NULL
    )
);

-- Claim query: unpublished rows that are due, oldest first (lease expiry filtered in SQL).
CREATE INDEX outbox_events_claim_idx
    ON outbox_events (next_attempt_at, seq) WHERE published_at IS NULL;

CREATE INDEX outbox_events_aggregate_idx
    ON outbox_events (aggregate_type, aggregate_id, seq);

-- The snapshot is immutable; only delivery bookkeeping changes.
CREATE FUNCTION outbox_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.published_at IS NULL THEN
            RAISE EXCEPTION 'unpublished outbox events cannot be deleted'
                USING ERRCODE = 'restrict_violation', CONSTRAINT = 'outbox_events_no_delete';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.seq IS DISTINCT FROM OLD.seq
       OR NEW.event_id IS DISTINCT FROM OLD.event_id
       OR NEW.aggregate_type IS DISTINCT FROM OLD.aggregate_type
       OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.event_version IS DISTINCT FROM OLD.event_version
       OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
       OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'outbox event snapshot is immutable'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'outbox_events_immutable';
    END IF;
    IF OLD.published_at IS NOT NULL AND NEW.published_at IS DISTINCT FROM OLD.published_at THEN
        RAISE EXCEPTION 'published outbox event cannot be republished or reopened'
            USING ERRCODE = 'restrict_violation', CONSTRAINT = 'outbox_events_immutable';
    END IF;
    IF NEW.attempt_count < OLD.attempt_count THEN
        RAISE EXCEPTION 'attempt_count cannot decrease'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'outbox_events_attempt_count_monotonic';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_events_guard_update
    BEFORE UPDATE OR DELETE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();
