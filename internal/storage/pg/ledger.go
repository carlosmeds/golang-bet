package pg

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
	"wagering/internal/domain"
)

// InsertLedger appends one immutable movement. The wallet version must equal
// the movement's resulting version and SQL validates the chain on commit.
func (tx *Tx) InsertLedger(ctx context.Context, e *domain.LedgerEntry, walletVersion int64) error {
	s := e.State()
	_, err := tx.Exec(ctx, `INSERT INTO ledger_entries(id,wallet_id,transaction_id,wallet_version,direction,amount,currency,balance_before,balance_after,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, s.ID.String(), s.WalletID.String(), s.TransactionID.String(), walletVersion, string(s.Direction), s.Amount.Minor(), s.Amount.Currency().String(), s.BalanceBefore.Minor(), s.BalanceAfter.Minor(), s.CreatedAt)
	return classify(err)
}

const ledgerSelect = `SELECT id::text,wallet_id::text,transaction_id::text,direction,amount,currency,balance_before,balance_after,created_at FROM ledger_entries`

func scanLedger(row pgx.Row) (*domain.LedgerEntry, error) {
	var id, wallet, transaction, direction, cur string
	var amount, before, after int64
	var created time.Time
	if err := row.Scan(&id, &wallet, &transaction, &direction, &amount, &cur, &before, &after, &created); err != nil {
		return nil, classify(err)
	}
	ids := make([]domain.UUID, 3)
	for i, raw := range []string{id, wallet, transaction} {
		parsed, err := domain.ParseUUID(raw)
		if err != nil {
			return nil, err
		}
		ids[i] = parsed
	}
	c, err := domain.ParseCurrency(cur)
	if err != nil {
		return nil, err
	}
	a, err := domain.NewMoney(amount, c)
	if err != nil {
		return nil, err
	}
	b, err := domain.NewMoney(before, c)
	if err != nil {
		return nil, err
	}
	z, err := domain.NewMoney(after, c)
	if err != nil {
		return nil, err
	}
	return domain.RehydrateLedgerEntry(domain.LedgerEntryState{ID: ids[0], WalletID: ids[1], TransactionID: ids[2], Direction: domain.Direction(direction), Amount: a, BalanceBefore: b, BalanceAfter: z, CreatedAt: created})
}
func (tx *Tx) ListLedger(ctx context.Context, wallet domain.UUID) ([]*domain.LedgerEntry, error) {
	rows, err := tx.Query(ctx, ledgerSelect+` WHERE wallet_id=$1 ORDER BY wallet_version`, wallet.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*domain.LedgerEntry
	for rows.Next() {
		entry, err := scanLedger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}
