//go:build !systemfault

package consumer

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func afterDurableCommit(context.Context, API, string, types.Message) error { return nil }
