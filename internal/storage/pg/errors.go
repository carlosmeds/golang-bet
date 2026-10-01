package pg

import (
	"context"
	"errors"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsTransient reports database failures for which a later attempt may succeed.
// It deliberately excludes constraint and other permanent SQL errors.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrConcurrentUpdate) || errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, puddle.ErrNotAvailable) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var connect *pgconn.ConnectError
	if errors.As(err, &connect) {
		return true
	}
	var network *net.OpError
	if errors.As(err, &network) {
		return true
	}
	var sql *pgconn.PgError
	if errors.As(err, &sql) {
		return strings.HasPrefix(sql.Code, "08") || strings.HasPrefix(sql.Code, "57P0") ||
			sql.Code == "40001" || sql.Code == "40P01"
	}
	return false
}
