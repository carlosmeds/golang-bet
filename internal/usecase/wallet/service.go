// Package wallet opens wallets and their optional initial credit in one SQL
// transaction. Internal authorization is enforced by the transport.
package wallet

import (
	"context"
	"errors"
	"time"

	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	"wagering/internal/storage/schema"
)

var ErrAlreadyExists = errors.New("wallet already exists for player and currency")

type Service struct {
	Store *pg.Store
	Now   func() time.Time
	NewID domain.IDGenerator
}

func New(store *pg.Store) *Service { return &Service{Store: store} }
func (s *Service) Open(ctx context.Context, player string, initial domain.Money, correlation string) (domain.OpenWalletResult, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if correlation == "" {
		correlation = "wallet:" + player
	}
	result, err := domain.OpenWallet(domain.OpenWalletInput{PlayerID: player, InitialBalance: initial, CorrelationID: correlation, Now: now, NewID: s.NewID})
	if err != nil {
		return result, err
	}
	err = s.Store.WithTx(ctx, func(tx *pg.Tx) error {
		if err := tx.InsertWallet(ctx, result.Wallet); err != nil {
			return err
		}
		if result.Opening == nil {
			return nil
		}
		if err := tx.InsertTransaction(ctx, result.Opening, time.Time{}, nil); err != nil {
			return err
		}
		if err := tx.InsertLedger(ctx, result.Ledger, 1); err != nil {
			return err
		}
		for _, event := range result.Events {
			if err := tx.InsertOutbox(ctx, event, now); err != nil {
				return err
			}
		}
		return nil
	})
	var conflict *pg.Conflict
	if errors.As(err, &conflict) && conflict.Constraint == schema.UniqueWalletPlayerCurrency {
		return domain.OpenWalletResult{}, ErrAlreadyExists
	}
	if err != nil {
		return domain.OpenWalletResult{}, err
	}
	return result, nil
}
