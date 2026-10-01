package consumer

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/observability"
	wager "wagering/internal/usecase/wagering"
)

func fromConfig(c config.Config, client *sqs.Client, svc *wager.Service, metrics *observability.Metrics) (*Worker, error) {
	return New(client, svc, DefaultConfig(c.WagerQueueURL), nil, metrics)
}

var Module = fx.Module("sqs-consumer", fx.Provide(fromConfig), fx.Invoke(func(lc fx.Lifecycle, w *Worker) {
	lc.Append(fx.Hook{OnStart: func(context.Context) error { w.Start(); return nil }, OnStop: w.Stop})
}))
