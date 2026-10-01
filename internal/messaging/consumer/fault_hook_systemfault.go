//go:build systemfault

package consumer

import (
	"context"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// This hook exists only in binaries built with -tags=systemfault. It creates
// a deterministic crash window after the durable SQL commit and before SQS
// DeleteMessage. Production builds compile the no-op implementation instead.
func afterDurableCommit(ctx context.Context, api API, queue string, m types.Message) error {
	marker := os.Getenv("WAGERING_FAULT_AFTER_COMMIT_MARKER")
	if marker == "" {
		return nil
	}
	changeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := api.ChangeMessageVisibility(changeCtx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl: aws.String(queue), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 1,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(marker, []byte(aws.ToString(m.MessageId)), 0600); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}
