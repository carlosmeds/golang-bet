// Package pg implements the durable PostgreSQL boundary for the wagering service.
// A Tx owns one connection and all financial rows must be written through it.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"wagering/internal/config"
	"wagering/internal/storage/schema"
)

var ErrNotFound = errors.New("record not found")
var ErrConcurrentUpdate = errors.New("concurrent update")
var ErrInvalidClaim = errors.New("invalid claim parameters")

// Conflict preserves the named unique constraint for a use case to classify.
type Conflict struct {
	Constraint string
	Cause      error
}

func (e *Conflict) Error() string {
	return fmt.Sprintf("unique conflict %s: %v", e.Constraint, e.Cause)
}
func (e *Conflict) Unwrap() error { return e.Cause }

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var p *pgconn.PgError
	if errors.As(err, &p) && p.Code == schema.SQLStateUniqueViolation {
		return &Conflict{Constraint: p.ConstraintName, Cause: err}
	}
	return err
}

// Store is safe for concurrent requests and service instances.
type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{Pool: p}, nil
}
func (s *Store) Close() { s.Pool.Close() }
func Provide(c config.Config, lc fx.Lifecycle) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, c.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err = ApplyMigrations(ctx, s.Pool); err != nil {
		s.Close()
		return nil, err
	}
	lc.Append(fx.Hook{OnStop: func(context.Context) error { s.Close(); return nil }})
	return s, nil
}

var Module = fx.Options(fx.Provide(Provide))

// Tx is deliberately not safe for concurrent use. Rollback on any callback
// error makes inbox, balance, transaction, ledger and outbox atomic.
type Tx struct{ pgx.Tx }

func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	raw, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer raw.Rollback(context.Background())
	if err = fn(&Tx{raw}); err != nil {
		return err
	}
	return classify(raw.Commit(ctx))
}

// WithSnapshot gives reconciliation and multi-query reads one consistent
// database view; PostgreSQL repeatable-read prevents a balance/ledger split.
func (s *Store) WithSnapshot(ctx context.Context, fn func(*Tx) error) error {
	raw, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer raw.Rollback(context.Background())
	if err = fn(&Tx{raw}); err != nil {
		return err
	}
	return classify(raw.Commit(ctx))
}
