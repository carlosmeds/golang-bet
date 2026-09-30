package domain

import "time"

// InboxMessage is the durable record of an SQS message consumed by one
// consumer, unique by (consumer, messageId) (REQ-031). It stores the hash of
// the message body so a redelivery with different content is detected.
type InboxMessage struct {
	s InboxMessageState
}

// InboxMessageState is the persisted shape. A zero CompletedAt means the
// message is still being processed.
type InboxMessageState struct {
	ConsumerName string
	MessageID    string
	RequestHash  string
	ReceivedAt   time.Time
	CompletedAt  time.Time
}

// NewInboxMessage records receipt of messageID by consumer.
func NewInboxMessage(consumer, messageID, hash string, now time.Time) (*InboxMessage, error) {
	return RehydrateInboxMessage(InboxMessageState{ConsumerName: consumer, MessageID: messageID, RequestHash: hash, ReceivedAt: now})
}

// RehydrateInboxMessage rebuilds an inbox record from stored state.
func RehydrateInboxMessage(s InboxMessageState) (*InboxMessage, error) {
	if err := validateText("consumer name", s.ConsumerName); err != nil {
		return nil, err
	}
	if err := validateText("message id", s.MessageID); err != nil {
		return nil, err
	}
	if err := validateHash("message hash", s.RequestHash); err != nil {
		return nil, err
	}
	if s.ReceivedAt.IsZero() {
		return nil, newInvalid(CodeInvariantViolation, "inbox receive time is required")
	}
	s.ReceivedAt = s.ReceivedAt.UTC()
	if !s.CompletedAt.IsZero() {
		s.CompletedAt = s.CompletedAt.UTC()
		if s.CompletedAt.Before(s.ReceivedAt) {
			return nil, newInvalid(CodeInvariantViolation, "inbox completion precedes receipt")
		}
	}
	return &InboxMessage{s: s}, nil
}

func (m *InboxMessage) ConsumerName() string   { return m.s.ConsumerName }
func (m *InboxMessage) MessageID() string      { return m.s.MessageID }
func (m *InboxMessage) RequestHash() string    { return m.s.RequestHash }
func (m *InboxMessage) ReceivedAt() time.Time  { return m.s.ReceivedAt }
func (m *InboxMessage) CompletedAt() time.Time { return m.s.CompletedAt }

// IsCompleted reports whether the financial effect committed with this record.
func (m *InboxMessage) IsCompleted() bool { return !m.s.CompletedAt.IsZero() }

// State returns a copy for persistence.
func (m *InboxMessage) State() InboxMessageState { return m.s }

// CheckReplay returns ErrInboxHashConflict when a redelivery of the same
// messageId carries a different hash. The conflict is a permanent, auditable
// error: the message goes to the DLQ rather than being retried.
func (m *InboxMessage) CheckReplay(hash string) error {
	if m.s.RequestHash != hash {
		return &Error{Kind: ErrKindConflict, Code: CodeInboxHashConflict,
			Message: "message id " + m.s.MessageID + " redelivered with different content"}
	}
	return nil
}

// Complete marks the message processed; it is done once.
func (m *InboxMessage) Complete(now time.Time) error {
	if m.IsCompleted() {
		return newInvalid(CodeInvalidTransition, "inbox message %s is already completed", m.s.MessageID)
	}
	t := now.UTC()
	if t.Before(m.s.ReceivedAt) {
		return newInvalid(CodeInvariantViolation, "inbox completion precedes receipt")
	}
	m.s.CompletedAt = t
	return nil
}
