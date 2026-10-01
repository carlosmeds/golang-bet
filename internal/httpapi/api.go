package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/auth"
	"wagering/internal/config"
	"wagering/internal/contract"
	"wagering/internal/domain"
	"wagering/internal/observability"
	"wagering/internal/storage/pg"
	wager "wagering/internal/usecase/wagering"
	wallets "wagering/internal/usecase/wallet"
)

type API struct {
	Wallets *wallets.Service
	Wagers  *wager.Service
	Store   *pg.Store
	Auth    *auth.Middleware
	Config  config.Config
	SQS     interface {
		GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	}
	Metrics *observability.Metrics
}

func NewAPI(w *wallets.Service, g *wager.Service, s *pg.Store, a *auth.Middleware, c config.Config, client *sqs.Client, metrics *observability.Metrics) *API {
	a.CorrelationID = func(r *http.Request) string { return correlation(r) }
	return &API{Wallets: w, Wagers: g, Store: s, Auth: a, Config: c, SQS: client, Metrics: metrics}
}
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("live\n"))
	})
	mux.HandleFunc("GET /health/ready", a.ready)
	mux.Handle("GET /metrics", a.Metrics.Handler())
	mux.Handle("POST /wallets", a.Auth.Internal(a.logRequest(http.HandlerFunc(a.openWallet))))
	mux.Handle("GET /wallets/{walletId}", a.Auth.Internal(a.logRequest(http.HandlerFunc(a.getWallet))))
	mux.Handle("GET /wallets/{walletId}/ledger", a.Auth.Internal(a.logRequest(http.HandlerFunc(a.getLedger))))
	mux.Handle("POST /wallets/{walletId}/reconciliation", a.Auth.Internal(a.logRequest(http.HandlerFunc(a.reconcile))))
	mux.Handle("POST /wagering/transactions", a.Auth.Provider(a.logRequest(http.HandlerFunc(a.submit))))
	mux.Handle("GET /wagering/transactions/{transactionId}", a.Auth.Provider(a.logRequest(http.HandlerFunc(a.getTransaction))))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", a.Auth.Provider(a.logRequest(http.HandlerFunc(a.getExternalTransaction))))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rw := &statusWriter{ResponseWriter: w}
		mux.ServeHTTP(rw, r)
		if r.URL.Path != "/metrics" && r.URL.Path != "/health/live" && r.URL.Path != "/health/ready" {
			a.Metrics.RecordHTTP(rw.status, time.Since(started), false)
		}
	})
}

func (a *API) logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		logged := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(logged, r)
		status := logged.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []any{"correlationId", correlation(r), "method", r.Method, "route", r.Pattern, "status", status, "durationMs", time.Since(started).Milliseconds()}
		if id, ok := auth.FromContext(r.Context()); ok && id.ProviderID != "" {
			attrs = append(attrs, "providerId", id.ProviderID)
		}
		for _, key := range []string{"walletId", "transactionId", "externalTransactionId"} {
			if value := r.PathValue(key); value != "" {
				attrs = append(attrs, key, value)
			}
		}
		slog.InfoContext(r.Context(), "HTTP request completed", attrs...)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (a *API) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.Store.Pool.Ping(ctx); err != nil {
		slog.WarnContext(ctx, "readiness database check failed", "error", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, err := a.SQS.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(a.Config.WagerQueueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameAll}})
	if err != nil {
		slog.WarnContext(ctx, "readiness queue check failed", "error", err)
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}
func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
func correlation(r *http.Request) string {
	v := r.Header.Get("X-Correlation-ID")
	if v == "" || len(v) > 255 {
		return domain.NewUUID().String()
	}
	return v
}
func badRequest(w http.ResponseWriter, r *http.Request, err error) {
	respond(w, http.StatusBadRequest, contract.ErrorBody{Code: "INVALID_REQUEST", Message: err.Error(), CorrelationID: correlation(r)})
}
func fail(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	var de *domain.Error
	if errors.As(err, &de) {
		status, body := contract.HTTPError(err, correlation(r))
		respond(w, status, body)
		return
	}
	status := http.StatusInternalServerError
	body := contract.ErrorBody{Code: "INTERNAL_ERROR", Message: "request failed", CorrelationID: correlation(r)}
	switch {
	case errors.Is(err, pg.ErrNotFound):
		status = http.StatusNotFound
		body.Code = "NOT_FOUND"
		body.Message = "resource not found"
	case errors.Is(err, wallets.ErrAlreadyExists), errors.Is(err, wager.ErrExternalIDConflict):
		status = http.StatusConflict
		body.Code = "CONFLICT"
		body.Message = "resource identity conflicts with an existing record"
	case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
		status = http.StatusServiceUnavailable
		body.Code = "TEMPORARILY_UNAVAILABLE"
		body.Message = "temporarily unavailable"
	default:
		var conflict *pg.Conflict
		if errors.As(err, &conflict) {
			status = http.StatusConflict
			body.Code = "CONFLICT"
			body.Message = "resource identity conflicts with an existing record"
		}
	}
	respond(w, status, body)
}
func decode(body io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON: multiple values")
	}
	return nil
}
func parseID(s string) (domain.UUID, error) { return domain.ParseUUID(s) }
func (a *API) openWallet(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PlayerID       string       `json:"playerId"`
		InitialBalance domain.Money `json:"initialBalance"`
	}
	if err := decode(r.Body, &request); err != nil {
		badRequest(w, r, err)
		return
	}
	if request.PlayerID == "" {
		badRequest(w, r, errors.New("playerId is required"))
		return
	}
	result, err := a.Wallets.Open(r.Context(), request.PlayerID, request.InitialBalance, correlation(r))
	if err != nil {
		fail(w, r, err)
		return
	}
	wallet := result.Wallet
	respond(w, http.StatusCreated, map[string]any{"id": wallet.ID(), "playerId": wallet.PlayerID(), "balance": wallet.Balance(), "version": wallet.Version()})
}
func (a *API) getWallet(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		badRequest(w, r, err)
		return
	}
	var wallet *domain.Wallet
	err = a.Store.WithSnapshot(r.Context(), func(tx *pg.Tx) error { var e error; wallet, e = tx.GetWallet(r.Context(), id); return e })
	if err != nil {
		fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"id": wallet.ID(), "playerId": wallet.PlayerID(), "balance": wallet.Balance(), "version": wallet.Version()})
}
func (a *API) submit(w http.ResponseWriter, r *http.Request) {
	payload, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		badRequest(w, r, err)
		return
	}
	op, err := contract.ParseHTTP(payload)
	if err != nil {
		badRequest(w, r, err)
		return
	}
	if err = a.AuthAuthorize(r, op.ProviderID); err != nil {
		a.Auth.WriteError(w, r, err)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if err = contract.ValidateIdempotencyKey(key); err != nil {
		badRequest(w, r, err)
		return
	}
	result, err := a.Wagers.Execute(r.Context(), op, key, correlation(r), nil)
	if err != nil {
		fail(w, r, err)
		return
	}
	if result.Replay {
		a.Metrics.Duplicates.Add(1)
	}
	status := http.StatusCreated
	if result.Replay {
		status = http.StatusOK
	} else if result.Transaction.Status() == domain.StatusPendingReference {
		status = http.StatusAccepted
	} else if result.Transaction.Status() == domain.StatusRejected {
		status = http.StatusUnprocessableEntity
	}
	respond(w, status, transactionView(result.Transaction, result.WalletVersion, &result.Replay))
}
func (a *API) AuthAuthorize(r *http.Request, provider string) error {
	return auth.AuthorizeProvider(r.Context(), provider)
}
func transactionView(t *domain.WagerTransaction, version *int64, replay *bool) map[string]any {
	s := t.State()
	out := map[string]any{"transactionId": s.ID, "status": s.Status, "kind": s.Kind, "walletId": s.WalletID, "playerId": s.PlayerID, "amount": s.Money, "createdAt": s.CreatedAt, "updatedAt": s.UpdatedAt}
	if s.ProviderID != "" {
		out["providerId"] = s.ProviderID
		out["externalTransactionId"] = s.ExternalTransactionID
		out["roundId"] = s.RoundID
		out["gameId"] = s.GameID
	}
	if s.ReferenceExternalTransactionID != "" {
		out["referenceExternalTransactionId"] = s.ReferenceExternalTransactionID
	}
	if s.FailureCode != "" {
		out["failureCode"] = s.FailureCode
	}
	if s.ResultBalance.IsInitialized() {
		out["balance"] = s.ResultBalance
	}
	if version != nil {
		out["walletVersion"] = *version
	}
	if replay != nil {
		out["idempotentReplay"] = *replay
	}
	if !s.CompletedAt.IsZero() {
		out["completedAt"] = s.CompletedAt
	}
	return out
}
func (a *API) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("transactionId"))
	if err != nil {
		badRequest(w, r, err)
		return
	}
	provider, err := auth.ProviderScope(r.Context())
	if err != nil {
		a.Auth.WriteError(w, r, err)
		return
	}
	var stored *pg.StoredTransaction
	err = a.Store.WithSnapshot(r.Context(), func(tx *pg.Tx) error { var e error; stored, e = tx.GetTransaction(r.Context(), id); return e })
	if err != nil {
		fail(w, r, err)
		return
	}
	if stored.Transaction.ProviderID() != provider {
		fail(w, r, pg.ErrNotFound)
		return
	}
	respond(w, http.StatusOK, transactionView(stored.Transaction, stored.ResultWalletVersion, nil))
}
func (a *API) getExternalTransaction(w http.ResponseWriter, r *http.Request) {
	provider, err := auth.ProviderScope(r.Context())
	if err != nil {
		a.Auth.WriteError(w, r, err)
		return
	}
	if provider != r.PathValue("providerId") {
		fail(w, r, pg.ErrNotFound)
		return
	}
	var stored *pg.StoredTransaction
	err = a.Store.WithSnapshot(r.Context(), func(tx *pg.Tx) error {
		var e error
		stored, e = tx.FindByExternalID(r.Context(), provider, r.PathValue("externalTransactionId"))
		return e
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, transactionView(stored.Transaction, stored.ResultWalletVersion, nil))
}

// Cursor is base64url of wallet UUID and last wallet version. It is opaque to
// clients and tied to one wallet so it cannot shift pagination to another.
func encodeCursor(id domain.UUID, version int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%d", id.String(), version)))
}
func decodeCursor(raw string, id domain.UUID) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, errors.New("invalid cursor")
	}
	wallet, version, ok := strings.Cut(string(data), ":")
	if !ok || wallet != id.String() {
		return 0, errors.New("invalid cursor")
	}
	v, err := strconv.ParseInt(version, 10, 64)
	if err != nil || v < 0 {
		return 0, errors.New("invalid cursor")
	}
	return v, nil
}
func (a *API) getLedger(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		badRequest(w, r, err)
		return
	}
	after, err := decodeCursor(r.URL.Query().Get("cursor"), id)
	if err != nil {
		badRequest(w, r, err)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			badRequest(w, r, errors.New("invalid limit: must be 1..200"))
			return
		}
	}
	var items []pg.LedgerPageItem
	err = a.Store.WithSnapshot(r.Context(), func(tx *pg.Tx) error {
		if _, e := tx.GetWallet(r.Context(), id); e != nil {
			return e
		}
		var e error
		items, e = tx.ListLedgerPage(r.Context(), id, after, limit+1)
		return e
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	entries := make([]any, 0, len(items))
	for _, item := range items {
		s := item.Entry.State()
		entries = append(entries, map[string]any{"id": s.ID, "transactionId": s.TransactionID, "walletVersion": item.WalletVersion, "direction": s.Direction, "amount": s.Amount, "balanceBefore": s.BalanceBefore, "balanceAfter": s.BalanceAfter, "createdAt": s.CreatedAt})
	}
	var next any
	if hasMore && len(items) > 0 {
		next = encodeCursor(id, items[len(items)-1].WalletVersion)
	}
	respond(w, http.StatusOK, map[string]any{"walletId": id, "entries": entries, "nextCursor": next})
}
func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("walletId"))
	if err != nil {
		badRequest(w, r, err)
		return
	}
	var wallet *domain.Wallet
	var entries []*domain.LedgerEntry
	err = a.Store.WithSnapshot(r.Context(), func(tx *pg.Tx) error {
		var e error
		wallet, e = tx.GetWallet(r.Context(), id)
		if e != nil {
			return e
		}
		entries, e = tx.ListLedger(r.Context(), id)
		return e
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	calculated, err := domain.CalculateBalance(wallet.Currency(), entries)
	if err != nil {
		fail(w, r, err)
		return
	}
	difference, err := wallet.Balance().Sub(calculated)
	if err != nil {
		fail(w, r, err)
		return
	}
	respond(w, http.StatusOK, map[string]any{"walletId": id, "storedBalance": wallet.Balance(), "calculatedBalance": calculated, "difference": difference, "consistent": difference.IsZero(), "checkedEntries": len(entries)})
	a.Metrics.RecordReconciliation(difference.IsZero())
	if !difference.IsZero() {
		slog.ErrorContext(r.Context(), "wallet reconciliation divergence", "walletId", id.String(), "correlationId", correlation(r))
	}
}
