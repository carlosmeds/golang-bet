// Package sqs verifies the inbound consumer against a real SQS-compatible
// broker: provisioned redrive, poison-message dead-lettering and transient
// retry with visibility backoff. Tests skip unless WAGERING_TEST_SQS_ENDPOINT
// is set (the Compose stack exposes http://localhost:4566).
package sqs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/contract"
	"wagering/internal/messaging/consumer"
	wager "wagering/internal/usecase/wagering"
	"wagering/tests/integration/brokertest"
)

// Mirrors infra/localstack/init-sqs.sh.
const provisionedMaxReceiveCount = "3"

// client signs as the broker operator: these tests create isolated queues.
func client(t *testing.T) (*sqs.Client, string) {
	t.Helper()
	endpoint := brokertest.Endpoint(t)
	return brokertest.Client(t, endpoint, brokertest.Operator), endpoint
}

func randomName(t *testing.T, prefix string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(b[:]) + ".fifo"
}

func createQueue(t *testing.T, c *sqs.Client, name string, attrs map[string]string) (string, string) {
	t.Helper()
	ctx := context.Background()
	all := map[string]string{string(types.QueueAttributeNameFifoQueue): "true"}
	maps.Copy(all, attrs)
	out, err := c.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(name), Attributes: all})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	url := aws.ToString(out.QueueUrl)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = c.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: aws.String(url)})
	})
	got, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: aws.String(url), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatalf("queue arn %s: %v", name, err)
	}
	return url, got.Attributes[string(types.QueueAttributeNameQueueArn)]
}

// pair creates an isolated inbound FIFO queue whose redrive policy matches the
// provisioned one, plus its DLQ.
func pair(t *testing.T, c *sqs.Client) (queue, dlq string) {
	t.Helper()
	dlq, dlqArn := createQueue(t, c, randomName(t, "t19-dlq-"), nil)
	policy, err := json.Marshal(map[string]string{"deadLetterTargetArn": dlqArn, "maxReceiveCount": provisionedMaxReceiveCount})
	if err != nil {
		t.Fatal(err)
	}
	queue, _ = createQueue(t, c, randomName(t, "t19-in-"), map[string]string{string(types.QueueAttributeNameRedrivePolicy): string(policy)})
	return queue, dlq
}

func send(t *testing.T, c *sqs.Client, queue, body string) {
	t.Helper()
	_, err := c.SendMessage(context.Background(), &sqs.SendMessageInput{QueueUrl: aws.String(queue), MessageBody: aws.String(body),
		MessageGroupId: aws.String("wallet-1"), MessageDeduplicationId: aws.String(randomName(t, "d"))})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
}

func count(t *testing.T, c *sqs.Client, queue string) int {
	t.Helper()
	out, err := c.GetQueueAttributes(context.Background(), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(queue),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
	if err != nil {
		t.Fatalf("queue attributes: %v", err)
	}
	n := 0
	for _, k := range []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible} {
		var v int
		_ = json.Unmarshal([]byte(out.Attributes[string(k)]), &v)
		n += v
	}
	return n
}

type scripted struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (s *scripted) Execute(context.Context, contract.Operation, string, string, *wager.Inbox) (wager.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return wager.Result{}, err
	}
	return wager.Result{}, nil
}

func (s *scripted) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func worker(t *testing.T, c *sqs.Client, queue string, p consumer.Processor) *consumer.Worker {
	t.Helper()
	cfg := consumer.DefaultConfig(queue)
	cfg.WaitSeconds = 1
	cfg.VisibilitySeconds = 5
	w, err := consumer.New(c, p, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// until polls the worker until cond holds or the deadline passes.
func until(t *testing.T, w *consumer.Worker, deadline time.Duration, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	for !cond() {
		if ctx.Err() != nil {
			t.Fatalf("condition not reached within %s", deadline)
		}
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			t.Fatalf("poll: %v", err)
		}
	}
}

const validBody = `{"messageId":"t19-msg-1","type":"WagerTransactionRequested","occurredAt":"2026-09-30T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"t19-ext-1","idempotencyKey":"provider-a:t19-ext-1","playerId":"player-1","walletId":"a1d3f476-b151-4644-aece-0871b4e22b7e","roundId":"r-1","gameId":"g-1","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}}`

func TestProvisionedInboundQueueRedrivesToDLQ(t *testing.T) {
	c, endpoint := client(t)
	ctx := context.Background()
	for _, name := range []string{"wager-transactions.fifo", "wager-transactions-dlq.fifo", "wager-events.fifo"} {
		if _, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)}); err != nil {
			t.Fatalf("provisioned queue %s missing at %s: %v", name, endpoint, err)
		}
	}
	url, err := c.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("wager-transactions.fifo")})
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: url.QueueUrl, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameRedrivePolicy}})
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		DeadLetterTargetArn string `json:"deadLetterTargetArn"`
		MaxReceiveCount     any    `json:"maxReceiveCount"`
	}
	if err := json.Unmarshal([]byte(out.Attributes[string(types.QueueAttributeNameRedrivePolicy)]), &policy); err != nil {
		t.Fatalf("redrive policy: %v", err)
	}
	if !strings.HasSuffix(policy.DeadLetterTargetArn, ":wager-transactions-dlq.fifo") {
		t.Fatalf("redrive target = %q", policy.DeadLetterTargetArn)
	}
}

// A malformed body is never deleted by the consumer; the broker's redrive
// moves it to the DLQ once maxReceiveCount is reached.
func TestPoisonMessageReachesDLQThroughRedrive(t *testing.T) {
	c, _ := client(t)
	queue, dlq := pair(t, c)
	send(t, c, queue, `{"messageId":"t19-poison","type":"WagerTransactionRequested"`)
	p := &scripted{}
	w := worker(t, c, queue, p)
	until(t, w, 60*time.Second, func() bool { return count(t, c, dlq) == 1 })
	if p.Calls() != 0 {
		t.Fatalf("poison message reached the use case %d times", p.Calls())
	}
	if n := count(t, c, queue); n != 0 {
		t.Fatalf("inbound queue still holds %d messages", n)
	}
	got, err := c.ReceiveMessage(context.Background(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(dlq), WaitTimeSeconds: 1})
	if err != nil || len(got.Messages) != 1 || !strings.Contains(aws.ToString(got.Messages[0].Body), "t19-poison") {
		t.Fatalf("DLQ receive = %+v, %v", got, err)
	}
}

// A transient use-case failure leaves the message on the queue with a
// visibility backoff; the next delivery commits and deletes it.
func TestTransientFailureRetriesThenDeletes(t *testing.T) {
	c, _ := client(t)
	queue, dlq := pair(t, c)
	send(t, c, queue, validBody)
	p := &scripted{errs: []error{errors.New("database temporarily unavailable")}}
	w := worker(t, c, queue, p)
	until(t, w, 60*time.Second, func() bool { return p.Calls() >= 2 })
	until(t, w, 10*time.Second, func() bool { return count(t, c, queue) == 0 })
	if n := count(t, c, dlq); n != 0 {
		t.Fatalf("transiently failed message reached the DLQ (%d)", n)
	}
	if p.Calls() != 2 {
		t.Fatalf("use case calls = %d, want 2", p.Calls())
	}
}
