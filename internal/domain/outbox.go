package domain

import (
	"bytes"
	"encoding/json"
	"time"
)

// OutboxEvent is a durable, immutable snapshot of a typed event awaiting
// publication (REQ-033). The payload is the full JSON envelope serialized when
// the event was created, so later code changes never alter a stored event. The
// eventId is stable across republication; consumers deduplicate on it.
type OutboxEvent struct {
	s OutboxEventState
}

// OutboxEventState is the persisted shape used for rehydration and snapshots.
// Zero times mean "absent".
type OutboxEventState struct {
	EventID       UUID
	AggregateID   UUID
	EventType     EventType
	Payload       []byte
	OccurredAt    time.Time
	Attempts      int
	NextAttemptAt time.Time
	PublishedAt   time.Time
	CreatedAt     time.Time
}

// NewOutboxEvent snapshots e for the outbox; it is due immediately.
func NewOutboxEvent(e Event, now time.Time) (*OutboxEvent, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return nil, newInvalid(CodeInvalidEvent, "event cannot be serialized: %v", err)
	}
	m := e.Meta()
	t := now.UTC()
	return RehydrateOutboxEvent(OutboxEventState{
		EventID: m.EventID, AggregateID: m.AggregateID, EventType: m.EventType, Payload: payload,
		OccurredAt: m.OccurredAt, NextAttemptAt: t, CreatedAt: t,
	})
}

// RehydrateOutboxEvent rebuilds an outbox record, verifying that the payload
// is a JSON envelope whose header matches the stored columns.
func RehydrateOutboxEvent(s OutboxEventState) (*OutboxEvent, error) {
	if err := validateUUID("event id", s.EventID); err != nil {
		return nil, err
	}
	if err := validateUUID("aggregate id", s.AggregateID); err != nil {
		return nil, err
	}
	if s.EventType == "" || s.CreatedAt.IsZero() || s.OccurredAt.IsZero() || s.Attempts < 0 {
		return nil, newInvalid(CodeInvalidEvent, "outbox event requires type, times and a non-negative attempt count")
	}
	var meta EventMeta
	dec := json.NewDecoder(bytes.NewReader(s.Payload))
	var header struct {
		EventMeta
		Data json.RawMessage `json:"data"`
	}
	if err := dec.Decode(&header); err != nil || len(header.Data) == 0 {
		return nil, newInvalid(CodeInvalidEvent, "outbox payload is not a JSON event envelope")
	}
	meta = header.EventMeta
	if meta.EventID != s.EventID || meta.AggregateID != s.AggregateID || meta.EventType != s.EventType || meta.Version < 1 {
		return nil, newInvalid(CodeInvalidEvent, "outbox payload header does not match the stored event")
	}
	s.Payload = append([]byte(nil), s.Payload...)
	s.OccurredAt, s.CreatedAt = s.OccurredAt.UTC(), s.CreatedAt.UTC()
	if !s.NextAttemptAt.IsZero() {
		s.NextAttemptAt = s.NextAttemptAt.UTC()
	}
	if !s.PublishedAt.IsZero() {
		s.PublishedAt = s.PublishedAt.UTC()
		if !s.NextAttemptAt.IsZero() {
			return nil, newInvalid(CodeInvariantViolation, "a published outbox event has no next attempt")
		}
	} else if s.NextAttemptAt.IsZero() {
		return nil, newInvalid(CodeInvariantViolation, "an unpublished outbox event requires a next attempt time")
	}
	return &OutboxEvent{s: s}, nil
}

func (o *OutboxEvent) EventID() UUID            { return o.s.EventID }
func (o *OutboxEvent) AggregateID() UUID        { return o.s.AggregateID }
func (o *OutboxEvent) EventType() EventType     { return o.s.EventType }
func (o *OutboxEvent) OccurredAt() time.Time    { return o.s.OccurredAt }
func (o *OutboxEvent) Attempts() int            { return o.s.Attempts }
func (o *OutboxEvent) NextAttemptAt() time.Time { return o.s.NextAttemptAt }
func (o *OutboxEvent) PublishedAt() time.Time   { return o.s.PublishedAt }

// IsPublished reports whether publication was confirmed.
func (o *OutboxEvent) IsPublished() bool { return !o.s.PublishedAt.IsZero() }

// Payload returns a copy of the immutable JSON snapshot.
func (o *OutboxEvent) Payload() []byte { return append([]byte(nil), o.s.Payload...) }

// State returns a copy for persistence.
func (o *OutboxEvent) State() OutboxEventState {
	s := o.s
	s.Payload = o.Payload()
	return s
}

// IsDue reports whether the event may be claimed for publication at now.
func (o *OutboxEvent) IsDue(now time.Time) bool {
	return !o.IsPublished() && !now.Before(o.s.NextAttemptAt)
}

// MarkPublished records a confirmed publication after the SQL commit.
func (o *OutboxEvent) MarkPublished(now time.Time) error {
	if o.IsPublished() {
		return newInvalid(CodeInvalidTransition, "outbox event %s is already published", o.s.EventID)
	}
	o.s.PublishedAt = now.UTC()
	o.s.NextAttemptAt = time.Time{}
	o.s.Attempts++
	return nil
}

// RecordPublishFailure counts a failed attempt and schedules the next one with
// exponential backoff.
func (o *OutboxEvent) RecordPublishFailure(now time.Time, policy RetryPolicy) error {
	if o.IsPublished() {
		return newInvalid(CodeInvalidTransition, "outbox event %s is already published", o.s.EventID)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	o.s.Attempts++
	o.s.NextAttemptAt = now.UTC().Add(policy.Backoff(o.s.Attempts))
	return nil
}
