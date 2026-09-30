package pg

import (
	"context"
	"time"
	"wagering/internal/domain"
)

func (tx *Tx) InsertInbox(ctx context.Context, m *domain.InboxMessage) error {
	s := m.State()
	_, err := tx.Exec(ctx, `INSERT INTO inbox_messages(consumer,message_id,payload_hash,received_at,completed_at) VALUES($1,$2,$3,$4,$5)`, s.ConsumerName, s.MessageID, s.RequestHash, s.ReceivedAt, timeOrNil(s.CompletedAt))
	return classify(err)
}
func (tx *Tx) GetInbox(ctx context.Context, consumer, messageID string) (*domain.InboxMessage, *domain.UUID, error) {
	var name, id, hash string
	var received time.Time
	var completed *time.Time
	var transaction *string
	err := tx.QueryRow(ctx, `SELECT consumer,message_id,payload_hash,transaction_id::text,received_at,completed_at FROM inbox_messages WHERE consumer=$1 AND message_id=$2`, consumer, messageID).Scan(&name, &id, &hash, &transaction, &received, &completed)
	if err != nil {
		return nil, nil, classify(err)
	}
	state := domain.InboxMessageState{ConsumerName: name, MessageID: id, RequestHash: hash, ReceivedAt: received}
	if completed != nil {
		state.CompletedAt = *completed
	}
	m, err := domain.RehydrateInboxMessage(state)
	if err != nil {
		return nil, nil, err
	}
	if transaction == nil {
		return m, nil, nil
	}
	tid, err := domain.ParseUUID(*transaction)
	return m, &tid, err
}
func (tx *Tx) CompleteInbox(ctx context.Context, consumer, messageID string, transactionID domain.UUID, completed time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE inbox_messages SET transaction_id=$3,completed_at=$4 WHERE consumer=$1 AND message_id=$2 AND completed_at IS NULL`, consumer, messageID, transactionID.String(), completed)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return ErrConcurrentUpdate
	}
	return nil
}
