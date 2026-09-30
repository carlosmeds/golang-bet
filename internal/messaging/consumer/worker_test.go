package consumer

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/contract"
	"wagering/internal/domain"
	wager "wagering/internal/usecase/wagering"
)

const validBody = `{"messageId":"message-1","type":"WagerTransactionRequested","occurredAt":"2026-09-30T12:00:00Z","data":{"providerId":"provider-a","externalTransactionId":"ext-1","idempotencyKey":"key-1","playerId":"player-1","walletId":"a1d3f476-b151-4644-aece-0871b4e22b7e","roundId":"r-1","gameId":"g-1","kind":"BET","money":{"amount":"1.00","currency":"BRL"}}}`

type fakeSQS struct {
	message types.Message
	deleted int
	changed []int32
}

func (f *fakeSQS) ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
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
	err   error
	calls int
	inbox *wager.Inbox
}

func (f *fakeProcessor) Execute(_ context.Context, _ contract.Operation, _, _ string, inbox *wager.Inbox) (wager.Result, error) {
	f.calls++
	f.inbox = inbox
	return wager.Result{}, f.err
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
