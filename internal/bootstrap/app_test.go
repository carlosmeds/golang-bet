package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"go.uber.org/fx"
	"wagering/internal/config"
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

func TestConfiguredShutdownTimeoutBoundsStop(t *testing.T) {
	setRequiredEnvironment(t)
	t.Setenv("SHUTDOWN_TIMEOUT", "80ms")
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	stopping := make(chan struct{})
	app := NewWithConfig(c, fx.NopLogger, fx.Invoke(func(lc fx.Lifecycle) {
		lc.Append(fx.Hook{OnStop: func(ctx context.Context) error {
			close(stopping)
			<-ctx.Done()
			return ctx.Err()
		}})
	}))
	if err := app.Err(); err != nil {
		t.Fatal(err)
	}
	if app.StopTimeout() != c.ShutdownTimeout {
		t.Fatalf("Fx stop timeout = %s, want %s", app.StopTimeout(), c.ShutdownTimeout)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), app.StopTimeout())
	defer cancel()
	err = app.Stop(ctx)
	select {
	case <-stopping:
	default:
		t.Fatal("stop hook did not run")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("shutdown took %s with configured timeout %s", elapsed, c.ShutdownTimeout)
	}
}

func TestWorkerTerminatesBeforeResourceClose(t *testing.T) {
	setRequiredEnvironment(t)
	workerDone := make(chan struct{})
	var cancel context.CancelFunc
	var order []string
	app := New(fx.NopLogger, fx.Invoke(func(lc fx.Lifecycle) {
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error { order = append(order, "resource-start"); return nil },
			OnStop: func(context.Context) error {
				select {
				case <-workerDone:
				default:
					return errors.New("resource closed before worker terminated")
				}
				order = append(order, "resource-stop")
				return nil
			},
		})
		lc.Append(fx.Hook{
			OnStart: func(context.Context) error {
				var ctx context.Context
				ctx, cancel = context.WithCancel(context.Background())
				go func() { <-ctx.Done(); close(workerDone) }()
				order = append(order, "worker-start")
				return nil
			},
			OnStop: func(ctx context.Context) error {
				cancel()
				select {
				case <-workerDone:
					order = append(order, "worker-stop")
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			},
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
	want := []string{"resource-start", "worker-start", "worker-stop", "resource-stop"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("lifecycle order = %v, want %v", order, want)
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
