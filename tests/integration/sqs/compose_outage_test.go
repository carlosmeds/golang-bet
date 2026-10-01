package sqs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"wagering/tests/integration/auth/idptest"
	"wagering/tests/integration/brokertest"
)

// F-2 acceptance against the running Compose stack: PostgreSQL is stopped (connections
// are refused) for longer than the 7 s in which the old settings dead-lettered a valid wager;
// the wager published meanwhile by the internal producer is processed exactly
// once after recovery, and a poison message still reaches the DLQ.
//
// Environment (skips when WAGERING_TEST_COMPOSE_POSTGRES_CONTAINER is unset):
//
//	WAGERING_TEST_COMPOSE_POSTGRES_CONTAINER  e.g. lead-postgres-1
//	WAGERING_TEST_COMPOSE_APP_URL             e.g. http://localhost:8081
//	WAGERING_TEST_KEYCLOAK_URL, WAGERING_TEST_ADMIN_URL, WAGERING_TEST_SQS_ENDPOINT,
//	WAGERING_TEST_BROKER_CREDENTIALS_FILE     as for the other live suites
func TestComposeValidWagerSurvivesPostgresOutageAndPoisonStillDeadLetters(t *testing.T) {
	container := os.Getenv("WAGERING_TEST_COMPOSE_POSTGRES_CONTAINER")
	appURL := strings.TrimRight(os.Getenv("WAGERING_TEST_COMPOSE_APP_URL"), "/")
	if container == "" || appURL == "" {
		t.Skip("set WAGERING_TEST_COMPOSE_POSTGRES_CONTAINER and WAGERING_TEST_COMPOSE_APP_URL to run against Compose")
	}
	endpoint := brokertest.Endpoint(t)
	adminDSN := os.Getenv("WAGERING_TEST_ADMIN_URL")
	if adminDSN == "" {
		t.Skip("WAGERING_TEST_ADMIN_URL is not set")
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/wagering"
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	producer := brokertest.Client(t, endpoint, brokertest.Producer)
	operator := brokertest.Client(t, endpoint, brokertest.Operator)
	inbound := brokertest.QueueURL(endpoint, brokertest.InboundQ)
	dlq := brokertest.QueueURL(endpoint, brokertest.DLQ)

	// A wallet to bet on, through the Compose application.
	internal := idptest.Token(t, idptest.KeycloakURL(t), idptest.Internal)
	player := "f2-player-" + randomName(t, "")
	player = strings.TrimSuffix(player, ".fifo")
	open, _ := json.Marshal(map[string]any{"playerId": player, "initialBalance": map[string]string{"amount": "50.00", "currency": "BRL"}})
	req, _ := http.NewRequest(http.MethodPost, appURL+"/wallets", bytes.NewReader(open))
	req.Header.Set("Authorization", "Bearer "+internal)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var wallet struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&wallet)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || wallet.ID == "" {
		t.Fatalf("open wallet: status %d id %q", resp.StatusCode, wallet.ID)
	}

	dlqBefore := count(t, operator, dlq)

	docker := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			t.Fatalf("docker %v: %v\n%s", args, err, out)
		}
	}
	docker("stop", container)
	stopped := true
	t.Cleanup(func() {
		if stopped {
			_ = exec.Command("docker", "start", container).Run()
		}
	})

	ext := "f2-" + strings.TrimSuffix(randomName(t, ""), ".fifo")
	msgID := "msg-" + ext
	body := fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested","occurredAt":%q,"data":{"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":%q,"playerId":%q,"walletId":%q,"roundId":"r-1","gameId":"g-1","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}}`,
		msgID, time.Now().UTC().Format(time.RFC3339), ext, "provider-a:"+ext, player, wallet.ID)
	if err := sendTo(producer, inbound, body); err != nil {
		t.Fatalf("producer publish: %v", err)
	}

	time.Sleep(outageLength + 5*time.Second) // well past the old 7 s window
	if n := count(t, operator, dlq); n != dlqBefore {
		t.Fatalf("DLQ grew from %d to %d during the PostgreSQL outage", dlqBefore, n)
	}
	if n := count(t, operator, inbound); n != 1 {
		t.Fatalf("inbound queue holds %d messages during the outage, want the valid wager (1)", n)
	}

	docker("start", container)
	stopped = false
	waitUntil(t, 90*time.Second, "the wager to be processed after recovery", func() bool {
		var tx, inbox int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM wager_transactions WHERE external_transaction_id=$1`, ext).Scan(&tx); err != nil {
			return false
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM inbox_messages WHERE message_id=$1 AND completed_at IS NOT NULL`, msgID).Scan(&inbox); err != nil {
			return false
		}
		return tx == 1 && inbox == 1 && count(t, operator, inbound) == 0
	})
	if n := count(t, operator, dlq); n != dlqBefore {
		t.Fatalf("DLQ grew from %d to %d: the valid wager was dead-lettered", dlqBefore, n)
	}

	// Poison still reaches the DLQ through redrive (15 receives, ~1 s apart).
	poison := `{"messageId":"` + msgID + `-poison","type":"WagerTransactionRequested"`
	if err := sendTo(producer, inbound, poison); err != nil {
		t.Fatalf("producer publish poison: %v", err)
	}
	waitUntil(t, 90*time.Second, "the poison message in the DLQ", func() bool { return count(t, operator, dlq) == dlqBefore+1 })
	waitUntil(t, 30*time.Second, "the inbound queue to drain", func() bool { return count(t, operator, inbound) == 0 })
	// Leave the DLQ as found: drop the poison message this test added.
	for i := 0; i < 5; i++ {
		out, err := operator.ReceiveMessage(ctx, receiveInput(dlq))
		if err != nil {
			break
		}
		for _, m := range out.Messages {
			if strings.Contains(deref(m.Body), msgID+"-poison") {
				_, _ = operator.DeleteMessage(ctx, deleteInput(dlq, m.ReceiptHandle))
			}
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func receiveInput(queue string) *sqs.ReceiveMessageInput {
	return &sqs.ReceiveMessageInput{QueueUrl: aws.String(queue), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 5}
}

func deleteInput(queue string, receipt *string) *sqs.DeleteMessageInput {
	return &sqs.DeleteMessageInput{QueueUrl: aws.String(queue), ReceiptHandle: receipt}
}
