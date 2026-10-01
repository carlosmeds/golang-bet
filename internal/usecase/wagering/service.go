// Package wagering is the single transactional entry point for provider
// operations arriving over HTTP or SQS and for reference retries.
package wagering

import (
	"context"
	"errors"
	"time"

	"wagering/internal/contract"
	"wagering/internal/domain"
	"wagering/internal/idempotency"
	"wagering/internal/storage/pg"
	"wagering/internal/storage/schema"
)

var ErrExternalIDConflict = errors.New("external transaction id reused with another idempotency key")

// ErrClaimNotHeld means a reference retry arrived without a live lease owned by
// the caller on a due row. It is returned before any write.
var ErrClaimNotHeld = errors.New("reference retry claim is not held by the caller or not due")
var ErrInboxIncomplete = errors.New("inbox receipt has no completed transaction")

type Inbox struct{ Consumer, MessageID, PayloadHash string }
type Result struct {
	Transaction   *domain.WagerTransaction
	WalletVersion *int64
	Replay        bool
}
type Service struct {
	Store *pg.Store
	Now   func() time.Time
	NewID domain.IDGenerator
	Retry domain.RetryPolicy
}

func New(store *pg.Store) *Service { return &Service{Store: store} }
func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) id() domain.UUID {
	if s.NewID != nil {
		return s.NewID()
	}
	return domain.NewUUID()
}
func (s *Service) policy() domain.RetryPolicy {
	if s.Retry != (domain.RetryPolicy{}) {
		return s.Retry
	}
	return domain.DefaultRetryPolicy()
}

// Execute commits the inbox receipt, financial result, ledger and event
// snapshots together. The provider is caller-authorized before entry.
func (s *Service) Execute(ctx context.Context, op contract.Operation, key, correlation string, inbox *Inbox) (Result, error) {
	if err := op.Validate(); err != nil {
		return Result{}, err
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return Result{}, err
	}
	hash, err := idempotency.Hash(op)
	if err != nil {
		return Result{}, err
	}
	if correlation == "" {
		correlation = op.ExternalTransactionID
	}
	if inbox != nil {
		if inbox.Consumer == "" || inbox.MessageID == "" || inbox.PayloadHash == "" {
			return Result{}, errors.New("inbox identity and hash required")
		}
	}
	var out Result
	err = s.Store.WithTx(ctx, func(tx *pg.Tx) error {
		if inbox != nil {
			existing, linked, e := tx.GetInbox(ctx, inbox.Consumer, inbox.MessageID)
			if e == nil {
				if e = existing.CheckReplay(inbox.PayloadHash); e != nil {
					return e
				}
				if !existing.IsCompleted() || linked == nil {
					return ErrInboxIncomplete
				}
				stored, e := tx.GetTransaction(ctx, *linked)
				if e != nil {
					return e
				}
				out = Result{Transaction: stored.Transaction, WalletVersion: stored.ResultWalletVersion, Replay: true}
				return nil
			}
			if !errors.Is(e, pg.ErrNotFound) {
				return e
			}
		}
		replay := func() (bool, error) {
			old, e := tx.FindByIdempotency(ctx, op.ProviderID, key)
			if errors.Is(e, pg.ErrNotFound) {
				return false, nil
			}
			if e != nil {
				return false, e
			}
			if e = old.Transaction.CheckReplay(hash); e != nil {
				return false, e
			}
			if old.Transaction.ExternalTransactionID() != op.ExternalTransactionID {
				return false, ErrExternalIDConflict
			}
			if inbox != nil {
				if e = completeReplayInbox(ctx, tx, *inbox, old.Transaction.ID(), s.now()); e != nil {
					return false, e
				}
			}
			out = Result{Transaction: old.Transaction, WalletVersion: old.ResultWalletVersion, Replay: true}
			return true, nil
		}
		if found, e := replay(); found || e != nil {
			return e
		}
		wallet, err := tx.LockWallet(ctx, op.WalletID)
		if err != nil {
			return err
		}
		// Recheck after acquiring the wallet lock: a same-wallet concurrent
		// request may have committed while this request waited.
		if found, e := replay(); found || e != nil {
			return e
		}
		byExternal, err := tx.FindByExternalID(ctx, op.ProviderID, op.ExternalTransactionID)
		if err == nil && byExternal != nil {
			return ErrExternalIDConflict
		}
		if !errors.Is(err, pg.ErrNotFound) {
			return err
		}
		now := s.now()
		txn, err := domain.NewExternalTransaction(domain.ExternalTransactionParams{ID: s.id(), ProviderID: op.ProviderID, ExternalTransactionID: op.ExternalTransactionID, IdempotencyKey: key, RequestHash: hash, Kind: op.Kind, PlayerID: op.PlayerID, WalletID: op.WalletID, RoundID: op.RoundID, GameID: op.GameID, Money: op.Money, ReferenceExternalTransactionID: op.ReferenceExternalTransactionID, Now: now})
		if err != nil {
			return err
		}
		if err = tx.InsertTransaction(ctx, txn, now, nil); err != nil {
			return err
		}
		stored, err := s.processLocked(ctx, tx, wallet, pg.StoredTransaction{Transaction: txn}, correlation, now)
		if err != nil {
			return err
		}
		if inbox != nil {
			if err = writeInbox(ctx, tx, *inbox, txn.ID(), now); err != nil {
				return err
			}
		}
		out = Result{Transaction: stored.Transaction, WalletVersion: stored.ResultWalletVersion}
		return nil
	})
	if err == nil {
		return out, nil
	}
	// A concurrent request for the same provider/key may have won while this
	// transaction was in progress. PostgreSQL aborted it; read the committed
	// snapshot in a fresh transaction and commit any SQS inbox identity with
	// the replay before its caller can acknowledge the message.
	var conflict *pg.Conflict
	if errors.As(err, &conflict) && conflict.Constraint == schema.UniqueTransactionIdempotencyKey {
		replayErr := s.Store.WithTx(ctx, func(tx *pg.Tx) error {
			old, e := tx.FindByIdempotency(ctx, op.ProviderID, key)
			if e != nil {
				return e
			}
			if e = old.Transaction.CheckReplay(hash); e != nil {
				return e
			}
			if old.Transaction.ExternalTransactionID() != op.ExternalTransactionID {
				return ErrExternalIDConflict
			}
			if inbox != nil {
				if e = completeReplayInbox(ctx, tx, *inbox, old.Transaction.ID(), s.now()); e != nil {
					return e
				}
			}
			out = Result{Transaction: old.Transaction, WalletVersion: old.ResultWalletVersion, Replay: true}
			return nil
		})
		if replayErr == nil {
			return out, nil
		}
		return Result{}, replayErr
	}
	if errors.As(err, &conflict) && conflict.Constraint == schema.UniqueTransactionExternalID {
		return Result{}, ErrExternalIDConflict
	}
	return Result{}, err
}
func completeReplayInbox(ctx context.Context, tx *pg.Tx, in Inbox, transaction domain.UUID, now time.Time) error {
	existing, linked, err := tx.GetInbox(ctx, in.Consumer, in.MessageID)
	if err == nil {
		if err = existing.CheckReplay(in.PayloadHash); err != nil {
			return err
		}
		if !existing.IsCompleted() || linked == nil || *linked != transaction {
			return ErrInboxIncomplete
		}
		return nil
	}
	if !errors.Is(err, pg.ErrNotFound) {
		return err
	}
	return writeInbox(ctx, tx, in, transaction, now)
}
func writeInbox(ctx context.Context, tx *pg.Tx, in Inbox, transaction domain.UUID, now time.Time) error {
	m, err := domain.NewInboxMessage(in.Consumer, in.MessageID, in.PayloadHash, now)
	if err != nil {
		return err
	}
	if err = tx.InsertInbox(ctx, m); err != nil {
		return err
	}
	return tx.CompleteInbox(ctx, in.Consumer, in.MessageID, transaction, now)
}

// RetryPending resumes one durably pending reference under its wallet lock. The
// caller must be the current lease owner (see pg.Store.ClaimTransactions) and
// the row must be due. A worker whose lease expired and was taken over, or a
// caller that never claimed the row, gets ErrClaimNotHeld and changes nothing:
// attempt_count, schedule, rejection and financial effects belong to the live
// claim. A row that already reached a final state is returned as a replay.
func (s *Service) RetryPending(ctx context.Context, id domain.UUID, owner string) (Result, error) {
	if owner == "" {
		return Result{}, pg.ErrInvalidClaim
	}
	var out Result
	err := s.Store.WithTx(ctx, func(tx *pg.Tx) error {
		peek, err := tx.GetTransaction(ctx, id)
		if err != nil {
			return err
		}
		wallet, err := tx.LockWallet(ctx, peek.Transaction.WalletID())
		if err != nil {
			return err
		}
		stored, err := tx.LockTransaction(ctx, id)
		if err != nil {
			return err
		}
		if stored.Transaction.Status().IsTerminal() {
			out = Result{Transaction: stored.Transaction, WalletVersion: stored.ResultWalletVersion, Replay: true}
			return nil
		}
		// Checked only after both locks: a competitor that finished or
		// rescheduled the row while this call waited has already cleared or
		// replaced the lease, so the check sees the committed state.
		held, due, err := tx.ClaimHeld(ctx, id, owner)
		if err != nil {
			return err
		}
		if !held || !due {
			return ErrClaimNotHeld
		}
		now := s.now()
		updated, err := s.processLocked(ctx, tx, wallet, *stored, id.String(), now)
		if err != nil {
			return err
		}
		out = Result{Transaction: updated.Transaction, WalletVersion: updated.ResultWalletVersion}
		return nil
	})
	return out, err
}

func (s *Service) processLocked(ctx context.Context, tx *pg.Tx, wallet *domain.Wallet, stored pg.StoredTransaction, correlation string, now time.Time) (pg.StoredTransaction, error) {
	txn := stored.Transaction
	oldVersion := wallet.Version()
	var ref *domain.Reference
	var refID *domain.UUID
	var refKind domain.Kind
	if txn.HasReference() {
		found, err := tx.FindByExternalID(ctx, txn.ProviderID(), txn.ReferenceExternalTransactionID())
		if err != nil && !errors.Is(err, pg.ErrNotFound) {
			return stored, err
		}
		if err == nil {
			refID = pointer(found.Transaction.ID())
			refKind = found.Transaction.Kind()
			reversals := domain.ReversalState{}
			reversal, e := tx.FindProcessedReversal(ctx, *refID)
			if e != nil && !errors.Is(e, pg.ErrNotFound) {
				return stored, e
			}
			if e == nil {
				reversals.Refunded = reversal.Transaction.Kind() == domain.KindRefund
				reversals.RolledBack = reversal.Transaction.Kind() == domain.KindRollback
			}
			snap := domain.ReferenceOf(found.Transaction, reversals)
			ref = &snap
		}
	}
	processed, err := domain.Process(domain.ProcessInput{Wallet: wallet, Tx: txn, Reference: ref, Now: now, Retry: s.policy(), CorrelationID: correlation, NewID: s.NewID})
	if err != nil {
		return stored, err
	}
	if processed.Movement != nil {
		if err = tx.UpdateWallet(ctx, wallet, oldVersion); err != nil {
			return stored, err
		}
		version := wallet.Version()
		stored.ResultWalletVersion = &version
	} else if txn.Status().IsTerminal() {
		version := wallet.Version()
		stored.ResultWalletVersion = &version
	}
	if processed.Outcome == domain.OutcomePending {
		deadline := txn.CreatedAt().Add(s.policy().TTL)
		stored.ReferenceDeadline = &deadline
	}
	if processed.Outcome == domain.OutcomeProcessed && refID != nil {
		stored.ReferenceID = refID
		stored.ReferenceKind = refKind
	}
	if err = tx.UpdateTransaction(ctx, stored); err != nil {
		return stored, err
	}
	if processed.Ledger != nil {
		if err = tx.InsertLedger(ctx, processed.Ledger, wallet.Version()); err != nil {
			return stored, err
		}
	}
	for _, event := range processed.Events {
		if err = tx.InsertOutbox(ctx, event, now); err != nil {
			return stored, err
		}
	}
	return stored, nil
}
func pointer[T any](v T) *T { return &v }
