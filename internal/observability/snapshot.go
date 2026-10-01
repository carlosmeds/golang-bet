package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/storage/pg"
)

// Snapshot polls durable storage and SQS. A failed poll leaves the last good
// gauge values intact; it never turns a dependency outage into a false zero.
type Snapshot struct {
	Metrics *Metrics
	Store   interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
	SQS interface {
		GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
		GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	}
	QueueURL string
}

func NewSnapshot(m *Metrics, store *pg.Store, client *sqs.Client, cfg config.Config) *Snapshot {
	return &Snapshot{Metrics: m, Store: store.Pool, SQS: client, QueueURL: cfg.WagerQueueURL}
}

func (s *Snapshot) Poll(ctx context.Context) error {
	var count int64
	var oldest *time.Time
	err := s.Store.QueryRow(ctx, `SELECT count(*), min(created_at) FROM outbox_events WHERE published_at IS NULL`).Scan(&count, &oldest)
	if err != nil {
		return err
	}
	age := int64(0)
	if oldest != nil {
		age = max(0, time.Since(*oldest).Milliseconds())
	}
	s.Metrics.OutboxUnpublished.Store(count)
	s.Metrics.OutboxOldestAgeMillis.Store(age)

	attrs, err := s.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(s.QueueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameRedrivePolicy}})
	if err != nil {
		return err
	}
	var policy struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
	}
	if err := json.Unmarshal([]byte(attrs.Attributes[string(types.QueueAttributeNameRedrivePolicy)]), &policy); err != nil {
		return err
	}
	parts := strings.Split(policy.DeadLetterTargetArn, ":")
	if len(parts) < 6 || parts[5] == "" {
		return errors.New("SQS redrive target missing")
	}
	url, err := s.SQS.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(parts[5]), QueueOwnerAWSAccountId: aws.String(parts[4])})
	if err != nil {
		return err
	}
	dlq, err := s.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: url.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages}})
	if err != nil {
		return err
	}
	var visible int64
	value := dlq.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]
	if _, err := fmt.Sscan(value, &visible); err != nil {
		return err
	}
	s.Metrics.DLQVisible.Store(visible)
	return nil
}

func StartSnapshot(lc fx.Lifecycle, s *Snapshot) {
	var cancel context.CancelFunc
	var done chan struct{}
	lc.Append(fx.Hook{OnStart: func(context.Context) error {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		done = make(chan struct{})
		go func() {
			defer close(done)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				probe, stop := context.WithTimeout(ctx, 3*time.Second)
				err := s.Poll(probe)
				stop()
				if err != nil && ctx.Err() == nil {
					slog.Warn("metrics snapshot failed", "error", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		return nil
	}, OnStop: func(ctx context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
}
