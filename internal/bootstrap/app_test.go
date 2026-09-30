package bootstrap

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/fx"
)

func TestLifecycleOrder(t *testing.T) {
	setRequiredEnvironment(t)
	var order []string
	app := New(fx.Invoke(func(lc fx.Lifecycle) {
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { order = append(order, "start-one"); return nil },
			OnStop:  func(context.Context) error { order = append(order, "stop-one"); return nil },
		})
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { order = append(order, "start-two"); return nil },
			OnStop:  func(context.Context) error { order = append(order, "stop-two"); return nil },
		})
	}))
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"start-one", "start-two", "stop-two", "stop-one"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("lifecycle order = %v, want %v", order, want)
	}
}

func TestInvalidConfigurationFailsComposition(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	app := New()
	if app.Err() == nil {
		t.Fatal("expected configuration error")
	}
}

func setRequiredEnvironment(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"DATABASE_URL":    "postgres://local:local@localhost:5432/wagering",
		"WAGER_QUEUE_URL": "http://localhost:4566/000000000000/wager-transactions.fifo",
		"EVENT_QUEUE_URL": "http://localhost:4566/000000000000/wager-events.fifo",
		"OIDC_ISSUER_URL": "http://localhost:8081/realms/wagering",
		"OIDC_AUDIENCE":   "wagering-api",
	} {
		t.Setenv(key, value)
	}
}
