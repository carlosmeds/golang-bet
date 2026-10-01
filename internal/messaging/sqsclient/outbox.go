package sqsclient

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/config"
	"wagering/internal/workers/outbox"
)

// OutboxSender maps immutable outbox envelopes to the configured FIFO queue.
type OutboxSender struct {
	Client   *sqs.Client
	QueueURL string
}

func NewOutboxSender(c config.Config, client *sqs.Client) (*OutboxSender, error) {
	if c.EventQueueURL == "" || client == nil {
		return nil, errors.New("event queue URL and SQS client required")
	}
	return &OutboxSender{Client: client, QueueURL: c.EventQueueURL}, nil
}
func (s *OutboxSender) Send(ctx context.Context, m outbox.Message) error {
	_, err := s.Client.SendMessage(ctx, &sqs.SendMessageInput{QueueUrl: aws.String(s.QueueURL), MessageBody: aws.String(string(m.Body)), MessageGroupId: aws.String(m.MessageGroupID), MessageDeduplicationId: aws.String(m.MessageDeduplicationID), MessageAttributes: map[string]types.MessageAttributeValue{
		"eventId":   {DataType: aws.String("String"), StringValue: aws.String(m.EventID.String())},
		"eventType": {DataType: aws.String("String"), StringValue: aws.String(string(m.EventType))},
	}})
	return err
}
