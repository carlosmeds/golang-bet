package reference

import (
	"context"
	"log/slog"

	"go.uber.org/fx"
	"wagering/internal/storage/pg"
	wagering "wagering/internal/usecase/wagering"
)

type params struct {
	fx.In
	Config   Config
	Store    *pg.Store
	Observer Observer     `optional:"true"`
	Logger   *slog.Logger `optional:"true"`
}

// newWorker builds the worker on the store. Its use case is constructed here with the
// configured retry policy, so scheduling and expiry always agree with Config.
func newWorker(p params) (*Worker, error) {
	svc := wagering.New(p.Store)
	svc.Retry = p.Config.Retry
	return NewWorker(p.Config, p.Store, svc, p.Observer, p.Logger)
}

// Module provides Config (from REFERENCE_* environment variables), builds the
// Worker and ties it to the Fx lifecycle. The HTTP/SQS entry path must build
// its wagering.Service with the provided Config.Retry so first-attempt
// scheduling matches the worker's.
var Module = fx.Module("reference-worker",
	fx.Provide(LoadConfig, newWorker),
	fx.Invoke(func(lc fx.Lifecycle, w *Worker) {
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { w.Start(); return nil },
			OnStop:  w.Stop,
		})
	}),
)
