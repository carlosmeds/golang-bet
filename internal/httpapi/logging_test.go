package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/internal/auth"
	"wagering/internal/domain"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
)

func captureRequestLog(t *testing.T, run func() int) (int, map[string]any, string) {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	status := run()
	var fields map[string]any
	if err := json.Unmarshal(output.Bytes(), &fields); err != nil {
		t.Fatalf("parse request log: %v: %s", err, output.String())
	}
	return status, fields, output.String()
}

func TestWagerRequestLogsValidatedIdentifiersWithoutSensitiveFields(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://localhost/wagering")
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	api := &API{Wagers: wager.New(&pg.Store{Pool: pool}), Auth: auth.NewMiddleware(nil)}
	walletID := domain.NewUUID().String()
	const externalID = "external-safe-identifier"
	const token = "bearer-secret-never-log"
	const idempotencyKey = "idempotency-secret-never-log"
	body := `{"providerId":"provider-a","externalTransactionId":"` + externalID + `","playerId":"player-secret-never-log","walletId":"` + walletID + `","roundId":"round-secret-never-log","gameId":"game-secret-never-log","kind":"BET","money":{"amount":"41.23","currency":"BRL"}}`
	status, fields, raw := captureRequestLog(t, func() int {
		request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(body))
		request.Pattern = "POST /wagering/transactions"
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Idempotency-Key", idempotencyKey)
		request.Header.Set("X-Correlation-ID", "correlation-1")
		request = request.WithContext(auth.WithIdentity(request.Context(), auth.NewIdentity("subject", "provider-a", auth.ScopeProvider)))
		recorder := httptest.NewRecorder()
		api.logRequest(http.HandlerFunc(api.submit)).ServeHTTP(recorder, request)
		return recorder.Code
	})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	for key, want := range map[string]any{
		"providerId": "provider-a", "walletId": walletID,
		"externalTransactionId": externalID, "correlationId": "correlation-1",
		"route": "POST /wagering/transactions", "status": float64(http.StatusServiceUnavailable),
	} {
		if fields[key] != want {
			t.Errorf("log %s = %v, want %v", key, fields[key], want)
		}
	}
	if _, ok := fields["transactionId"]; ok {
		t.Error("failed request logged an unavailable transactionId")
	}
	for _, forbidden := range []string{token, idempotencyKey, "player-secret-never-log", "round-secret-never-log", "game-secret-never-log", "41.23", "BRL", "authorization", "money"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("request log contains sensitive value %q", forbidden)
		}
	}
}

func TestWagerRequestLogsCommittedTransactionID(t *testing.T) {
	transactionID := domain.NewUUID().String()
	walletID := domain.NewUUID().String()
	api := &API{}
	status, fields, raw := captureRequestLog(t, func() int {
		request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil)
		request.Header.Set("Authorization", "Bearer secret-token")
		request = request.WithContext(auth.WithIdentity(request.Context(), auth.NewIdentity("subject", "provider-a", auth.ScopeProvider)))
		recorder := httptest.NewRecorder()
		api.logRequest(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ids := r.Context().Value(requestLogKey{}).(*requestLogIDs)
			ids.transactionID = transactionID
			ids.walletID = walletID
			ids.externalTransactionID = "external-1"
			w.WriteHeader(http.StatusCreated)
		})).ServeHTTP(recorder, request)
		return recorder.Code
	})
	if status != http.StatusCreated || fields["transactionId"] != transactionID || fields["walletId"] != walletID ||
		fields["externalTransactionId"] != "external-1" || fields["providerId"] != "provider-a" {
		t.Fatalf("incomplete success log: %s", raw)
	}
	if strings.Contains(raw, "secret-token") {
		t.Fatal("authorization credential appeared in request log")
	}
}
