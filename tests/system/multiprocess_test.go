// Package system runs the production binary as independent OS processes
// against Compose PostgreSQL, Keycloak and SQS. It is intentionally gated by
// explicit integration environment variables.
package system_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/tests/integration/auth/idptest"
)

const (
	adminEnv    = "WAGERING_TEST_ADMIN_URL"
	keycloakEnv = "WAGERING_TEST_KEYCLOAK_URL"
)

type process struct {
	cmd     *exec.Cmd
	base    string
	done    chan struct{}
	waitErr error
	logPath string
}

type system struct {
	t                              *testing.T
	root, bin, faultBin, dsn       string
	pool                           *pgxpool.Pool
	apps                           []*process
	client                         *http.Client
	internal, provider, providerID string
	keycloak                       string
	queue, events                  string
	sqs                            *sqs.Client
}

func TestThreeProcessConcurrencyAndRecovery(t *testing.T) {
	adminDSN := strings.TrimSpace(os.Getenv(adminEnv))
	if adminDSN == "" {
		t.Skip("set WAGERING_TEST_ADMIN_URL to run independent-process integration tests")
	}
	kc := idptest.KeycloakURL(t)
	s := newSystem(t, adminDSN, kc)
	s.apps = append(s.apps, s.start(s.bin, nil))
	s.apps = append(s.apps, s.start(s.bin, nil))
	s.apps = append(s.apps, s.start(s.bin, nil))
	t.Cleanup(s.stopAll)
	for _, p := range s.apps {
		s.ready(p)
	}

	t.Run("50 duplicate BETs through three independent processes", func(t *testing.T) {
		s.ensureApps(t, 3)
		s.refreshTokens(t)
		wallet, player := s.openWallet(t, s.apps[0], "100.00")
		op := s.operation("race-50", wallet, player, "BET", "1.00")
		body := mustJSON(t, op)
		statuses := concurrentRequests(50, func(i int) int { return s.submit(s.apps[i%len(s.apps)], s.provider, "race-50-key", body) })
		created, replayed := 0, 0
		for _, code := range statuses {
			if code == http.StatusCreated {
				created++
			} else if code == http.StatusOK {
				replayed++
			} else {
				t.Errorf("unexpected duplicate status: %d", code)
			}
		}
		if created != 1 || replayed != 49 {
			t.Fatalf("created=%d replayed=%d; want 1 and 49", created, replayed)
		}
		s.assertWallet(t, wallet, 9900, 2)
		if n := s.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id='race-50'`); n != 1 {
			t.Fatalf("transactions=%d", n)
		}
	})

	t.Run("two 80 BETs compete for 100 and independent wallets proceed", func(t *testing.T) {
		s.ensureApps(t, 3)
		s.refreshTokens(t)
		wallet, player := s.openWallet(t, s.apps[0], "100.00")
		ops := [][]byte{mustJSON(t, s.operation("race-80-a", wallet, player, "BET", "80.00")), mustJSON(t, s.operation("race-80-b", wallet, player, "BET", "80.00"))}
		var codes [2]int
		var wg sync.WaitGroup
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = s.submit(s.apps[i], s.provider, "race-80-key-"+fmt.Sprint(i), ops[i])
			}(i)
		}
		wg.Wait()
		created, rejected := 0, 0
		for _, code := range codes {
			if code == http.StatusCreated {
				created++
			} else if code == http.StatusUnprocessableEntity {
				rejected++
			} else {
				t.Errorf("unexpected 80 BET status: %d", code)
			}
		}
		if created != 1 || rejected != 1 {
			t.Fatalf("80 BET results=%v; want one commit and one business rejection", codes)
		}
		s.assertWallet(t, wallet, 2000, 2)

		w1, p1 := s.openWallet(t, s.apps[0], "25.00")
		w2, p2 := s.openWallet(t, s.apps[1], "25.00")
		independent := [][]byte{mustJSON(t, s.operation("independent-a", w1, p1, "BET", "25.00")), mustJSON(t, s.operation("independent-b", w2, p2, "BET", "25.00"))}
		codes = [2]int{}
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i] = s.submit(s.apps[i], s.provider, "independent-key-"+fmt.Sprint(i), independent[i])
			}(i)
		}
		wg.Wait()
		if codes != [2]int{http.StatusCreated, http.StatusCreated} {
			t.Fatalf("independent wallet statuses=%v", codes)
		}
		s.assertWallet(t, w1, 0, 2)
		s.assertWallet(t, w2, 0, 2)
	})

	t.Run("HTTP SQS crossing, commit-before-delete crash, restart replay", func(t *testing.T) {
		s.ensureApps(t, 3)
		s.refreshTokens(t)
		wallet, player := s.openWallet(t, s.apps[0], "50.00")
		op := s.operation("cross-transport-crash", wallet, player, "BET", "7.00")
		key := "cross-transport-crash-key"
		if code := s.submit(s.apps[0], s.provider, key, mustJSON(t, op)); code != http.StatusCreated {
			t.Fatalf("HTTP status %d", code)
		}
		msgID := randomID(t)
		body := mustJSON(t, map[string]any{"messageId": msgID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": merge(op, map[string]any{"idempotencyKey": key})})
		s.stopAll()
		marker := filepath.Join(t.TempDir(), "committed")
		fault := s.start(s.faultBin, map[string]string{"WAGERING_FAULT_AFTER_COMMIT_MARKER": marker})
		s.apps = append(s.apps, fault)
		s.sendSQS(t, body, wallet)
		waitFile(t, marker, 30*time.Second)
		if raw, err := os.ReadFile(marker); err != nil || len(raw) == 0 {
			t.Fatalf("fault marker=%q err=%v", raw, err)
		}
		waitFor(t, 10*time.Second, func() bool {
			return s.count(t, `SELECT count(*) FROM inbox_messages WHERE message_id='`+msgID+`' AND completed_at IS NOT NULL`) == 1
		})
		fault.kill(t)
		s.apps = nil
		for i := 0; i < 3; i++ {
			s.apps = append(s.apps, s.start(s.bin, nil))
		}
		for _, p := range s.apps {
			s.ready(p)
		}
		waitFor(t, 20*time.Second, func() bool {
			return s.count(t, `SELECT count(*) FROM inbox_messages WHERE message_id='`+msgID+`' AND completed_at IS NOT NULL`) == 1
		})
		if !s.inboundQueueEmpty(t, 20*time.Second) {
			t.Fatal("SQS redelivery was not deleted after the already-completed inbox was observed")
		}
		// The same SQS inbox identity is delivered again after the forced crash.
		records := s.count(t, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id='cross-transport-crash'`)
		if records != 1 {
			t.Fatalf("cross transport transaction count=%d", records)
		}
		s.assertWallet(t, wallet, 4300, 2)
		// Restart all app processes and replay HTTP; the durable key must return
		// the original result without a second ledger entry.
		s.stopAll()
		s.apps = append(s.apps, s.start(s.bin, nil))
		s.ready(s.apps[0])
		if code := s.submit(s.apps[0], s.provider, key, mustJSON(t, op)); code != http.StatusOK {
			t.Fatalf("restart replay status=%d", code)
		}
		s.assertWallet(t, wallet, 4300, 2)
	})

	t.Run("pending reference resumes after restart and ledger reconciles", func(t *testing.T) {
		s.ensureApps(t, 3)
		s.refreshTokens(t)
		wallet, player := s.openWallet(t, s.apps[0], "50.00")
		refund := s.operation("refund-arrives-first", wallet, player, "REFUND", "10.00")
		refund["referenceExternalTransactionId"] = "reference-arrives-later"
		if code := s.submit(s.apps[0], s.provider, "refund-pending-key", mustJSON(t, refund)); code != http.StatusAccepted {
			t.Fatalf("pending REFUND status=%d", code)
		}
		if state := s.transactionStatus(t, "refund-arrives-first"); state != "PENDING_REFERENCE" {
			t.Fatalf("pre-restart state=%s", state)
		}
		s.stopAll()
		s.apps = nil
		s.apps = append(s.apps, s.start(s.bin, nil))
		s.ready(s.apps[0])
		bet := s.operation("reference-arrives-later", wallet, player, "BET", "10.00")
		bet["roundId"] = refund["roundId"]
		if code := s.submit(s.apps[0], s.provider, "reference-bet-key", mustJSON(t, bet)); code != http.StatusCreated {
			t.Fatalf("reference BET status=%d", code)
		}
		waitFor(t, 30*time.Second, func() bool { return s.transactionStatus(t, "refund-arrives-first") == "PROCESSED" })
		s.assertWallet(t, wallet, 5000, 3)
		s.reconcile(t, s.apps[0], wallet)
	})

	t.Run("claimed outbox recovers into a second publisher process", func(t *testing.T) {
		s.ensureApps(t, 3)
		s.refreshTokens(t)
		waitFor(t, 30*time.Second, func() bool { return s.count(t, "SELECT count(*) FROM outbox_events WHERE published_at IS NULL") == 0 })
		s.stopAll()
		s.apps = nil
		marker := filepath.Join(t.TempDir(), "claimed")
		fault := s.start(s.faultBin, map[string]string{"WAGERING_FAULT_AFTER_OUTBOX_CLAIM_MARKER": marker})
		s.apps = append(s.apps, fault)
		s.ready(fault)
		wallet, _ := s.openWallet(t, fault, "2.00")
		waitFile(t, marker, 20*time.Second)
		pendingIDs := s.eventIDs(t, wallet)
		if len(pendingIDs) == 0 {
			t.Fatal("faulted process claimed no event rows")
		}
		second := s.start(s.bin, nil)
		s.apps = append(s.apps, second)
		s.ready(second)
		time.Sleep(300 * time.Millisecond)
		for _, id := range pendingIDs {
			if s.count(t, `SELECT count(*) FROM outbox_events WHERE event_id='`+id+`' AND published_at IS NOT NULL`) > 0 {
				t.Fatalf("event %s published while first process held claim", id)
			}
		}
		fault.kill(t)
		waitFor(t, 20*time.Second, func() bool {
			return s.count(t, `SELECT count(*) FROM outbox_events WHERE aggregate_id='`+wallet+`' AND published_at IS NOT NULL`) == int64(len(pendingIDs))
		})
		found := s.receiveEvents(t, pendingIDs, 30*time.Second)
		for _, id := range pendingIDs {
			if !found[id] {
				t.Errorf("event %s was not observed on outbound SQS", id)
			}
		}
		// Confirm storage is the ledger-derived balance for each wallet touched.
		s.verifyLedgerSums(t)
	})
}

func newSystem(t *testing.T, adminDSN, kc string) *system {
	t.Helper()
	root := moduleRoot(t)
	admin, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	db := "wagering_t17_" + strings.ReplaceAll(randomID(t), "-", "")
	if _, err = admin.Exec(context.Background(), "CREATE DATABASE "+db); err != nil {
		admin.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		if e != nil {
			t.Errorf("drop T17 database: %v", e)
		}
		admin.Close(context.Background())
	})
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	pool, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	bin := filepath.Join(t.TempDir(), "wagering")
	build := exec.Command("go", "build", "-o", bin, "./cmd/wagering")
	build.Dir = root
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build application: %v\n%s", e, out)
	}
	faultBin := filepath.Join(t.TempDir(), "wagering-systemfault")
	build = exec.Command("go", "build", "-tags=systemfault", "-o", faultBin, "./cmd/wagering")
	build.Dir = root
	if out, e := build.CombinedOutput(); e != nil {
		t.Fatalf("build fault-injection application: %v\n%s", e, out)
	}
	awsRegion := envOr("WAGERING_TEST_AWS_REGION", "us-east-1")
	endpoint := envOr("WAGERING_TEST_SQS_ENDPOINT", "http://localhost:4566")
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(awsRegion), awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	if err != nil {
		t.Fatal(err)
	}
	client := sqs.NewFromConfig(awsCfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(endpoint) })
	q := createTestQueue(t, client, "t17-in-")
	eq := createTestQueue(t, client, "t17-out-")
	internal := idptest.Token(t, kc, idptest.Internal)
	tok := idptest.Token(t, kc, idptest.ProviderA)
	return &system{t: t, root: root, bin: bin, faultBin: faultBin, dsn: u.String(), pool: pool, client: &http.Client{Timeout: 10 * time.Second}, internal: internal, provider: tok, providerID: "provider-a", keycloak: kc, queue: q, events: eq, sqs: client}
}

func createTestQueue(t *testing.T, client *sqs.Client, prefix string) string {
	t.Helper()
	name := prefix + strings.ReplaceAll(randomID(t), "-", "") + ".fifo"
	created, err := client.CreateQueue(context.Background(), &sqs.CreateQueueInput{
		QueueName: aws.String(name),
		Attributes: map[string]string{
			string(types.QueueAttributeNameFifoQueue):                 "true",
			string(types.QueueAttributeNameContentBasedDeduplication): "false",
		},
	})
	if err != nil {
		t.Fatalf("create isolated T17 queue %s: %v", name, err)
	}
	queueURL := aws.ToString(created.QueueUrl)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(queueURL)}); err != nil {
			t.Errorf("delete isolated T17 queue %s: %v", name, err)
		}
	})
	return queueURL
}

func (s *system) refreshTokens(t *testing.T) {
	t.Helper()
	s.internal = idptest.Token(t, s.keycloak, idptest.Internal)
	s.provider = idptest.Token(t, s.keycloak, idptest.ProviderA)
}

func (s *system) ensureApps(t *testing.T, count int) {
	t.Helper()
	live := s.apps[:0]
	for _, p := range s.apps {
		select {
		case <-p.done:
		default:
			live = append(live, p)
		}
	}
	s.apps = live
	for len(s.apps) < count {
		p := s.start(s.bin, nil)
		s.apps = append(s.apps, p)
		s.ready(p)
	}
}

func (s *system) start(bin string, extra map[string]string) *process {
	s.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		s.t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	portEnv := map[string]string{
		"DATABASE_URL": s.dsn, "HTTP_ADDR": addr, "AWS_REGION": envOr("WAGERING_TEST_AWS_REGION", "us-east-1"), "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test",
		"SQS_ENDPOINT": envOr("WAGERING_TEST_SQS_ENDPOINT", "http://localhost:4566"), "WAGER_QUEUE_URL": s.queue, "EVENT_QUEUE_URL": s.events,
		"OIDC_ISSUER_URL": idptest.Issuer(strings.TrimRight(os.Getenv(keycloakEnv), "/")), "OIDC_JWKS_URL": "", "OIDC_AUDIENCE": idptest.Audience, "SHUTDOWN_TIMEOUT": "10s",
		"REFERENCE_RETRY_BASE_DELAY": "100ms", "REFERENCE_RETRY_MAX_DELAY": "100ms", "REFERENCE_RETRY_TTL": "2m", "REFERENCE_RETRY_MAX_ATTEMPTS": "0", "REFERENCE_WORKER_POLL_INTERVAL": "100ms",
		"OUTBOX_LEASE": "3s", "OUTBOX_SEND_TIMEOUT": "500ms", "OUTBOX_ACK_TIMEOUT": "500ms", "OUTBOX_POLL_INTERVAL": "100ms",
	}
	for k, v := range extra {
		portEnv[k] = v
	}
	env := filteredEnv(portEnv)
	logPath := filepath.Join(s.t.TempDir(), "process-"+randomID(s.t))
	log, err := os.Create(logPath)
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(bin)
	cmd.Dir = s.root
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		log.Close()
		s.t.Fatal(err)
	}
	_ = log.Close()
	p := &process{cmd: cmd, base: "http://" + addr, done: make(chan struct{}), logPath: logPath}
	go func() { p.waitErr = cmd.Wait(); close(p.done) }()
	s.t.Cleanup(func() { p.shutdown(s.t) })
	return p
}

func (s *system) ready(p *process) {
	s.t.Helper()
	until := time.Now().Add(30 * time.Second)
	for time.Now().Before(until) {
		select {
		case <-p.done:
			raw, _ := os.ReadFile(p.logPath)
			s.t.Fatalf("process exited before readiness: %v\n%s", p.waitErr, raw)
		default:
		}
		req, e := http.NewRequest(http.MethodGet, p.base+"/health/ready", nil)
		if e == nil {
			resp, e := s.client.Do(req)
			if e == nil {
				status := resp.StatusCode
				_ = resp.Body.Close()
				if status == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	raw, _ := os.ReadFile(p.logPath)
	s.t.Fatalf("process failed readiness\n%s", raw)
}
func (p *process) kill(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Errorf("process %s did not exit after kill", p.base)
	}
}
func (s *system) stopAll() {
	for _, p := range s.apps {
		if p.cmd.Process == nil {
			continue
		}
		select {
		case <-p.done:
			continue
		default:
		}
		p.shutdown(s.t)
	}
	s.apps = nil
}

func (p *process) shutdown(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
	case <-time.After(12 * time.Second):
		p.kill(t)
		t.Errorf("process %s exceeded graceful shutdown", p.base)
	}
}

func (s *system) openWallet(t *testing.T, p *process, amount string) (string, string) {
	t.Helper()
	player := "t17-player-" + randomID(t)
	body := mustJSON(t, map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": amount, "currency": "BRL"}})
	resp := s.request(p, http.MethodPost, "/wallets", s.internal, "", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("open wallet status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil || data.ID == "" {
		t.Fatalf("wallet response %s: %v", resp.Body, err)
	}
	return data.ID, player
}
func (s *system) operation(ext, wallet, player, kind, amount string) map[string]any {
	return map[string]any{"providerId": s.providerID, "externalTransactionId": ext, "playerId": player, "walletId": wallet, "roundId": "r-" + ext, "gameId": "t17", "kind": kind, "money": map[string]string{"amount": amount, "currency": "BRL"}}
}
func (s *system) submit(p *process, token, key string, body []byte) int {
	return s.request(p, http.MethodPost, "/wagering/transactions", token, key, body).StatusCode
}

type response struct {
	StatusCode int
	Body       []byte
}

func (s *system) request(p *process, method, path, token, key string, body []byte) response {
	s.t.Helper()
	req, e := http.NewRequest(method, p.base+path, strings.NewReader(string(body)))
	if e != nil {
		s.t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	r, e := s.client.Do(req)
	if e != nil {
		s.t.Fatalf("%s %s: %v", method, path, e)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return response{r.StatusCode, b}
}
func (s *system) sendSQS(t *testing.T, body []byte, wallet string) {
	t.Helper()
	_, e := s.sqs.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(s.queue), MessageBody: aws.String(string(body)), MessageGroupId: aws.String(wallet), MessageDeduplicationId: aws.String(randomID(t))})
	if e != nil {
		t.Fatalf("send test SQS wager: %v", e)
	}
}

func (s *system) inboundQueueEmpty(t *testing.T, timeout time.Duration) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for ctx.Err() == nil {
		out, err := s.sqs.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(s.queue), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
		if err == nil && out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)] == "0" && out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)] == "0" {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}
func (s *system) count(t *testing.T, query string) int64 {
	t.Helper()
	var n int64
	if e := s.pool.QueryRow(context.Background(), query).Scan(&n); e != nil {
		t.Fatalf("query count: %v", e)
	}
	return n
}
func (s *system) assertWallet(t *testing.T, id string, minor, version int64) {
	t.Helper()
	var got, v, sum int64
	e := s.pool.QueryRow(context.Background(), `SELECT w.balance_amount,w.version,COALESCE((SELECT SUM(CASE direction WHEN 'CREDIT' THEN amount ELSE -amount END) FROM ledger_entries WHERE wallet_id=w.id),0) FROM wallets w WHERE w.id=$1`, id).Scan(&got, &v, &sum)
	if e != nil {
		t.Fatal(e)
	}
	if got != minor || v != version || sum != got {
		t.Fatalf("wallet %s balance=%d version=%d ledgerSum=%d; expected balance=%d version=%d", id, got, v, sum, minor, version)
	}
}
func (s *system) transactionStatus(t *testing.T, external string) string {
	t.Helper()
	var state string
	e := s.pool.QueryRow(context.Background(), `SELECT status FROM wager_transactions WHERE provider_id=$1 AND external_transaction_id=$2`, s.providerID, external).Scan(&state)
	if e != nil {
		return ""
	}
	return state
}
func (s *system) reconcile(t *testing.T, p *process, wallet string) {
	t.Helper()
	r := s.request(p, http.MethodPost, "/wallets/"+wallet+"/reconciliation", s.internal, "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("reconciliation status=%d body=%s", r.StatusCode, r.Body)
	}
	var x struct {
		Consistent bool `json:"consistent"`
	}
	if e := json.Unmarshal(r.Body, &x); e != nil || !x.Consistent {
		t.Fatalf("reconciliation response=%s err=%v", r.Body, e)
	}
}
func (s *system) eventIDs(t *testing.T, wallet string) []string {
	t.Helper()
	rows, e := s.pool.Query(context.Background(), `SELECT event_id::text FROM outbox_events WHERE aggregate_type='wallet' AND aggregate_id=$1 AND published_at IS NULL ORDER BY seq`, wallet)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	if e = rows.Err(); e != nil {
		t.Fatal(e)
	}
	return ids
}
func (s *system) receiveEvents(t *testing.T, ids []string, timeout time.Duration) map[string]bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	want, found := make(map[string]bool, len(ids)), make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for ctx.Err() == nil && len(found) < len(want) {
		out, e := s.sqs.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(s.events), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 5})
		if e != nil {
			if ctx.Err() != nil {
				return found
			}
			t.Logf("receive outbound SQS: %v", e)
			continue
		}
		for _, m := range out.Messages {
			var h struct {
				EventID string `json:"eventId"`
			}
			_ = json.Unmarshal([]byte(aws.ToString(m.Body)), &h)
			_, _ = s.sqs.DeleteMessage(context.Background(), &sqs.DeleteMessageInput{QueueUrl: aws.String(s.events), ReceiptHandle: m.ReceiptHandle})
			if want[h.EventID] {
				found[h.EventID] = true
			}
		}
	}
	return found
}
func (s *system) verifyLedgerSums(t *testing.T) {
	t.Helper()
	rows, e := s.pool.Query(context.Background(), `SELECT w.id::text,w.balance_amount,COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount ELSE -l.amount END),0) FROM wallets w LEFT JOIN ledger_entries l ON l.wallet_id=w.id GROUP BY w.id,w.balance_amount HAVING w.balance_amount<>COALESCE(SUM(CASE l.direction WHEN 'CREDIT' THEN l.amount ELSE -l.amount END),0)`)
	if e != nil {
		t.Fatal(e)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var balance, sum int64
		if e = rows.Scan(&id, &balance, &sum); e != nil {
			t.Fatal(e)
		}
		t.Errorf("ledger mismatch wallet=%s balance=%d sum=%d", id, balance, sum)
	}
}

func concurrentRequests(n int, fn func(int) int) []int {
	out := make([]int, n)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(i int) { defer wg.Done(); out[i] = fn(i) }(i)
	}
	wg.Wait()
	return out
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func merge(a map[string]any, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	waitFor(t, d, func() bool { _, e := os.Stat(path); return e == nil })
}
func waitFor(t *testing.T, d time.Duration, condition func() bool) {
	t.Helper()
	until := time.Now().Add(d)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}
func randomID(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		t.Fatal(e)
	}
	return hex.EncodeToString(b[:])
}
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root, e := filepath.Abs(filepath.Join(filepath.Dir(file), "../.."))
	if e != nil {
		t.Fatal(e)
	}
	return root
}
func filteredEnv(overrides map[string]string) []string {
	var env []string
	for _, item := range os.Environ() {
		k, _, _ := strings.Cut(item, "=")
		if _, ok := overrides[k]; !ok {
			env = append(env, item)
		}
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
func envOr(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}
