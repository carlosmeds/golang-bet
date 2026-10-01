package pg

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"connection class", &pgconn.PgError{Code: "08006"}, true},
		{"server stopping", &pgconn.PgError{Code: "57P01"}, true},
		{"cannot connect now", &pgconn.PgError{Code: "57P03"}, true},
		{"serialization", &pgconn.PgError{Code: "40001"}, true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, true},
		{"closed pool", puddle.ErrClosedPool, true},
		{"pool unavailable", puddle.ErrNotAvailable, true},
		{"acquire deadline", context.DeadlineExceeded, true},
		{"concurrent update", ErrConcurrentUpdate, true},
		{"unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"check violation", &pgconn.PgError{Code: "23514"}, false},
		{"syntax error", &pgconn.PgError{Code: "42601"}, false},
		{"permanent operator cancellation", &pgconn.PgError{Code: "57014"}, false},
		{"other rollback", &pgconn.PgError{Code: "40003"}, false},
		{"other", errors.New("bug"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(fmt.Errorf("storage: %w", tt.err)); got != tt.want {
				t.Fatalf("IsTransient = %v, want %v", got, tt.want)
			}
		})
	}
}
