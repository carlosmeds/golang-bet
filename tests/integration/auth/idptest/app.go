package idptest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"wagering/internal/auth"
	"wagering/internal/config"
	"wagering/internal/domain"
	"wagering/internal/httpapi"
	"wagering/internal/messaging/sqsclient"
	"wagering/internal/observability"
	"wagering/internal/storage/pg"
	"wagering/internal/workers/reference"
)

// App is the real service (Fx composition of pg, auth, reference worker, SQS,
// observability and HTTP modules, listening on a real TCP port) wired to the
// real Keycloak and a scratch PostgreSQL database that exists only for one test.
type App struct {
	t        testing.TB
	BaseURL  string
	Keycloak string
	DSN      string
	Pool     *pgxpool.Pool
}

// StartApp creates a scratch database, starts the service against it and the
// Keycloak named by WAGERING_TEST_KEYCLOAK_URL, and registers cleanup. The
// service discovers its signing keys through OIDC discovery of the issuer, as
// a deployment without OIDC_JWKS_URL does.
func StartApp(t *testing.T) *App {
	t.Helper()
	kc := KeycloakURL(t)
	adminDSN := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if adminDSN == "" {
		t.Skip("set WAGERING_TEST_ADMIN_URL for PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}
	db := "wagering_t16_" + strings.ReplaceAll(domain.NewUUID().String(), "-", "")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); e != nil {
			t.Errorf("drop database: %v", e)
		}
		admin.Close(ctx)
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close() // the service binds it itself; nothing else uses this port in the test

	t.Setenv("OIDC_JWKS_URL", "") // force discovery through the issuer
	t.Setenv("OIDC_PROVIDER_CLAIM", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	sqsEndpoint := strings.TrimRight(os.Getenv("WAGERING_TEST_SQS_ENDPOINT"), "/")
	if sqsEndpoint == "" {
		sqsEndpoint = "http://localhost:4566"
	}
	cfg := config.Config{
		HTTPAddr:        addr,
		DatabaseURL:     u.String(),
		AWSRegion:       "us-east-1",
		SQSEndpoint:     sqsEndpoint,
		WagerQueueURL:   sqsEndpoint + "/000000000000/wager-transactions.fifo",
		EventQueueURL:   sqsEndpoint + "/000000000000/wager-events.fifo",
		OIDCIssuerURL:   Issuer(kc),
		OIDCAudience:    Audience,
		ShutdownTimeout: 10 * time.Second,
	}
	app := fx.New(fx.NopLogger, fx.Supply(cfg), observability.Module, pg.Module, auth.Module, reference.Module, sqsclient.Module, httpapi.Module)
	if err := app.Err(); err != nil {
		t.Fatalf("compose service: %v", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start service: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := app.Stop(stopCtx); err != nil {
			t.Errorf("stop service: %v", err)
		}
	})
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return &App{t: t, BaseURL: "http://" + addr, Keycloak: kc, DSN: u.String(), Pool: pool}
}

// Response is a recorded HTTP response.
type Response struct {
	Status int
	Header http.Header
	Raw    []byte
}

// JSON decodes the body into a generic object.
func (r Response) JSON(t testing.TB) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(r.Raw, &out); err != nil {
		t.Fatalf("response is not a JSON object (status %d): %q", r.Status, r.Raw)
	}
	return out
}

// Code is the error envelope's code, or "" when the body has none.
func (r Response) Code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Raw, &e)
	return e.Code
}

// Request describes a call. Token is sent as a bearer credential when set;
// RawAuthorization, when set, is sent verbatim instead.
type Request struct {
	Method, Path     string
	Token            string
	RawAuthorization string
	Headers          map[string]string
	Body             any
}

// Do sends r and records the answer.
func (a *App) Do(r Request) Response {
	a.t.Helper()
	var body io.Reader
	switch b := r.Body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	case []byte:
		body = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			a.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(r.Method, a.BaseURL+r.Path, body)
	if err != nil {
		a.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case r.RawAuthorization != "":
		req.Header.Set("Authorization", r.RawAuthorization)
	case r.Token != "":
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", r.Method, r.Path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return Response{Status: resp.StatusCode, Header: resp.Header, Raw: raw}
}

// Counts is a snapshot of every table a financial request could write.
type Counts struct{ Wallets, Transactions, Ledger, Outbox, Inbox int }

func (c Counts) String() string {
	return fmt.Sprintf("wallets=%d transactions=%d ledger=%d outbox=%d inbox=%d", c.Wallets, c.Transactions, c.Ledger, c.Outbox, c.Inbox)
}

// Counts reads the row counts directly from PostgreSQL.
func (a *App) Counts() Counts {
	a.t.Helper()
	var c Counts
	err := a.Pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM wallets), (SELECT count(*) FROM wager_transactions),
		(SELECT count(*) FROM ledger_entries), (SELECT count(*) FROM outbox_events),
		(SELECT count(*) FROM inbox_messages)`).Scan(&c.Wallets, &c.Transactions, &c.Ledger, &c.Outbox, &c.Inbox)
	if err != nil {
		a.t.Fatalf("count rows: %v", err)
	}
	return c
}

// Balance reads a wallet's stored balance (minor units) and version.
func (a *App) Balance(walletID string) (minor, version int64) {
	a.t.Helper()
	if err := a.Pool.QueryRow(context.Background(), `SELECT balance_amount, version FROM wallets WHERE id=$1`, walletID).Scan(&minor, &version); err != nil {
		a.t.Fatalf("read wallet %s: %v", walletID, err)
	}
	return
}

// OpenWallet creates a wallet through the internal API and returns its ID.
func (a *App) OpenWallet(internalToken, player, amount string) string {
	a.t.Helper()
	r := a.Do(Request{Method: http.MethodPost, Path: "/wallets", Token: internalToken,
		Body: map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"}}})
	if r.Status != http.StatusCreated {
		a.t.Fatalf("open wallet: status %d body %s", r.Status, r.Raw)
	}
	return r.JSON(a.t)["id"].(string)
}

// Operation builds a provider operation body.
func Operation(provider, external, wallet, player, kind, amount string) map[string]any {
	op := map[string]any{
		"providerId": provider, "externalTransactionId": external, "playerId": player,
		"walletId": wallet, "roundId": "round-1", "gameId": "game-1", "kind": kind,
		"money": map[string]string{"amount": amount, "currency": "BRL"},
	}
	return op
}

// Submit posts an operation with the given idempotency key.
func (a *App) Submit(token, key string, op map[string]any) Response {
	a.t.Helper()
	headers := map[string]string{}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	return a.Do(Request{Method: http.MethodPost, Path: "/wagering/transactions", Token: token, Headers: headers, Body: op})
}
