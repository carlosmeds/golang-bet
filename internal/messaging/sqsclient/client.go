// Package sqsclient supplies one AWS SDK v2 SQS client to inbound and outbound
// workers. Endpoint override permits the same code to use MiniStack locally.
package sqsclient

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/workers/outbox"
)

func New(c config.Config) (*sqs.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(c.AWSRegion))
	if err != nil {
		return nil, err
	}
	return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
		if c.SQSEndpoint != "" {
			o.BaseEndpoint = aws.String(c.SQSEndpoint)
		}
	}), nil
}

var Module = fx.Module("sqs", fx.Provide(New, fx.Annotate(NewOutboxSender, fx.As(new(outbox.Sender)))))
