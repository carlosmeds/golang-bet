package main

import (
	"wagering/internal/auth"
	"wagering/internal/bootstrap"
	"wagering/internal/httpapi"
	"wagering/internal/messaging/consumer"
	"wagering/internal/messaging/sqsclient"
	"wagering/internal/storage/pg"
	"wagering/internal/workers/reference"
)

func main() {
	bootstrap.New(pg.Module, auth.Module, reference.Module, sqsclient.Module, consumer.Module, httpapi.Module).Run()
}
