package domain

import (
	"time"
)

// EventType names a typed domain event.
type EventType string

const (
	// EventWagerTransactionProcessed fires when a transaction reaches
	// PROCESSED, including LOSS (no balance change) and OPENING.
	EventWagerTransactionProcessed EventType = "WagerTransactionProcessed"
	// EventWagerTransactionRejected fires when a transaction reaches REJECTED.
	EventWagerTransactionRejected EventType = "WagerTransactionRejected"
	// EventWalletBalanceChanged fires with every ledger entry.
	EventWalletBalanceChanged EventType = "WalletBalanceChanged"
	// EventWagerTransactionPendingReference fires once, when a transaction
	// first moves to PENDING_REFERENCE.
	EventWagerTransactionPendingReference EventType = "WagerTransactionPendingReference"
)

// EventVersion is the schema version set by every event constructor.
const EventVersion = 1

// EventMeta is the envelope header shared by all events (REQ-072). The
// aggregate is the wallet, so events of one wallet share an ordering group.
// OccurredAt is UTC and serializes as RFC3339.
type EventMeta struct {
	EventID       UUID      `json:"eventId"`
	EventType     EventType `json:"eventType"`
	AggregateID   UUID      `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
}

// Envelope is a typed event: header plus typed data.
type Envelope[T any] struct {
	EventMeta
	Data T `json:"data"`
}

// Meta returns the envelope header.
func (e Envelope[T]) Meta() EventMeta { return e.EventMeta }

// Event is implemented by every Envelope; it lets the outbox and use cases
// handle events of different payload types uniformly.
type Event interface {
	Meta() EventMeta
}

// EventContext carries the header values a use case supplies to event
// constructors. CausationID is optional (for example the transaction ID or
// the inbox message ID that caused the event).
type EventContext struct {
	EventID       UUID
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

func (c EventContext) meta(t EventType, aggregate UUID) (EventMeta, error) {
	if err := validateUUID("event id", c.EventID); err != nil {
		return EventMeta{}, err
	}
	if err := validateUUID("aggregate id", aggregate); err != nil {
		return EventMeta{}, err
	}
	if err := validateText("correlation id", c.CorrelationID); err != nil {
		return EventMeta{}, err
	}
	if c.CausationID != "" {
		if err := validateText("causation id", c.CausationID); err != nil {
			return EventMeta{}, err
		}
	}
	if c.OccurredAt.IsZero() {
		return EventMeta{}, newInvalid(CodeInvalidEvent, "event time is required")
	}
	return EventMeta{
		EventID: c.EventID, EventType: t, AggregateID: aggregate, CorrelationID: c.CorrelationID,
		CausationID: c.CausationID, OccurredAt: c.OccurredAt.UTC(), Version: EventVersion,
	}, nil
}

// TransactionData are the identifying fields shared by the transaction events.
// External identity fields are omitted for OPENING.
type TransactionData struct {
	TransactionID                  UUID   `json:"transactionId"`
	WalletID                       UUID   `json:"walletId"`
	PlayerID                       string `json:"playerId"`
	Kind                           Kind   `json:"kind"`
	ProviderID                     string `json:"providerId,omitempty"`
	ExternalTransactionID          string `json:"externalTransactionId,omitempty"`
	RoundID                        string `json:"roundId,omitempty"`
	GameID                         string `json:"gameId,omitempty"`
	ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
	Money                          Money  `json:"money"`
}

func transactionData(t *WagerTransaction) TransactionData {
	s := t.s
	return TransactionData{
		TransactionID: s.ID, WalletID: s.WalletID, PlayerID: s.PlayerID, Kind: s.Kind,
		ProviderID: s.ProviderID, ExternalTransactionID: s.ExternalTransactionID,
		RoundID: s.RoundID, GameID: s.GameID,
		ReferenceExternalTransactionID: s.ReferenceExternalTransactionID, Money: s.Money,
	}
}

// WagerTransactionProcessedData is the payload of WagerTransactionProcessed.
type WagerTransactionProcessedData struct {
	TransactionData
	ResultBalance Money `json:"resultBalance"`
}

// WagerTransactionRejectedData is the payload of WagerTransactionRejected.
// ObservedBalance is omitted when the wallet balance was not known.
type WagerTransactionRejectedData struct {
	TransactionData
	FailureCode     FailureCode `json:"failureCode"`
	ObservedBalance *Money      `json:"observedBalance,omitempty"`
}

// WagerTransactionPendingReferenceData is the payload of
// WagerTransactionPendingReference.
type WagerTransactionPendingReferenceData struct {
	TransactionData
	AttemptCount  int       `json:"attemptCount"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
}

// WalletBalanceChangedData is the payload of WalletBalanceChanged (REQ-073).
type WalletBalanceChangedData struct {
	WalletID      UUID      `json:"walletId"`
	TransactionID UUID      `json:"transactionId"`
	Direction     Direction `json:"direction"`
	Money         Money     `json:"money"`
	BalanceBefore Money     `json:"balanceBefore"`
	BalanceAfter  Money     `json:"balanceAfter"`
	WalletVersion int64     `json:"walletVersion"`
}

func requireStatus(t *WagerTransaction, want Status, event EventType) error {
	if t.s.Status != want {
		return newInvalid(CodeInvalidEvent, "%s requires a %s transaction, got %s", event, want, t.s.Status)
	}
	return nil
}

// NewWagerTransactionProcessed builds the event for a PROCESSED transaction.
func NewWagerTransactionProcessed(c EventContext, t *WagerTransaction) (Envelope[WagerTransactionProcessedData], error) {
	var zero Envelope[WagerTransactionProcessedData]
	if err := requireStatus(t, StatusProcessed, EventWagerTransactionProcessed); err != nil {
		return zero, err
	}
	m, err := c.meta(EventWagerTransactionProcessed, t.s.WalletID)
	if err != nil {
		return zero, err
	}
	return Envelope[WagerTransactionProcessedData]{EventMeta: m, Data: WagerTransactionProcessedData{
		TransactionData: transactionData(t), ResultBalance: t.s.ResultBalance,
	}}, nil
}

// NewWagerTransactionRejected builds the event for a REJECTED transaction.
func NewWagerTransactionRejected(c EventContext, t *WagerTransaction) (Envelope[WagerTransactionRejectedData], error) {
	var zero Envelope[WagerTransactionRejectedData]
	if err := requireStatus(t, StatusRejected, EventWagerTransactionRejected); err != nil {
		return zero, err
	}
	m, err := c.meta(EventWagerTransactionRejected, t.s.WalletID)
	if err != nil {
		return zero, err
	}
	d := WagerTransactionRejectedData{TransactionData: transactionData(t), FailureCode: t.s.FailureCode}
	if t.s.ResultBalance.IsInitialized() {
		b := t.s.ResultBalance
		d.ObservedBalance = &b
	}
	return Envelope[WagerTransactionRejectedData]{EventMeta: m, Data: d}, nil
}

// NewWagerTransactionPendingReference builds the event for a transaction that
// just entered PENDING_REFERENCE.
func NewWagerTransactionPendingReference(c EventContext, t *WagerTransaction) (Envelope[WagerTransactionPendingReferenceData], error) {
	var zero Envelope[WagerTransactionPendingReferenceData]
	if err := requireStatus(t, StatusPendingReference, EventWagerTransactionPendingReference); err != nil {
		return zero, err
	}
	m, err := c.meta(EventWagerTransactionPendingReference, t.s.WalletID)
	if err != nil {
		return zero, err
	}
	return Envelope[WagerTransactionPendingReferenceData]{EventMeta: m, Data: WagerTransactionPendingReferenceData{
		TransactionData: transactionData(t), AttemptCount: t.s.AttemptCount, NextAttemptAt: t.s.NextAttemptAt.UTC(),
	}}, nil
}

// NewWalletBalanceChanged builds the event for one wallet Movement.
func NewWalletBalanceChanged(c EventContext, walletID, transactionID UUID, mv Movement) (Envelope[WalletBalanceChangedData], error) {
	var zero Envelope[WalletBalanceChangedData]
	if err := validateUUID("transaction id", transactionID); err != nil {
		return zero, err
	}
	if !mv.Direction.valid() || !mv.Amount.IsPositive() || !mv.Before.IsInitialized() || !mv.After.IsInitialized() || mv.Version < 1 {
		return zero, newInvalid(CodeInvalidEvent, "balance change requires a direction, positive amount, balances and version")
	}
	m, err := c.meta(EventWalletBalanceChanged, walletID)
	if err != nil {
		return zero, err
	}
	return Envelope[WalletBalanceChangedData]{EventMeta: m, Data: WalletBalanceChangedData{
		WalletID: walletID, TransactionID: transactionID, Direction: mv.Direction, Money: mv.Amount,
		BalanceBefore: mv.Before, BalanceAfter: mv.After, WalletVersion: mv.Version,
	}}, nil
}
