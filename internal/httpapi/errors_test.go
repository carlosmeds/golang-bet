package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/internal/contract"
	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
)

func TestFailPreservesErrorClasses(t *testing.T) {
	tests := []struct {
		name, code, message string
		err                 error
		status              int
		retry               bool
	}{
		{"connection", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", &pgconn.PgError{Code: "08006", Message: "secret SQL detail"}, 503, true},
		{"serialization", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", &pgconn.PgError{Code: "40001", Message: "secret SQL detail"}, 503, true},
		{"deadlock", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", &pgconn.PgError{Code: "40P01", Message: "secret SQL detail"}, 503, true},
		{"server shutdown", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", &pgconn.PgError{Code: "57P01", Message: "secret SQL detail"}, 503, true},
		{"concurrent update", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", pg.ErrConcurrentUpdate, 503, true},
		{"permanent SQL", "INTERNAL_ERROR", "request failed", &pgconn.PgError{Code: "42601", Message: "secret SQL detail"}, 500, false},
		{"business rejection", string(domain.FailureInsufficientFundsBet), "insufficient funds", domain.Rejection(domain.FailureInsufficientFundsBet, "insufficient funds"), 422, false},
		{"validation", domain.CodeInvalidAmount, "invalid amount", &domain.Error{Kind: domain.ErrKindInvalid, Code: domain.CodeInvalidAmount, Message: "invalid amount"}, 400, false},
		{"conflict", "CONFLICT", "resource identity conflicts with an existing record", wager.ErrExternalIDConflict, 409, false},
		{"storage conflict", "CONFLICT", "resource identity conflicts with an existing record", &pg.Conflict{Constraint: "unique_key", Cause: &pgconn.PgError{Code: "23505", Message: "secret SQL detail"}}, 409, false},
		{"typed transient", "TEMPORARILY_UNAVAILABLE", "temporarily unavailable", domain.Transient(errors.New("secret SQL detail")), 503, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/wallets/unused", nil)
			request.Header.Set("X-Correlation-ID", "test-correlation")
			fail(recorder, request, fmt.Errorf("wrapped: %w", tt.err))
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			var body contract.ErrorBody
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != tt.code || body.Message != tt.message || body.CorrelationID != "test-correlation" {
				t.Fatalf("body = %+v", body)
			}
			if tt.retry && recorder.Header().Get("Retry-After") != "5" {
				t.Fatalf("Retry-After = %q", recorder.Header().Get("Retry-After"))
			}
			if !tt.retry && recorder.Header().Get("Retry-After") != "" {
				t.Fatalf("unexpected Retry-After = %q", recorder.Header().Get("Retry-After"))
			}
			if strings.Contains(recorder.Body.String(), "secret SQL detail") {
				t.Fatal("database detail leaked")
			}
		})
	}
}

func TestClosedDatabasePoolReturnsTemporaryUnavailable(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/wagering")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()

	api := &API{Store: &pg.Store{Pool: pool}}
	request := httptest.NewRequest(http.MethodGet, "/wallets/"+domain.NewUUID().String(), nil)
	request.SetPathValue("walletId", domain.NewUUID().String())
	recorder := httptest.NewRecorder()
	api.getWallet(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("status = %d Retry-After = %q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	var body contract.ErrorBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "TEMPORARILY_UNAVAILABLE" || body.Message != "temporarily unavailable" {
		t.Fatalf("body = %+v", body)
	}
}
