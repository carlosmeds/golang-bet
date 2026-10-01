package consumer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/contract"
	"wagering/internal/domain"
	wager "wagering/internal/usecase/wagering"
)

const validBody = `{"messageId":"message-1","type":"WagerTransactionRequested","occurredAt":"2026-09-30T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"ext-1","idempotencyKey":"key-1","playerId":"player-1","walletId":"a1d3f476-b151-4644-aece-0871b4e22b7e","roundId":"r-1","gameId":"g-1","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}}`

type fakeSQS struct {
	message  types.Message
	messages []types.Message
	deleted  int
	changed  []int32
}

func (f *fakeSQS) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	if f.messages != nil {
		return &sqs.ReceiveMessageOutput{Messages: f.messages}, nil
	}
	return &sqs.ReceiveMessageOutput{Messages: []types.Message{f.message}}, nil
}
func (f *fakeSQS) DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.deleted++
	return &sqs.DeleteMessageOutput{}, nil
}
func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, in *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.changed = append(f.changed, in.VisibilityTimeout)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

type fakeProcessor struct {
	err    error
	result wager.Result
	calls  int
	inbox  *wager.Inbox
}

func (f *fakeProcessor) Execute(_ context.Context, _ contract.Operation, _, _ string, inbox *wager.Inbox) (wager.Result, error) {
	f.calls++
	f.inbox = inbox
	return f.result, f.err
}

type metricObserver struct {
	status  domain.Status
	source  string
	elapsed time.Duration
}

func (*metricObserver) CountRetry()     {}
func (*metricObserver) CountDLQ()       {}
func (*metricObserver) CountDuplicate() {}
func (*metricObserver) CountConflict()  {}
func (m *metricObserver) RecordTransaction(source string, status domain.Status) {
	m.source, m.status = source, status
}
func (m *metricObserver) RecordProcessing(source string, elapsed time.Duration) {
	m.source, m.elapsed = source, elapsed
}

func TestSQSOutcomeAndLatencyObservedAfterDurableResult(t *testing.T) {
	money, err := domain.NewMoney(100, domain.Currency("BRL"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := domain.NewOpeningTransaction(domain.NewUUID(), domain.NewUUID(), "player", money, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	observer := &metricObserver{}
	broker := &fakeSQS{message: types.Message{Body: aws.String(validBody), ReceiptHandle: aws.String("receipt")}}
	worker, err := New(broker, &fakeProcessor{result: wager.Result{Transaction: tx}}, DefaultConfig("queue-url"), nil, observer)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if observer.source != "sqs" || observer.status != domain.StatusProcessed || broker.deleted != 1 {
		t.Fatalf("metrics: %+v; deleted: %d", observer, broker.deleted)
	}
}
func TestAckOnlyAfterDurableSuccess(t *testing.T) {
	for _, tc := range []struct {
		name, body     string
		err            error
		wantDelete     int
		wantVisibility int32
	}{
		{"committed", validBody, nil, 1, -1},
		{"transient", validBody, errors.New("database unavailable"), 0, 1},
		{"permanent", validBody, domain.ErrHashMismatch, 0, 1},
		{"malformed", "{", nil, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker := &fakeSQS{message: types.Message{Body: aws.String(tc.body), ReceiptHandle: aws.String("receipt"), MessageId: aws.String("sqs-id"), Attributes: map[string]string{"ApproximateReceiveCount": "1"}}}
			process := &fakeProcessor{err: tc.err}
			worker, err := New(broker, process, DefaultConfig("queue-url"), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if broker.deleted != tc.wantDelete {
				t.Fatalf("deleted %d, want %d", broker.deleted, tc.wantDelete)
			}
			if tc.wantVisibility >= 0 && (len(broker.changed) != 1 || broker.changed[0] != tc.wantVisibility) {
				t.Fatalf("visibility %+v", broker.changed)
			}
			if tc.name == "committed" && (process.inbox == nil || process.inbox.MessageID != "message-1" || len(process.inbox.PayloadHash) != 64) {
				t.Fatalf("inbox identity %+v", process.inbox)
			}
			if tc.name == "malformed" && process.calls != 0 {
				t.Fatal("invalid envelope reached processor")
			}
		})
	}
}

func TestCancelledBatchReleasesAllReceipts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	broker := &fakeSQS{messages: []types.Message{{ReceiptHandle: aws.String("one")}, {ReceiptHandle: aws.String("two")}}}
	worker, err := New(broker, &fakeProcessor{}, DefaultConfig("queue-url"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if len(broker.changed) != 2 || broker.changed[0] != 0 || broker.changed[1] != 0 {
		t.Fatalf("released: %+v", broker.changed)
	}
	if broker.deleted != 0 {
		t.Fatal("deleted uncommitted receipt")
	}
}

type receiveCounter struct {
	fakeSQS
	received int
}

func (r *receiveCounter) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, o ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	r.received++
	return r.fakeSQS.ReceiveMessage(ctx, in, o...)
}

// While the dependency is down nothing is received, so the queue's redrive
// budget is not spent on valid messages (F-2).
func TestReceivesPauseWhileDependencyUnavailable(t *testing.T) {
	broker := &receiveCounter{fakeSQS: fakeSQS{message: types.Message{Body: aws.String(validBody), ReceiptHandle: aws.String("r"), Attributes: map[string]string{"ApproximateReceiveCount": "1"}}}}
	process := &fakeProcessor{}
	worker, err := New(broker, process, DefaultConfig("queue-url"), nil)
	if err != nil {
		t.Fatal(err)
	}
	up := false
	worker.Ready = func(context.Context) error {
		if !up {
			return errors.New("connection refused")
		}
		return nil
	}
	for i := 0; i < 5; i++ {
		if err := worker.RunOnce(context.Background()); !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("RunOnce while down = %v", err)
		}
	}
	if broker.received != 0 || process.calls != 0 {
		t.Fatalf("received %d, processed %d while the dependency was down", broker.received, process.calls)
	}
	up = true
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broker.received != 1 || process.calls != 1 || broker.deleted != 1 {
		t.Fatalf("after recovery: received %d, processed %d, deleted %d", broker.received, process.calls, broker.deleted)
	}
}

// After a transient failure the rest of the batch is released unprocessed
// while the dependency stays down, instead of spending an attempt each.
func TestTransientFailureReleasesRestOfBatchWhileDown(t *testing.T) {
	attrs := map[string]string{"ApproximateReceiveCount": "1"}
	broker := &fakeSQS{messages: []types.Message{
		{Body: aws.String(validBody), ReceiptHandle: aws.String("one"), Attributes: attrs},
		{Body: aws.String(validBody), ReceiptHandle: aws.String("two"), Attributes: attrs},
		{Body: aws.String(validBody), ReceiptHandle: aws.String("three"), Attributes: attrs},
	}}
	process := &fakeProcessor{err: errors.New("connection refused")}
	worker, err := New(broker, process, DefaultConfig("queue-url"), nil)
	if err != nil {
		t.Fatal(err)
	}
	down := false
	worker.Ready = func(context.Context) error {
		if down {
			return errors.New("connection refused")
		}
		return nil
	}
	process.err = errors.New("connection refused")
	// The first Execute fails and the dependency is then reported down.
	worker.Service = failAndGoDown{process: process, down: &down}
	if err := worker.RunOnce(context.Background()); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("RunOnce = %v, want ErrDependencyUnavailable", err)
	}
	if process.calls != 1 {
		t.Fatalf("Execute called %d times, want 1", process.calls)
	}
	// First message: backoff; the other two: released immediately (0).
	if len(broker.changed) != 3 || broker.changed[0] != 1 || broker.changed[1] != 0 || broker.changed[2] != 0 {
		t.Fatalf("visibility changes %+v, want [1 0 0]", broker.changed)
	}
	if broker.deleted != 0 {
		t.Fatal("deleted an uncommitted receipt")
	}
}

type failAndGoDown struct {
	process *fakeProcessor
	down    *bool
}

func (f failAndGoDown) Execute(ctx context.Context, op contract.Operation, key, id string, in *wager.Inbox) (wager.Result, error) {
	*f.down = true
	return f.process.Execute(ctx, op, key, id, in)
}

func TestBackoffIsExponentialAndBounded(t *testing.T) {
	want := []int32{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		if got := backoffSeconds(i+1, 60); got != w {
			t.Errorf("attempt %d: backoff %d, want %d", i+1, got, w)
		}
	}
	if got := backoffSeconds(1000, 60); got != 60 {
		t.Errorf("large attempt: %d", got)
	}
	if got := backoffSeconds(0, 60); got != 1 {
		t.Errorf("attempt 0: %d", got)
	}
}
