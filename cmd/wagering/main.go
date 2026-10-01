package main

import (
	"go.uber.org/fx"
	"log/slog"
	"os"
	"wagering/internal/auth"
	"wagering/internal/bootstrap"
	"wagering/internal/httpapi"
	"wagering/internal/messaging/consumer"
	"wagering/internal/messaging/sqsclient"
	"wagering/internal/observability"
	"wagering/internal/storage/pg"
	"wagering/internal/workers/outbox"
	"wagering/internal/workers/reference"
)

func main() {
	logger := fx.Provide(func() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) })
	bootstrap.New(logger, fx.Invoke(func(l *slog.Logger) { slog.SetDefault(l) }), observability.Module, pg.Module, auth.Module, reference.Module, sqsclient.Module, consumer.Module, outbox.Module, httpapi.Module).Run()
}
