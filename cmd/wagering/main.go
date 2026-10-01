package main

import (
	"go.uber.org/fx"
	"log/slog"
	"os"
	"wagering/internal/auth"
	"wagering/internal/bootstrap"
	"wagering/internal/config"
	"wagering/internal/httpapi"
	"wagering/internal/messaging/consumer"
	"wagering/internal/messaging/sqsclient"
	"wagering/internal/observability"
	"wagering/internal/storage/pg"
	"wagering/internal/workers/outbox"
	"wagering/internal/workers/reference"
)

func main() {
	c, err := config.Load()
	if err != nil {
		slog.Error("invalid process configuration", "error", err)
		os.Exit(1)
	}
	logger := fx.Provide(func() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) })
	bootstrap.NewWithConfig(c, logger, fx.Invoke(func(l *slog.Logger) { slog.SetDefault(l) }), observability.Module, pg.Module, auth.Module, reference.Module, sqsclient.Module, consumer.Module, outbox.Module, httpapi.Module).Run()
}
