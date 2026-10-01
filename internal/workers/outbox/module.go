package outbox

import (
	"context"
	"log/slog"

	"go.uber.org/fx"
	"wagering/internal/storage/pg"
)

type params struct {
	fx.In
	Config   Config
	Store    *pg.Store
	Sender   Sender
	Observer Observer     `optional:"true"`
	Logger   *slog.Logger `optional:"true"`
}

func newPublisher(p params) (*Publisher, error) {
	return NewPublisher(p.Config, p.Store, p.Sender, p.Observer, p.Logger)
}

// Module provides Config (from OUTBOX_* environment variables), builds the
// Publisher and ties it to the Fx lifecycle: started after its dependencies,
// stopped (no new claims, in-flight event finished or cancelled at the stop
// deadline) before them. The graph must provide a Sender, the adapter for the
// outbound queue, and a *pg.Store.
var Module = fx.Module("outbox-publisher",
	fx.Provide(LoadConfig, newPublisher),
	fx.Invoke(func(lc fx.Lifecycle, p *Publisher) {
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { p.Start(); return nil },
			OnStop:  p.Stop,
		})
	}),
)
