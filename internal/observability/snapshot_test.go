package observability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
)

type backlogRow struct {
	count  int64
	oldest time.Time
	err    error
}

func (r backlogRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.count
	*dest[1].(**time.Time) = &r.oldest
	return nil
}

type backlogStore struct{ row backlogRow }

func (s backlogStore) QueryRow(context.Context, string, ...any) pgx.Row { return s.row }

type queueSnapshot struct {
	visible string
	err     error
}

func (q queueSnapshot) GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	return &sqs.GetQueueUrlOutput{QueueUrl: aws.String("http://local/dlq")}, nil
}
func (q queueSnapshot) GetQueueAttributes(_ context.Context, in *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	if q.err != nil {
		return nil, q.err
	}
	if aws.ToString(in.QueueUrl) == "http://local/dlq" {
		return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{string(types.QueueAttributeNameApproximateNumberOfMessages): q.visible}}, nil
	}
	return &sqs.GetQueueAttributesOutput{Attributes: map[string]string{string(types.QueueAttributeNameRedrivePolicy): `{"deadLetterTargetArn":"arn:aws:sqs:us-east-1:000000000000:dlq"}`}}, nil
}
func TestSnapshotPollTracksBacklogAgeAndActualDLQ(t *testing.T) {
	m := NewMetrics()
	s := &Snapshot{Metrics: m, Store: backlogStore{backlogRow{count: 4, oldest: time.Now().Add(-5 * time.Second)}}, SQS: queueSnapshot{visible: "2"}, QueueURL: "http://local/inbound"}
	if err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.OutboxUnpublished.Load() != 4 || m.OutboxOldestAgeMillis.Load() < 4900 || m.DLQVisible.Load() != 2 {
		t.Fatalf("snapshot: %d %d %d", m.OutboxUnpublished.Load(), m.OutboxOldestAgeMillis.Load(), m.DLQVisible.Load())
	}
	s.Store = backlogStore{row: backlogRow{err: errors.New("db down")}}
	if err := s.Poll(context.Background()); err == nil || m.OutboxUnpublished.Load() != 4 {
		t.Fatal("failed poll changed last good count")
	}
}
