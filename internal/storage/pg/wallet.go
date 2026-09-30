package pg

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
	"wagering/internal/domain"
)

const walletSelect = `SELECT id::text, player_id, currency, balance_amount, version, created_at, updated_at FROM wallets`

func scanWallet(row pgx.Row) (*domain.Wallet, error) {
	var id, player, cur string
	var minor, version int64
	var created, updatedTime time.Time
	err := row.Scan(&id, &player, &cur, &minor, &version, &created, &updatedTime)
	if err != nil {
		return nil, classify(err)
	}
	uid, err := domain.ParseUUID(id)
	if err != nil {
		return nil, err
	}
	currency, err := domain.ParseCurrency(cur)
	if err != nil {
		return nil, err
	}
	money, err := domain.NewMoney(minor, currency)
	if err != nil {
		return nil, err
	}
	return domain.RehydrateWallet(domain.WalletState{ID: uid, PlayerID: player, Balance: money, Version: version, CreatedAt: created, UpdatedAt: updatedTime})
}
func (tx *Tx) InsertWallet(ctx context.Context, w *domain.Wallet) error {
	s := w.State()
	_, err := tx.Exec(ctx, `INSERT INTO wallets(id,player_id,currency,balance_amount,version,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, s.ID.String(), s.PlayerID, s.Balance.Currency().String(), s.Balance.Minor(), s.Version, s.CreatedAt, s.UpdatedAt)
	return classify(err)
}
func (tx *Tx) GetWallet(ctx context.Context, id domain.UUID) (*domain.Wallet, error) {
	return scanWallet(tx.QueryRow(ctx, walletSelect+` WHERE id=$1`, id.String()))
}
func (tx *Tx) LockWallet(ctx context.Context, id domain.UUID) (*domain.Wallet, error) {
	return scanWallet(tx.QueryRow(ctx, walletSelect+` WHERE id=$1 FOR UPDATE`, id.String()))
}
func (tx *Tx) GetWalletByPlayerCurrency(ctx context.Context, player string, currency domain.Currency) (*domain.Wallet, error) {
	return scanWallet(tx.QueryRow(ctx, walletSelect+` WHERE player_id=$1 AND currency=$2`, player, currency.String()))
}
func (tx *Tx) UpdateWallet(ctx context.Context, w *domain.Wallet, expectedVersion int64) error {
	s := w.State()
	tag, err := tx.Exec(ctx, `UPDATE wallets SET balance_amount=$2,version=$3 WHERE id=$1 AND version=$4`, s.ID.String(), s.Balance.Minor(), s.Version, expectedVersion)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}

// LockWalletByPlayerCurrency serializes all effects against one aggregate.
func (tx *Tx) LockWalletByPlayerCurrency(ctx context.Context, player string, currency domain.Currency) (*domain.Wallet, error) {
	return scanWallet(tx.QueryRow(ctx, walletSelect+` WHERE player_id=$1 AND currency=$2 FOR UPDATE`, player, currency.String()))
}
