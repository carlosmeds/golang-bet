package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
	wallets "wagering/internal/usecase/wallet"
	"wagering/internal/workers/reference"
)

// Module creates the service and registers its listener with Fx lifecycle.
var Module = fx.Module("httpapi", fx.Provide(wallets.New, newWageringService, NewAPI), fx.Invoke(Start))

func newWageringService(store *pg.Store, c reference.Config) *wager.Service {
	svc := wager.New(store)
	svc.Retry = c.Retry
	return svc
}

func Start(lc fx.Lifecycle, c config.Config, a *API) {
	server := &http.Server{Addr: c.HTTPAddr, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	var listener net.Listener
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			var err error
			listener, err = net.Listen("tcp", c.HTTPAddr)
			if err != nil {
				return err
			}
			go func() {
				e := server.Serve(listener)
				if e != nil && !errors.Is(e, http.ErrServerClosed) {
					slog.Error("HTTP server stopped", "error", e)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error { return server.Shutdown(ctx) },
	})
}
