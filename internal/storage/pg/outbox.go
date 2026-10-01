package pg

import (
	"context"
	"time"

	"wagering/internal/domain"
)

// ClaimedEvent is the exact immutable JSON snapshot to publish. The lease
// owner is required when acknowledging publication or scheduling a retry.
type ClaimedEvent struct {
	Seq          int64
	EventID      domain.UUID
	Payload      []byte
	LeaseOwner   string
	AttemptCount int
}

func (tx *Tx) InsertOutbox(ctx context.Context, e domain.Event, now time.Time) error {
	event, err := domain.NewOutboxEvent(e, now)
	if err != nil {
		return err
	}
	s := event.State()
	m := e.Meta()
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_id,aggregate_type,aggregate_id,event_type,event_version,correlation_id,causation_id,payload,occurred_at,created_at,next_attempt_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.EventID.String(), "wallet", s.AggregateID.String(), string(s.EventType), m.Version, m.CorrelationID, nullable(m.CausationID), s.Payload, s.OccurredAt, s.CreatedAt, s.NextAttemptAt)
	return classify(err)
}
func (s *Store) ClaimOutbox(ctx context.Context, owner string, limit int, lease time.Duration) ([]ClaimedEvent, error) {
	if owner == "" || limit < 1 || lease <= 0 {
		return nil, ErrInvalidClaim
	}
	rows, err := s.Pool.Query(ctx, `WITH due AS (SELECT candidate.event_id FROM outbox_events candidate WHERE candidate.published_at IS NULL AND candidate.next_attempt_at<=now() AND (candidate.lease_expires_at IS NULL OR candidate.lease_expires_at<=now()) AND NOT EXISTS (SELECT 1 FROM outbox_events older WHERE older.aggregate_type=candidate.aggregate_type AND older.aggregate_id=candidate.aggregate_id AND older.seq<candidate.seq AND older.published_at IS NULL) ORDER BY candidate.seq FOR UPDATE SKIP LOCKED LIMIT $2) UPDATE outbox_events o SET lease_owner=$1,lease_expires_at=now()+($3 * interval '1 microsecond') FROM due WHERE o.event_id=due.event_id RETURNING o.seq,o.event_id::text,o.payload,o.attempt_count`, owner, limit, lease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClaimedEvent
	for rows.Next() {
		var id string
		var c ClaimedEvent
		if err := rows.Scan(&c.Seq, &id, &c.Payload, &c.AttemptCount); err != nil {
			return nil, err
		}
		c.EventID, err = domain.ParseUUID(id)
		if err != nil {
			return nil, err
		}
		c.LeaseOwner = owner
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkPublished succeeds only for the instance that still owns a live lease.
func (s *Store) MarkPublished(ctx context.Context, event domain.UUID, owner string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE outbox_events SET published_at=now(),attempt_count=attempt_count+1,lease_owner=NULL,lease_expires_at=NULL WHERE event_id=$1 AND published_at IS NULL AND lease_owner=$2 AND lease_expires_at>now()`, event.String(), owner)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}
func (s *Store) RetryOutbox(ctx context.Context, event domain.UUID, owner string, after time.Duration, reason string) error {
	if after <= 0 {
		return ErrInvalidClaim
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE outbox_events SET attempt_count=attempt_count+1,next_attempt_at=now()+($3 * interval '1 microsecond'),last_error=$4,lease_owner=NULL,lease_expires_at=NULL WHERE event_id=$1 AND published_at IS NULL AND lease_owner=$2 AND lease_expires_at>now()`, event.String(), owner, after.Microseconds(), reason)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}
