package consumer

import (
	"context"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/observability"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
)

func fromConfig(c config.Config, client *sqs.Client, svc *wager.Service, store *pg.Store, metrics *observability.Metrics) (*Worker, error) {
	w, err := New(client, svc, DefaultConfig(c.WagerQueueURL), nil, metrics)
	if err != nil {
		return nil, err
	}
	// Receives pause while PostgreSQL is unreachable (F-2).
	w.Ready = store.Pool.Ping
	return w, nil
}

var Module = fx.Module("sqs-consumer", fx.Provide(fromConfig), fx.Invoke(func(lc fx.Lifecycle, w *Worker) {
	lc.Append(fx.Hook{OnStart: func(context.Context) error { w.Start(); return nil }, OnStop: w.Stop})
}))
