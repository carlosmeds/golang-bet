package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"wagering/internal/domain"
)

// StoredTransaction carries SQL-only recovery and reference metadata alongside
// the validated domain transaction. Nullable fields remain explicit.
type StoredTransaction struct {
	Transaction         *domain.WagerTransaction
	ReferenceID         *domain.UUID
	ReferenceKind       domain.Kind
	ResultWalletVersion *int64
	ReferenceDeadline   *time.Time
	LeaseOwner          string
	LeaseExpiresAt      *time.Time
}

const transactionSelect = `SELECT id::text,origin,provider_id,external_transaction_id,idempotency_key,request_hash,wallet_id::text,player_id,kind,round_id,game_id,amount,currency,reference_external_transaction_id,reference_transaction_id::text,reference_kind,status,failure_code,result_balance_amount,result_balance_currency,result_wallet_version,attempt_count,next_attempt_at,reference_deadline_at,lease_owner,lease_expires_at,created_at,updated_at,completed_at FROM wager_transactions`

func scanTransaction(row pgx.Row) (*StoredTransaction, error) {
	var id, wallet, origin, player, kind, status, currency string
	var provider, external, key, hash, round, game, refExternal, refID, refKind, failure, resultCurrency, lease *string
	var amount int64
	var resultAmount, resultVersion *int64
	var attempts int
	var next, deadline, leaseExpiry, completed *time.Time
	var created, updated time.Time
	err := row.Scan(&id, &origin, &provider, &external, &key, &hash, &wallet, &player, &kind, &round, &game, &amount, &currency, &refExternal, &refID, &refKind, &status, &failure, &resultAmount, &resultCurrency, &resultVersion, &attempts, &next, &deadline, &lease, &leaseExpiry, &created, &updated, &completed)
	if err != nil {
		return nil, classify(err)
	}
	tid, err := domain.ParseUUID(id)
	if err != nil {
		return nil, err
	}
	wid, err := domain.ParseUUID(wallet)
	if err != nil {
		return nil, err
	}
	cur, err := domain.ParseCurrency(currency)
	if err != nil {
		return nil, err
	}
	money, err := domain.NewMoney(amount, cur)
	if err != nil {
		return nil, err
	}
	state := domain.WagerTransactionState{ID: tid, Origin: domain.Origin(origin), Kind: domain.Kind(kind), Status: domain.Status(status), ProviderID: value(provider), ExternalTransactionID: value(external), IdempotencyKey: value(key), RequestHash: value(hash), RoundID: value(round), GameID: value(game), ReferenceExternalTransactionID: value(refExternal), PlayerID: player, WalletID: wid, Money: money, FailureCode: domain.FailureCode(value(failure)), AttemptCount: attempts, CreatedAt: created, UpdatedAt: updated}
	if resultAmount != nil && resultCurrency != nil {
		rc, e := domain.ParseCurrency(*resultCurrency)
		if e != nil {
			return nil, e
		}
		state.ResultBalance, e = domain.NewMoney(*resultAmount, rc)
		if e != nil {
			return nil, e
		}
	}
	if next != nil && state.Status != domain.StatusPending {
		state.NextAttemptAt = *next
	}
	if completed != nil {
		state.CompletedAt = *completed
	}
	tx, err := domain.RehydrateWagerTransaction(state)
	if err != nil {
		return nil, err
	}
	out := &StoredTransaction{Transaction: tx, ReferenceKind: domain.Kind(value(refKind)), ResultWalletVersion: resultVersion, ReferenceDeadline: deadline, LeaseOwner: value(lease), LeaseExpiresAt: leaseExpiry}
	if refID != nil {
		parsed, e := domain.ParseUUID(*refID)
		if e != nil {
			return nil, e
		}
		out.ReferenceID = &parsed
	}
	return out, nil
}
func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func moneyOrNil(m domain.Money) any {
	if !m.IsInitialized() {
		return nil
	}
	return m.Minor()
}
func currencyOrNil(m domain.Money) any {
	if !m.IsInitialized() {
		return nil
	}
	return m.Currency().String()
}
func uuidOrNil(id *domain.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}
func ptrOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// InsertTransaction inserts a PENDING row with a durable takeover schedule. A
// domain PENDING object has no retry timestamp, so the SQL schedule is supplied
// independently. Opening rows are terminal and omit it.
func (tx *Tx) InsertTransaction(ctx context.Context, t *domain.WagerTransaction, schedule time.Time, referenceDeadline *time.Time) error {
	s := t.State()
	var internalKey any
	if s.Origin == domain.OriginInternal {
		internalKey = "opening:" + s.WalletID.String()
	}
	if s.Status == domain.StatusPending && schedule.IsZero() {
		schedule = s.CreatedAt
	}
	_, err := tx.Exec(ctx, `INSERT INTO wager_transactions(id,origin,internal_key,provider_id,external_transaction_id,idempotency_key,request_hash,wallet_id,player_id,kind,round_id,game_id,amount,currency,reference_external_transaction_id,status,failure_code,result_balance_amount,result_balance_currency,attempt_count,next_attempt_at,reference_deadline_at,created_at,updated_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`, s.ID.String(), string(s.Origin), internalKey, nullable(s.ProviderID), nullable(s.ExternalTransactionID), nullable(s.IdempotencyKey), nullable(s.RequestHash), s.WalletID.String(), s.PlayerID, string(s.Kind), nullable(s.RoundID), nullable(s.GameID), s.Money.Minor(), s.Money.Currency().String(), nullable(s.ReferenceExternalTransactionID), string(s.Status), nullable(string(s.FailureCode)), moneyOrNil(s.ResultBalance), currencyOrNil(s.ResultBalance), s.AttemptCount, timeOrNil(schedule), ptrOrNil(referenceDeadline), s.CreatedAt, s.UpdatedAt, timeOrNil(s.CompletedAt))
	return classify(err)
}

// UpdateTransaction only changes lifecycle and recovery columns. Identity is
// immutable in SQL; terminal rows cannot be updated.
func (tx *Tx) UpdateTransaction(ctx context.Context, st StoredTransaction) error {
	s := st.Transaction.State()
	tag, err := tx.Exec(ctx, `UPDATE wager_transactions SET status=$2,failure_code=$3,result_balance_amount=$4,result_balance_currency=$5,result_wallet_version=$6,reference_transaction_id=$7,reference_kind=$8,attempt_count=$9,next_attempt_at=$10,reference_deadline_at=$11,lease_owner=NULL,lease_expires_at=NULL,completed_at=$12 WHERE id=$1 AND status IN ('PENDING','PENDING_REFERENCE')`, s.ID.String(), string(s.Status), nullable(string(s.FailureCode)), moneyOrNil(s.ResultBalance), currencyOrNil(s.ResultBalance), ptrOrNil(st.ResultWalletVersion), uuidOrNil(st.ReferenceID), nullable(string(st.ReferenceKind)), s.AttemptCount, timeOrNil(s.NextAttemptAt), ptrOrNil(st.ReferenceDeadline), timeOrNil(s.CompletedAt))
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}
func (tx *Tx) GetTransaction(ctx context.Context, id domain.UUID) (*StoredTransaction, error) {
	return scanTransaction(tx.QueryRow(ctx, transactionSelect+` WHERE id=$1`, id.String()))
}
func (tx *Tx) LockTransaction(ctx context.Context, id domain.UUID) (*StoredTransaction, error) {
	return scanTransaction(tx.QueryRow(ctx, transactionSelect+` WHERE id=$1 FOR UPDATE`, id.String()))
}
func (tx *Tx) FindByIdempotency(ctx context.Context, provider, key string) (*StoredTransaction, error) {
	return scanTransaction(tx.QueryRow(ctx, transactionSelect+` WHERE provider_id=$1 AND idempotency_key=$2`, provider, key))
}
func (tx *Tx) FindByExternalID(ctx context.Context, provider, external string) (*StoredTransaction, error) {
	return scanTransaction(tx.QueryRow(ctx, transactionSelect+` WHERE provider_id=$1 AND external_transaction_id=$2`, provider, external))
}
func (tx *Tx) FindProcessedReversal(ctx context.Context, id domain.UUID) (*StoredTransaction, error) {
	return scanTransaction(tx.QueryRow(ctx, transactionSelect+` WHERE reference_transaction_id=$1 AND status='PROCESSED' AND kind IN ('REFUND','ROLLBACK')`, id.String()))
}

// ClaimTransactions reserves due work for one instance; an expired lease is
// claimable by another instance after a crash.
func (s *Store) ClaimTransactions(ctx context.Context, owner string, limit int, lease time.Duration) ([]domain.UUID, error) {
	if owner == "" || limit < 1 || lease <= 0 {
		return nil, ErrInvalidClaim
	}
	rows, err := s.Pool.Query(ctx, `WITH due AS (SELECT id FROM wager_transactions WHERE status IN ('PENDING','PENDING_REFERENCE') AND next_attempt_at<=now() AND (lease_expires_at IS NULL OR lease_expires_at<=now()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT $2) UPDATE wager_transactions t SET lease_owner=$1,lease_expires_at=now()+($3 * interval '1 microsecond') FROM due WHERE t.id=due.id RETURNING t.id::text`, owner, limit, lease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.UUID
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := domain.ParseUUID(raw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
