package pg

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
	"wagering/internal/domain"
)

// LedgerPageItem exposes the stable per-wallet version for cursor pagination.
type LedgerPageItem struct {
	Entry         *domain.LedgerEntry
	WalletVersion int64
}

func scanLedgerPage(row pgx.Row) (LedgerPageItem, error) {
	var id, wallet, transaction, direction, cur string
	var version, amount, before, after int64
	var created time.Time
	err := row.Scan(&version, &id, &wallet, &transaction, &direction, &amount, &cur, &before, &after, &created)
	if err != nil {
		return LedgerPageItem{}, classify(err)
	}
	ids := make([]domain.UUID, 3)
	for i, raw := range []string{id, wallet, transaction} {
		parsed, e := domain.ParseUUID(raw)
		if e != nil {
			return LedgerPageItem{}, e
		}
		ids[i] = parsed
	}
	c, err := domain.ParseCurrency(cur)
	if err != nil {
		return LedgerPageItem{}, err
	}
	a, err := domain.NewMoney(amount, c)
	if err != nil {
		return LedgerPageItem{}, err
	}
	b, err := domain.NewMoney(before, c)
	if err != nil {
		return LedgerPageItem{}, err
	}
	z, err := domain.NewMoney(after, c)
	if err != nil {
		return LedgerPageItem{}, err
	}
	entry, err := domain.RehydrateLedgerEntry(domain.LedgerEntryState{ID: ids[0], WalletID: ids[1], TransactionID: ids[2], Direction: domain.Direction(direction), Amount: a, BalanceBefore: b, BalanceAfter: z, CreatedAt: created})
	if err != nil {
		return LedgerPageItem{}, err
	}
	return LedgerPageItem{Entry: entry, WalletVersion: version}, nil
}
func (tx *Tx) ListLedgerPage(ctx context.Context, wallet domain.UUID, after int64, limit int) ([]LedgerPageItem, error) {
	rows, err := tx.Query(ctx, `SELECT wallet_version,id::text,wallet_id::text,transaction_id::text,direction,amount,currency,balance_before,balance_after,created_at FROM ledger_entries WHERE wallet_id=$1 AND wallet_version>$2 ORDER BY wallet_version LIMIT $3`, wallet.String(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerPageItem
	for rows.Next() {
		item, e := scanLedgerPage(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (tx *Tx) CountLedger(ctx context.Context, wallet domain.UUID) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE wallet_id=$1`, wallet.String()).Scan(&n)
	return n, classify(err)
}
