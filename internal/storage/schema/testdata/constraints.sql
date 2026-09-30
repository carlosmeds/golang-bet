-- Behavioural tests for the schema. Run by ../run_tests.sh against a database with
-- every migration applied. A failed expectation raises and aborts (ON_ERROR_STOP).
\set ON_ERROR_STOP on
\set QUIET on

CREATE FUNCTION pg_temp.u(n int) RETURNS uuid LANGUAGE sql IMMUTABLE AS
$$ SELECT ('00000000-0000-0000-0000-' || lpad(n::text, 12, '0'))::uuid $$;

-- Runs stmt (and all deferred constraints) in a sub-transaction and requires it to
-- fail with the given SQLSTATE (and constraint name when given).
CREATE FUNCTION pg_temp.expect_error(label text, stmt text, want_state text,
                                     want_constraint text DEFAULT NULL) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE st text; cn text; msg text;
BEGIN
    BEGIN
        EXECUTE stmt;
        EXECUTE 'SET CONSTRAINTS ALL IMMEDIATE';
    EXCEPTION WHEN OTHERS THEN
        GET STACKED DIAGNOSTICS st = RETURNED_SQLSTATE, cn = CONSTRAINT_NAME, msg = MESSAGE_TEXT;
        IF st = want_state AND (want_constraint IS NULL OR cn = want_constraint) THEN
            RETURN;
        END IF;
        RAISE EXCEPTION 'TEST FAILED [%]: expected % / %, got % / % (%)',
            label, want_state, want_constraint, st, cn, msg;
    END;
    RAISE EXCEPTION 'TEST FAILED [%]: statement succeeded but must fail with % / %',
        label, want_state, want_constraint;
END $$;

-- Runs stmt and requires success, including every deferred constraint.
CREATE FUNCTION pg_temp.expect_ok(label text, stmt text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE st text; msg text;
BEGIN
    EXECUTE stmt;
    EXECUTE 'SET CONSTRAINTS ALL IMMEDIATE';
    EXECUTE 'SET CONSTRAINTS ALL DEFERRED';
EXCEPTION WHEN OTHERS THEN
    GET STACKED DIAGNOSTICS st = RETURNED_SQLSTATE, msg = MESSAGE_TEXT;
    RAISE EXCEPTION 'TEST FAILED [%]: unexpected error % (%)', label, st, msg;
END $$;

-- Opens a wallet the way the application must: zero -> wallet only; positive ->
-- wallet + PROCESSED OPENING + credit ledger entry (version 1).
CREATE FUNCTION pg_temp.mk_wallet(w uuid, p_player text, p_currency text, p_opening bigint)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE tx uuid := md5('opening' || w::text)::uuid;
BEGIN
    INSERT INTO wallets (id, player_id, currency, balance_amount, version)
    VALUES (w, p_player, p_currency, p_opening, 1);
    IF p_opening > 0 THEN
        INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount,
            currency, status, result_balance_amount, result_balance_currency, result_wallet_version,
            completed_at)
        VALUES (tx, 'INTERNAL', 'opening:' || w, w, p_player, 'OPENING', p_opening, p_currency,
            'PROCESSED', p_opening, p_currency, 1, now());
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount,
            currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), w, tx, 1, 'CREDIT', p_opening, p_currency, 0, p_opening);
    END IF;
END $$;

-- Applies one processed external operation the way the application must: lock the
-- wallet, insert transaction + ledger entry, update balance and version.
CREATE FUNCTION pg_temp.apply_op(p_tx uuid, w uuid, p_kind text, p_ext text, p_amount bigint,
    p_ref_ext text DEFAULT NULL, p_ref_id uuid DEFAULT NULL, p_round text DEFAULT 'r1',
    p_provider text DEFAULT 'p1') RETURNS void LANGUAGE plpgsql AS $$
DECLARE wl wallets%ROWTYPE; dir text; rk text; new_balance bigint; v bigint;
BEGIN
    SELECT * INTO wl FROM wallets WHERE id = w FOR UPDATE;
    IF p_ref_id IS NOT NULL THEN
        SELECT t.kind INTO rk FROM wager_transactions t WHERE t.id = p_ref_id;
    END IF;
    dir := CASE WHEN p_kind = 'BET' THEN 'DEBIT'
                WHEN p_kind IN ('WIN', 'REFUND') THEN 'CREDIT'
                WHEN rk = 'BET' THEN 'CREDIT' ELSE 'DEBIT' END;
    new_balance := CASE WHEN dir = 'CREDIT' THEN wl.balance_amount + p_amount
                        ELSE wl.balance_amount - p_amount END;
    v := wl.version + 1;
    INSERT INTO wager_transactions (id, origin, provider_id, external_transaction_id, idempotency_key,
        request_hash, wallet_id, player_id, kind, round_id, game_id, amount, currency,
        reference_external_transaction_id, reference_transaction_id, reference_kind, status,
        result_balance_amount, result_balance_currency, result_wallet_version, completed_at)
    VALUES (p_tx, 'EXTERNAL', p_provider, p_ext, 'key-' || p_ext, repeat('a', 64), w, wl.player_id,
        p_kind, p_round, 'g1', p_amount, wl.currency, p_ref_ext, p_ref_id, rk, 'PROCESSED',
        new_balance, wl.currency, v, now());
    INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount,
        currency, balance_before, balance_after)
    VALUES (gen_random_uuid(), w, p_tx, v, dir, p_amount, wl.currency, wl.balance_amount, new_balance);
    UPDATE wallets SET balance_amount = new_balance, version = v WHERE id = w;
END $$;

-- Raw insert of an external transaction: a valid PENDING BET on wallet 1 unless overridden.
CREATE FUNCTION pg_temp.ins_tx(over jsonb) RETURNS void LANGUAGE plpgsql AS $$
DECLARE rec wager_transactions;
BEGIN
    rec := jsonb_populate_record(NULL::wager_transactions, jsonb_build_object(
        'id', gen_random_uuid(), 'origin', 'EXTERNAL', 'provider_id', 'p1',
        'external_transaction_id', 'e-' || gen_random_uuid(),
        'idempotency_key', 'k-' || gen_random_uuid(), 'request_hash', repeat('b', 64),
        'wallet_id', pg_temp.u(1), 'player_id', 'pl1', 'kind', 'BET', 'round_id', 'r1',
        'game_id', 'g1', 'amount', 10, 'currency', 'USD', 'status', 'PENDING',
        'attempt_count', 0, 'next_attempt_at', now(), 'created_at', now(), 'updated_at', now()
    ) || over);
    INSERT INTO wager_transactions SELECT (rec).*;
END $$;

-- Fixtures -------------------------------------------------------------------
SELECT pg_temp.mk_wallet(pg_temp.u(1), 'pl1', 'USD', 100);
SELECT pg_temp.mk_wallet(pg_temp.u(2), 'pl2', 'USD', 0);
SELECT pg_temp.mk_wallet(pg_temp.u(3), 'pl1', 'EUR', 50);

-- Wallets ----------------------------------------------------------------------
SELECT pg_temp.expect_error('duplicate (player,currency)',
    $q$ INSERT INTO wallets (id, player_id, currency) VALUES (pg_temp.u(90), 'pl1', 'USD') $q$,
    '23505', 'wallets_player_currency_key');
SELECT pg_temp.expect_ok('same player other currency',
    $q$ INSERT INTO wallets (id, player_id, currency) VALUES (pg_temp.u(91), 'pl1', 'BRL') $q$);
SELECT pg_temp.expect_error('negative initial balance',
    $q$ INSERT INTO wallets (id, player_id, currency, balance_amount) VALUES (pg_temp.u(92), 'x', 'USD', -1) $q$,
    '23514', 'wallets_balance_nonnegative_check');
SELECT pg_temp.expect_error('negative balance on update',
    $q$ UPDATE wallets SET balance_amount = -1, version = version + 1 WHERE id = pg_temp.u(1) $q$,
    '23514', 'wallets_balance_nonnegative_check');
SELECT pg_temp.expect_error('initial version must be 1',
    $q$ INSERT INTO wallets (id, player_id, currency, version) VALUES (pg_temp.u(93), 'x', 'USD', 2) $q$,
    '23514', 'wallets_initial_version');
SELECT pg_temp.expect_error('bad currency',
    $q$ INSERT INTO wallets (id, player_id, currency) VALUES (pg_temp.u(94), 'x', 'usd') $q$,
    '23514', 'wallets_currency_check');
SELECT pg_temp.expect_error('blank player',
    $q$ INSERT INTO wallets (id, player_id, currency) VALUES (pg_temp.u(95), '  ', 'USD') $q$,
    '23514', 'wallets_player_id_check');
SELECT pg_temp.expect_error('balance change without version bump',
    $q$ UPDATE wallets SET balance_amount = balance_amount + 1 WHERE id = pg_temp.u(2) $q$,
    '23514', 'wallets_version_step');
SELECT pg_temp.expect_error('version bump without balance change',
    $q$ UPDATE wallets SET version = version + 1 WHERE id = pg_temp.u(2) $q$,
    '23514', 'wallets_version_step');
SELECT pg_temp.expect_error('version jump',
    $q$ UPDATE wallets SET balance_amount = 5, version = 3 WHERE id = pg_temp.u(2) $q$,
    '23514', 'wallets_version_step');
SELECT pg_temp.expect_error('wallet identity immutable',
    $q$ UPDATE wallets SET player_id = 'other' WHERE id = pg_temp.u(2) $q$,
    '23001', 'wallets_immutable_identity');
SELECT pg_temp.expect_error('wallet delete',
    $q$ DELETE FROM wallets WHERE id = pg_temp.u(2) $q$, '23001', 'wallets_no_delete');
SELECT pg_temp.expect_error('balance change without ledger (deferred)',
    $q$ UPDATE wallets SET balance_amount = 5, version = 2 WHERE id = pg_temp.u(2) $q$,
    '23514', 'wallets_ledger_match');
SELECT pg_temp.expect_error('new positive wallet without opening ledger (deferred)',
    $q$ INSERT INTO wallets (id, player_id, currency, balance_amount) VALUES (pg_temp.u(96), 'x', 'USD', 5) $q$,
    '23514', 'wallets_ledger_match');

-- Opening ------------------------------------------------------------------------
SELECT pg_temp.expect_error('duplicate opening for a wallet',
    $q$ INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount,
            currency, status, result_balance_amount, result_balance_currency, completed_at)
        VALUES (pg_temp.u(97), 'INTERNAL', 'opening:' || pg_temp.u(1), pg_temp.u(1), 'pl1', 'OPENING',
            100, 'USD', 'PROCESSED', 100, 'USD', now()) $q$,
    '23505');
SELECT pg_temp.expect_error('opening must use the stable internal key',
    $q$ INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount,
            currency, status, result_balance_amount, result_balance_currency, completed_at)
        VALUES (pg_temp.u(97), 'INTERNAL', 'opening:wrong', pg_temp.u(2), 'pl2', 'OPENING',
            10, 'USD', 'PROCESSED', 10, 'USD', now()) $q$,
    '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('opening cannot carry external fields',
    $q$ INSERT INTO wager_transactions (id, origin, internal_key, provider_id, external_transaction_id,
            idempotency_key, request_hash, wallet_id, player_id, kind, round_id, game_id, amount,
            currency, status, result_balance_amount, result_balance_currency, completed_at)
        VALUES (pg_temp.u(97), 'INTERNAL', 'opening:' || pg_temp.u(2), 'p1', 'e1', 'k1', repeat('a', 64),
            pg_temp.u(2), 'pl2', 'OPENING', 'r', 'g', 10, 'USD', 'PROCESSED', 10, 'USD', now()) $q$,
    '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('external origin cannot be OPENING',
    $q$ SELECT pg_temp.ins_tx('{"kind":"OPENING"}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('zero opening amount',
    $q$ INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount,
            currency, status, result_balance_amount, result_balance_currency, completed_at)
        VALUES (pg_temp.u(97), 'INTERNAL', 'opening:' || pg_temp.u(2), pg_temp.u(2), 'pl2', 'OPENING',
            0, 'USD', 'PROCESSED', 0, 'USD', now()) $q$,
    '23514', 'wager_transactions_amount_by_kind_check');
SELECT pg_temp.expect_error('opening without ledger entry (deferred)',
    $q$ INSERT INTO wager_transactions (id, origin, internal_key, wallet_id, player_id, kind, amount,
            currency, status, result_balance_amount, result_balance_currency, completed_at)
        VALUES (pg_temp.u(97), 'INTERNAL', 'opening:' || pg_temp.u(2), pg_temp.u(2), 'pl2', 'OPENING',
            10, 'USD', 'PROCESSED', 10, 'USD', now()) $q$,
    '23514', 'wager_transactions_ledger_match');

-- Transaction shape and unique keys ------------------------------------------------
SELECT pg_temp.expect_ok('valid pending BET', $q$ SELECT pg_temp.ins_tx('{}') $q$);
SELECT pg_temp.expect_error('external needs idempotency key',
    $q$ SELECT pg_temp.ins_tx('{"idempotency_key":null}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('external needs request hash',
    $q$ SELECT pg_temp.ins_tx('{"request_hash":null}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('external needs external id',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":null}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('external needs round',
    $q$ SELECT pg_temp.ins_tx('{"round_id":null}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('external needs game',
    $q$ SELECT pg_temp.ins_tx('{"game_id":null}') $q$, '23514', 'wager_transactions_origin_shape_check');
SELECT pg_temp.expect_error('hash must be sha-256 hex',
    $q$ SELECT pg_temp.ins_tx('{"request_hash":"XYZ"}') $q$, '23514', 'wager_transactions_request_hash_check');
SELECT pg_temp.expect_error('blank external id',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":" "}') $q$, '23514', 'wager_transactions_text_check');
SELECT pg_temp.expect_error('unknown wallet',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('wallet_id', pg_temp.u(999))) $q$, '23503', 'wager_transactions_wallet_fkey');
SELECT pg_temp.expect_error('BET amount zero',
    $q$ SELECT pg_temp.ins_tx('{"amount":0}') $q$, '23514', 'wager_transactions_amount_by_kind_check');
SELECT pg_temp.expect_error('negative amount',
    $q$ SELECT pg_temp.ins_tx('{"amount":-5}') $q$, '23514');
SELECT pg_temp.expect_error('LOSS amount must be zero',
    $q$ SELECT pg_temp.ins_tx('{"kind":"LOSS","amount":5}') $q$, '23514', 'wager_transactions_amount_by_kind_check');
SELECT pg_temp.expect_ok('LOSS with zero amount',
    $q$ SELECT pg_temp.ins_tx('{"kind":"LOSS","amount":0}') $q$);
SELECT pg_temp.expect_error('unknown kind',
    $q$ SELECT pg_temp.ins_tx('{"kind":"TIP"}') $q$, '23514', 'wager_transactions_kind_check');
SELECT pg_temp.expect_error('unknown status',
    $q$ SELECT pg_temp.ins_tx('{"status":"DONE"}') $q$, '23514');

SELECT pg_temp.expect_ok('seed unique keys',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"dup-ext","idempotency_key":"dup-key","provider_id":"pA"}') $q$);
SELECT pg_temp.expect_error('same provider and idempotency key',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"other-ext","idempotency_key":"dup-key","provider_id":"pA"}') $q$,
    '23505', 'wager_transactions_provider_idempotency_key');
SELECT pg_temp.expect_error('same provider and external id with a second key',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"dup-ext","idempotency_key":"other-key","provider_id":"pA"}') $q$,
    '23505', 'wager_transactions_provider_external_key');
SELECT pg_temp.expect_ok('same ids under another provider',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"dup-ext","idempotency_key":"dup-key","provider_id":"pB"}') $q$);

-- References -------------------------------------------------------------------------
SELECT pg_temp.expect_error('REFUND requires reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"REFUND"}') $q$, '23514', 'wager_transactions_reference_shape_check');
SELECT pg_temp.expect_error('ROLLBACK requires reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"ROLLBACK"}') $q$, '23514', 'wager_transactions_reference_shape_check');
SELECT pg_temp.expect_error('BET cannot reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"BET","reference_external_transaction_id":"x"}') $q$,
    '23514', 'wager_transactions_reference_shape_check');
SELECT pg_temp.expect_ok('WIN without reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"WIN"}') $q$);
SELECT pg_temp.expect_ok('WIN waiting for unknown reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"WIN","reference_external_transaction_id":"later","status":"PENDING_REFERENCE","reference_deadline_at":"2099-01-01T00:00:00Z"}') $q$);
SELECT pg_temp.expect_error('processed REFUND with unresolved reference',
    $q$ SELECT pg_temp.ins_tx('{"kind":"REFUND","reference_external_transaction_id":"x","status":"PROCESSED","completed_at":"2099-01-01T00:00:00Z","next_attempt_at":null,"result_balance_amount":1,"result_balance_currency":"USD"}') $q$,
    '23514', 'wager_transactions_processed_reference_check');

-- Reversal rules: wallet 4 bets, refunds, rolls back.
SELECT pg_temp.mk_wallet(pg_temp.u(4), 'pl4', 'USD', 1000);
SELECT pg_temp.expect_ok('BET 1', $q$ SELECT pg_temp.apply_op(pg_temp.u(41), pg_temp.u(4), 'BET', 'bet-1', 100) $q$);
SELECT pg_temp.expect_ok('BET 2', $q$ SELECT pg_temp.apply_op(pg_temp.u(42), pg_temp.u(4), 'BET', 'bet-2', 100) $q$);
SELECT pg_temp.expect_ok('WIN 1 (refs bet-2)', $q$ SELECT pg_temp.apply_op(pg_temp.u(43), pg_temp.u(4), 'WIN', 'win-1', 300, 'bet-2', pg_temp.u(42)) $q$);
SELECT pg_temp.expect_ok('REFUND of BET 1', $q$ SELECT pg_temp.apply_op(pg_temp.u(44), pg_temp.u(4), 'REFUND', 'refund-1', 100, 'bet-1', pg_temp.u(41)) $q$);
SELECT pg_temp.expect_error('ROLLBACK of a BET that was already refunded',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(45), pg_temp.u(4), 'ROLLBACK', 'rb-1', 100, 'bet-1', pg_temp.u(41)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_error('second REFUND of the same BET',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(45), pg_temp.u(4), 'REFUND', 'refund-2', 100, 'bet-1', pg_temp.u(41)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_ok('ROLLBACK of the REFUND',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(46), pg_temp.u(4), 'ROLLBACK', 'rb-refund', 100, 'refund-1', pg_temp.u(44)) $q$);
SELECT pg_temp.expect_error('rolling back a REFUND does not reopen REFUND of its BET',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(47), pg_temp.u(4), 'REFUND', 'refund-3', 100, 'bet-1', pg_temp.u(41)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_error('rolling back a REFUND does not reopen ROLLBACK of its BET',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(47), pg_temp.u(4), 'ROLLBACK', 'rb-2', 100, 'bet-1', pg_temp.u(41)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_error('second ROLLBACK of the REFUND',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(47), pg_temp.u(4), 'ROLLBACK', 'rb-3', 100, 'refund-1', pg_temp.u(44)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_ok('ROLLBACK of the WIN',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(48), pg_temp.u(4), 'ROLLBACK', 'rb-win', 300, 'win-1', pg_temp.u(43)) $q$);
SELECT pg_temp.expect_error('second ROLLBACK of the WIN',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(49), pg_temp.u(4), 'ROLLBACK', 'rb-win-2', 300, 'win-1', pg_temp.u(43)) $q$,
    '23505', 'wager_transactions_processed_reversal_key');
SELECT pg_temp.expect_error('REFUND cannot target a WIN',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('kind','REFUND','reference_external_transaction_id','win-1',
        'reference_transaction_id', pg_temp.u(43), 'reference_kind','WIN','wallet_id', pg_temp.u(4))) $q$,
    '23514', 'wager_transactions_reference_target_check');
SELECT pg_temp.expect_error('reference kind must match the referenced row',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('kind','ROLLBACK','reference_external_transaction_id','bet-2',
        'reference_transaction_id', pg_temp.u(42), 'reference_kind','WIN','wallet_id', pg_temp.u(4))) $q$,
    '23503', 'wager_transactions_reference_fkey');
SELECT pg_temp.expect_error('reference external id must name the referenced row',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('kind','REFUND','reference_external_transaction_id','not-bet-2',
        'reference_transaction_id', pg_temp.u(42), 'reference_kind','BET','wallet_id', pg_temp.u(4))) $q$,
    '23514', 'wager_transactions_reference_identity');
SELECT pg_temp.expect_error('reference from another provider',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('kind','REFUND','reference_external_transaction_id','bet-2',
        'reference_transaction_id', pg_temp.u(42), 'reference_kind','BET','wallet_id', pg_temp.u(4), 'provider_id','p2')) $q$,
    '23514', 'wager_transactions_reference_identity');
SELECT pg_temp.mk_wallet(pg_temp.u(5), 'pl4', 'EUR', 500);
SELECT pg_temp.expect_ok('BET 3', $q$ SELECT pg_temp.apply_op(pg_temp.u(50), pg_temp.u(4), 'BET', 'bet-3', 70) $q$);
SELECT pg_temp.expect_error('REFUND with a different amount',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(51), pg_temp.u(4), 'REFUND', 'refund-x', 60, 'bet-3', pg_temp.u(50)) $q$,
    '23514', 'wager_transactions_reference_compatible');
SELECT pg_temp.expect_error('REFUND from another round',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(51), pg_temp.u(4), 'REFUND', 'refund-x', 70, 'bet-3', pg_temp.u(50), 'other-round') $q$,
    '23514', 'wager_transactions_reference_compatible');
SELECT pg_temp.expect_error('REFUND whose reference is in another wallet',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(51), pg_temp.u(5), 'REFUND', 'refund-x', 70, 'bet-3', pg_temp.u(50)) $q$,
    '23514', 'wager_transactions_reference_compatible');
SELECT pg_temp.expect_error('dependent cannot be processed against a pending reference',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"pend-bet","kind":"BET"}');
        SELECT pg_temp.ins_tx(jsonb_build_object('kind','WIN','reference_external_transaction_id','pend-bet',
            'reference_transaction_id', (SELECT id FROM wager_transactions WHERE external_transaction_id = 'pend-bet'),
            'reference_kind','BET','wallet_id', pg_temp.u(1), 'status','PROCESSED','completed_at', now(),
            'next_attempt_at', NULL, 'result_balance_amount', 1, 'result_balance_currency', 'USD')) $q$,
    '23514', 'wager_transactions_reference_compatible');

-- Lifecycle, failure and lease shape ---------------------------------------------------
SELECT pg_temp.expect_error('REJECTED needs failure code',
    $q$ SELECT pg_temp.ins_tx('{"status":"REJECTED","completed_at":"2099-01-01T00:00:00Z","next_attempt_at":null}') $q$,
    '23514', 'wager_transactions_failure_check');
SELECT pg_temp.expect_error('PENDING cannot carry a failure code',
    $q$ SELECT pg_temp.ins_tx('{"failure_code":"X"}') $q$, '23514', 'wager_transactions_failure_check');
SELECT pg_temp.expect_ok('REJECTED with failure code',
    $q$ SELECT pg_temp.ins_tx('{"status":"REJECTED","failure_code":"INSUFFICIENT_FUNDS_BET","completed_at":"2099-01-01T00:00:00Z","next_attempt_at":null}') $q$);
SELECT pg_temp.expect_error('PENDING_REFERENCE needs a deadline',
    $q$ SELECT pg_temp.ins_tx('{"kind":"WIN","reference_external_transaction_id":"r","status":"PENDING_REFERENCE"}') $q$,
    '23514', 'wager_transactions_schedule_check');
SELECT pg_temp.expect_error('PENDING needs next_attempt_at',
    $q$ SELECT pg_temp.ins_tx('{"next_attempt_at":null}') $q$, '23514', 'wager_transactions_schedule_check');
SELECT pg_temp.expect_error('lease owner without expiry',
    $q$ SELECT pg_temp.ins_tx('{"lease_owner":"w1"}') $q$, '23514', 'wager_transactions_lease_check');
SELECT pg_temp.expect_error('terminal row cannot keep a lease',
    $q$ SELECT pg_temp.ins_tx('{"status":"REJECTED","failure_code":"X","completed_at":"2099-01-01T00:00:00Z","next_attempt_at":null,"lease_owner":"w","lease_expires_at":"2099-01-01T00:00:00Z"}') $q$,
    '23514', 'wager_transactions_schedule_check');
SELECT pg_temp.expect_error('processed needs completion time',
    $q$ SELECT pg_temp.ins_tx('{"kind":"LOSS","amount":0,"status":"PROCESSED","next_attempt_at":null,"result_balance_amount":1,"result_balance_currency":"USD"}') $q$,
    '23514', 'wager_transactions_completed_check');
SELECT pg_temp.expect_error('processed needs result balance',
    $q$ SELECT pg_temp.ins_tx('{"kind":"LOSS","amount":0,"status":"PROCESSED","next_attempt_at":null,"completed_at":"2099-01-01T00:00:00Z"}') $q$,
    '23514', 'wager_transactions_processed_result_check');
SELECT pg_temp.expect_error('result balance must not be negative',
    $q$ SELECT pg_temp.ins_tx('{"kind":"LOSS","amount":0,"status":"PROCESSED","next_attempt_at":null,"completed_at":"2099-01-01T00:00:00Z","result_balance_amount":-1,"result_balance_currency":"USD"}') $q$,
    '23514', 'wager_transactions_result_range_check');
SELECT pg_temp.expect_ok('PROCESSED LOSS needs no ledger entry',
    $q$ SELECT pg_temp.ins_tx('{"external_transaction_id":"loss-ok","kind":"LOSS","amount":0,"status":"PROCESSED","next_attempt_at":null,"completed_at":"2099-01-01T00:00:00Z","result_balance_amount":100,"result_balance_currency":"USD"}') $q$);
SELECT pg_temp.expect_error('processed BET without ledger entry (deferred)',
    $q$ SELECT pg_temp.ins_tx('{"status":"PROCESSED","next_attempt_at":null,"completed_at":"2099-01-01T00:00:00Z","result_balance_amount":90,"result_balance_currency":"USD"}') $q$,
    '23514', 'wager_transactions_ledger_match');

SELECT pg_temp.expect_ok('seed lifecycle row',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('id', pg_temp.u(60), 'kind','WIN',
        'reference_external_transaction_id','bet-lifecycle','status','PENDING')) $q$);
SELECT pg_temp.expect_ok('PENDING -> PENDING_REFERENCE with retry bookkeeping',
    $q$ UPDATE wager_transactions SET status = 'PENDING_REFERENCE', attempt_count = 1,
            reference_deadline_at = now() + interval '24 hours', lease_owner = 'w1',
            lease_expires_at = now() + interval '30 seconds' WHERE id = pg_temp.u(60) $q$);
SELECT pg_temp.expect_error('attempt count cannot decrease',
    $q$ UPDATE wager_transactions SET attempt_count = 0 WHERE id = pg_temp.u(60) $q$,
    '23514', 'wager_transactions_attempt_count_monotonic');
SELECT pg_temp.expect_error('identity cannot change while pending',
    $q$ UPDATE wager_transactions SET amount = 99 WHERE id = pg_temp.u(60) $q$,
    '23001', 'wager_transactions_immutable_identity');
SELECT pg_temp.expect_error('hash cannot change while pending',
    $q$ UPDATE wager_transactions SET request_hash = repeat('c', 64) WHERE id = pg_temp.u(60) $q$,
    '23001', 'wager_transactions_immutable_identity');
SELECT pg_temp.expect_ok('PENDING_REFERENCE -> REJECTED',
    $q$ UPDATE wager_transactions SET status = 'REJECTED', failure_code = 'REFERENCE_NOT_FOUND',
            completed_at = now(), next_attempt_at = NULL, lease_owner = NULL, lease_expires_at = NULL
        WHERE id = pg_temp.u(60) $q$);
SELECT pg_temp.expect_error('terminal row cannot transition',
    $q$ UPDATE wager_transactions SET status = 'PENDING', failure_code = NULL, completed_at = NULL,
            next_attempt_at = now() WHERE id = pg_temp.u(60) $q$,
    '23001', 'wager_transactions_terminal');
SELECT pg_temp.expect_error('processed row cannot change',
    $q$ UPDATE wager_transactions SET game_id = 'x' WHERE id = pg_temp.u(41) $q$,
    '23001', 'wager_transactions_terminal');
SELECT pg_temp.expect_error('transaction delete',
    $q$ DELETE FROM wager_transactions WHERE id = pg_temp.u(41) $q$, '23001', 'wager_transactions_no_delete');
SELECT pg_temp.expect_error('pending transaction delete',
    $q$ DELETE FROM wager_transactions WHERE status = 'PENDING' $q$, '23001', 'wager_transactions_no_delete');

-- Ledger ---------------------------------------------------------------------------------
SELECT pg_temp.mk_wallet(pg_temp.u(6), 'pl6', 'USD', 100);
SELECT pg_temp.expect_ok('seed processed BET awaiting ledger', $q$ SELECT pg_temp.apply_op(pg_temp.u(61), pg_temp.u(6), 'BET', 'l-bet-1', 30) $q$);
-- A pending BET in wallet 6 to hang ledger tests on.
SELECT pg_temp.expect_ok('pending BET for ledger tests',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('id', pg_temp.u(62), 'wallet_id', pg_temp.u(6), 'player_id','pl6',
        'external_transaction_id','l-bet-2', 'amount', 20)) $q$);
-- wallet 6 is now at version 2, balance 70.
-- The next group targets the declarative CHECK constraints, which run after BEFORE triggers,
-- so the row-validation trigger is disabled (inside the rolled-back sub-transaction) for them.
SELECT pg_temp.expect_error('ledger equation: credit that subtracts',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'CREDIT', 20, 'USD', 70, 50) $q$,
    '23514', 'ledger_entries_equation_check');
SELECT pg_temp.expect_error('ledger equation: debit that adds',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 90) $q$,
    '23514', 'ledger_entries_equation_check');
SELECT pg_temp.expect_error('ledger equation: wrong arithmetic',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 49) $q$,
    '23514', 'ledger_entries_equation_check');
SELECT pg_temp.expect_error('ledger amount must be positive',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 0, 'USD', 70, 70) $q$,
    '23514', 'ledger_entries_amount_positive_check');
SELECT pg_temp.expect_error('ledger amount cannot be negative',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'CREDIT', -20, 'USD', 70, 50) $q$,
    '23514', 'ledger_entries_amount_positive_check');
SELECT pg_temp.expect_error('ledger balance cannot go negative',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 80, 'USD', 70, -10) $q$,
    '23514', 'ledger_entries_balances_nonnegative_check');
SELECT pg_temp.expect_error('ledger direction',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'SIDEWAYS', 20, 'USD', 70, 50) $q$,
    '23514', 'ledger_entries_direction_check');
SELECT pg_temp.expect_error('ledger entry for another wallet currency',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'EUR', 70, 50) $q$,
    '23503', 'ledger_entries_wallet_fkey');
SELECT pg_temp.expect_error('ledger entry for a transaction of another wallet',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(4), pg_temp.u(62), 99, 'DEBIT', 20, 'USD', 70, 50) $q$,
    '23503', 'ledger_entries_transaction_fkey');
SELECT pg_temp.expect_error('ledger entry amount differs from its transaction',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 10, 'USD', 70, 60) $q$,
    '23514', 'ledger_entries_transaction_match');
SELECT pg_temp.expect_error('BET must debit',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'CREDIT', 20, 'USD', 70, 90) $q$,
    '23514', 'ledger_entries_transaction_match');
SELECT pg_temp.expect_error('balance_before must continue the chain',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 75, 55) $q$,
    '23514', 'ledger_entries_chain');
SELECT pg_temp.expect_error('ledger version gap',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 4, 'DEBIT', 20, 'USD', 70, 50) $q$,
    '23514', 'ledger_entries_chain');
SELECT pg_temp.expect_error('appending an existing wallet version is rejected by the trigger',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 2, 'DEBIT', 20, 'USD', 100, 80) $q$,
    '23514', 'ledger_entries_chain');
SELECT pg_temp.expect_error('duplicate wallet version (unique index backstop)',
    $q$ ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_validate_insert; INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 2, 'DEBIT', 20, 'USD', 100, 80) $q$,
    '23505', 'ledger_entries_wallet_version_key');
SELECT pg_temp.expect_error('second entry for one (wallet, transaction)',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(61), 3, 'DEBIT', 30, 'USD', 70, 40) $q$,
    '23505', 'ledger_entries_wallet_transaction_key');
SELECT pg_temp.expect_error('ledger entry for a LOSS',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('id', pg_temp.u(63), 'wallet_id', pg_temp.u(6), 'player_id','pl6',
            'kind','LOSS','amount',0));
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(63), 3, 'DEBIT', 1, 'USD', 70, 69) $q$,
    '23514', 'ledger_entries_kind');
SELECT pg_temp.expect_error('ledger entry without matching wallet update (deferred)',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 50) $q$,
    '23514', 'ledger_entries_wallet_match');
SELECT pg_temp.expect_error('ledger entry of an unprocessed transaction (deferred)',
    $q$ INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 50);
        UPDATE wallets SET balance_amount = 50, version = 3 WHERE id = pg_temp.u(6) $q$,
    '23514', 'ledger_entries_transaction_processed');
SELECT pg_temp.expect_error('processed result balance differs from ledger (deferred)',
    $q$ UPDATE wager_transactions SET status = 'PROCESSED', completed_at = now(), next_attempt_at = NULL,
            result_balance_amount = 49, result_balance_currency = 'USD', result_wallet_version = 3
        WHERE id = pg_temp.u(62);
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 50);
        UPDATE wallets SET balance_amount = 50, version = 3 WHERE id = pg_temp.u(6) $q$,
    '23514', 'wager_transactions_ledger_match');
SELECT pg_temp.expect_ok('complete the pending BET through its ledger entry',
    $q$ UPDATE wager_transactions SET status = 'PROCESSED', completed_at = now(), next_attempt_at = NULL,
            result_balance_amount = 50, result_balance_currency = 'USD', result_wallet_version = 3
        WHERE id = pg_temp.u(62);
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(6), pg_temp.u(62), 3, 'DEBIT', 20, 'USD', 70, 50);
        UPDATE wallets SET balance_amount = 50, version = 3 WHERE id = pg_temp.u(6) $q$);
SELECT pg_temp.expect_error('ledger update',
    $q$ UPDATE ledger_entries SET amount = amount WHERE wallet_id = pg_temp.u(6) $q$, '23001', 'ledger_entries_append_only');
SELECT pg_temp.expect_error('ledger update of one column',
    $q$ UPDATE ledger_entries SET balance_after = balance_after + 1 WHERE wallet_id = pg_temp.u(1) $q$, '23001', 'ledger_entries_append_only');
SELECT pg_temp.expect_error('ledger delete',
    $q$ DELETE FROM ledger_entries WHERE wallet_id = pg_temp.u(6) $q$, '23001', 'ledger_entries_append_only');
SELECT pg_temp.expect_error('ledger truncate',
    $q$ TRUNCATE ledger_entries $q$, '23001', 'ledger_entries_append_only');
SELECT pg_temp.expect_error('ledger cascade truncate from wallets',
    $q$ TRUNCATE wallets CASCADE $q$, '23001', 'ledger_entries_append_only');

-- Opening + chain through version 1 and zero-opened wallet
SELECT pg_temp.expect_error('zero-opened wallet: first movement cannot start above zero',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('id', pg_temp.u(70), 'wallet_id', pg_temp.u(2), 'player_id','pl2',
            'kind','WIN','amount',10,'status','PROCESSED','next_attempt_at',NULL,'completed_at',now(),
            'result_balance_amount',10,'result_balance_currency','USD'));
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(2), pg_temp.u(70), 2, 'CREDIT', 10, 'USD', 5, 15) $q$,
    '23514', 'ledger_entries_chain');
SELECT pg_temp.expect_error('only OPENING produces version 1',
    $q$ SELECT pg_temp.ins_tx(jsonb_build_object('id', pg_temp.u(70), 'wallet_id', pg_temp.u(2), 'player_id','pl2',
            'kind','WIN','amount',10,'status','PROCESSED','next_attempt_at',NULL,'completed_at',now(),
            'result_balance_amount',10,'result_balance_currency','USD'));
        INSERT INTO ledger_entries (id, wallet_id, transaction_id, wallet_version, direction, amount, currency, balance_before, balance_after)
        VALUES (gen_random_uuid(), pg_temp.u(2), pg_temp.u(70), 1, 'CREDIT', 10, 'USD', 0, 10) $q$,
    '23514', 'ledger_entries_opening_version');
SELECT pg_temp.expect_ok('zero-opened wallet: WIN then BET',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(71), pg_temp.u(2), 'WIN', 'z-win', 10);
        SELECT pg_temp.apply_op(pg_temp.u(72), pg_temp.u(2), 'BET', 'z-bet', 10) $q$);
SELECT pg_temp.expect_error('debit below zero',
    $q$ SELECT pg_temp.apply_op(pg_temp.u(73), pg_temp.u(2), 'BET', 'z-bet-2', 1) $q$, '23514');

-- Ledger always reproduces the wallet (sanity over everything above).
DO $$
DECLARE bad int;
BEGIN
    SELECT count(*) INTO bad FROM wallets w
    WHERE w.balance_amount <> coalesce((SELECT sum(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END)
                                        FROM ledger_entries l WHERE l.wallet_id = w.id), 0);
    IF bad <> 0 THEN RAISE EXCEPTION 'TEST FAILED: % wallets differ from their ledger', bad; END IF;
END $$;

-- Inbox ------------------------------------------------------------------------------------
SELECT pg_temp.expect_ok('inbox insert',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash) VALUES ('c1', 'm1', repeat('a', 64)) $q$);
SELECT pg_temp.expect_error('inbox duplicate (consumer, message)',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash) VALUES ('c1', 'm1', repeat('b', 64)) $q$,
    '23505', 'inbox_messages_pkey');
SELECT pg_temp.expect_ok('same message id for another consumer',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash) VALUES ('c2', 'm1', repeat('a', 64)) $q$);
SELECT pg_temp.expect_error('inbox hash format',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash) VALUES ('c1', 'm2', 'nope') $q$,
    '23514', 'inbox_messages_hash_check');
SELECT pg_temp.expect_error('inbox blank message id',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash) VALUES ('c1', ' ', repeat('a', 64)) $q$,
    '23514', 'inbox_messages_message_id_check');
SELECT pg_temp.expect_error('inbox unknown transaction',
    $q$ INSERT INTO inbox_messages (consumer, message_id, payload_hash, transaction_id)
        VALUES ('c1', 'm3', repeat('a', 64), pg_temp.u(9999)) $q$, '23503', 'inbox_messages_transaction_fkey');
SELECT pg_temp.expect_error('inbox hash immutable',
    $q$ UPDATE inbox_messages SET payload_hash = repeat('c', 64) WHERE consumer = 'c1' AND message_id = 'm1' $q$,
    '23001', 'inbox_messages_immutable');
SELECT pg_temp.expect_ok('inbox completion',
    $q$ UPDATE inbox_messages SET completed_at = now(), transaction_id = pg_temp.u(41)
        WHERE consumer = 'c1' AND message_id = 'm1' $q$);
SELECT pg_temp.expect_error('inbox completion is final',
    $q$ UPDATE inbox_messages SET completed_at = now() + interval '1 hour' WHERE consumer = 'c1' AND message_id = 'm1' $q$,
    '23001', 'inbox_messages_immutable');
SELECT pg_temp.expect_error('inbox delete',
    $q$ DELETE FROM inbox_messages WHERE consumer = 'c1' $q$, '23001', 'inbox_messages_no_delete');

-- Outbox -----------------------------------------------------------------------------------
CREATE FUNCTION pg_temp.ins_event(over jsonb) RETURNS void LANGUAGE plpgsql AS $$
DECLARE rec outbox_events;
BEGIN
    rec := jsonb_populate_record(NULL::outbox_events, jsonb_build_object(
        'event_id', gen_random_uuid(), 'aggregate_type', 'wallet', 'aggregate_id', pg_temp.u(1)::text,
        'event_type', 'WalletBalanceChanged', 'event_version', 1, 'correlation_id', 'corr-1',
        'payload', '{"eventId":"x"}'::jsonb, 'occurred_at', now(), 'created_at', now(),
        'attempt_count', 0, 'next_attempt_at', now()) || over);
    INSERT INTO outbox_events (event_id, aggregate_type, aggregate_id, event_type, event_version, correlation_id,
        causation_id, payload, occurred_at, created_at, attempt_count, next_attempt_at, lease_owner,
        lease_expires_at, last_error, published_at)
    VALUES (rec.event_id, rec.aggregate_type, rec.aggregate_id, rec.event_type, rec.event_version,
        rec.correlation_id, rec.causation_id, rec.payload, rec.occurred_at, rec.created_at,
        rec.attempt_count, rec.next_attempt_at, rec.lease_owner, rec.lease_expires_at, rec.last_error,
        rec.published_at);
END $$;

SELECT pg_temp.expect_ok('outbox insert',
    $q$ SELECT pg_temp.ins_event(jsonb_build_object('event_id', pg_temp.u(500))) $q$);
SELECT pg_temp.expect_error('outbox duplicate event id',
    $q$ SELECT pg_temp.ins_event(jsonb_build_object('event_id', pg_temp.u(500))) $q$, '23505', 'outbox_events_pkey');
SELECT pg_temp.expect_error('outbox payload must be an object',
    $q$ SELECT pg_temp.ins_event('{"payload":[1]}') $q$, '23514', 'outbox_events_payload_check');
SELECT pg_temp.expect_error('outbox blank aggregate',
    $q$ SELECT pg_temp.ins_event('{"aggregate_id":""}') $q$, '23514', 'outbox_events_text_check');
SELECT pg_temp.expect_error('outbox lease needs expiry',
    $q$ SELECT pg_temp.ins_event('{"lease_owner":"p1"}') $q$, '23514', 'outbox_events_lease_check');
SELECT pg_temp.expect_error('outbox payload immutable',
    $q$ UPDATE outbox_events SET payload = '{"eventId":"y"}' WHERE event_id = pg_temp.u(500) $q$,
    '23001', 'outbox_events_immutable');
SELECT pg_temp.expect_error('outbox event id immutable',
    $q$ UPDATE outbox_events SET event_id = pg_temp.u(501) WHERE event_id = pg_temp.u(500) $q$,
    '23001', 'outbox_events_immutable');
SELECT pg_temp.expect_error('outbox attempts cannot decrease',
    $q$ UPDATE outbox_events SET attempt_count = 3 WHERE event_id = pg_temp.u(500);
        UPDATE outbox_events SET attempt_count = 2 WHERE event_id = pg_temp.u(500) $q$,
    '23514', 'outbox_events_attempt_count_monotonic');
SELECT pg_temp.expect_error('unpublished outbox delete',
    $q$ DELETE FROM outbox_events WHERE event_id = pg_temp.u(500) $q$, '23001', 'outbox_events_no_delete');
SELECT pg_temp.expect_ok('outbox claim with lease (SKIP LOCKED)',
    $q$ UPDATE outbox_events o SET lease_owner = 'pub-1', lease_expires_at = now() + interval '30 seconds',
            attempt_count = attempt_count + 1
        WHERE o.event_id IN (SELECT event_id FROM outbox_events
                              WHERE published_at IS NULL AND next_attempt_at <= now()
                                AND (lease_expires_at IS NULL OR lease_expires_at < now())
                              ORDER BY next_attempt_at, seq LIMIT 10 FOR UPDATE SKIP LOCKED) $q$);
SELECT pg_temp.expect_error('published event cannot keep a lease',
    $q$ UPDATE outbox_events SET published_at = now() WHERE event_id = pg_temp.u(500) $q$,
    '23514', 'outbox_events_published_check');
SELECT pg_temp.expect_ok('publish',
    $q$ UPDATE outbox_events SET published_at = now(), lease_owner = NULL, lease_expires_at = NULL
        WHERE event_id = pg_temp.u(500) $q$);
SELECT pg_temp.expect_error('published event cannot be reopened',
    $q$ UPDATE outbox_events SET published_at = NULL WHERE event_id = pg_temp.u(500) $q$,
    '23001', 'outbox_events_immutable');
SELECT pg_temp.expect_ok('published event may be pruned',
    $q$ DELETE FROM outbox_events WHERE event_id = pg_temp.u(500) $q$);

\echo schema constraint tests passed
