package consumer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"wagering/internal/contract"
	"wagering/internal/domain"
	wager "wagering/internal/usecase/wagering"
)

// API is the subset of SQS needed by the inbound consumer; the production
// client is AWS SDK v2, while tests can inject a deterministic broker.
type API interface {
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}
type Config struct {
	QueueURL          string
	ConsumerName      string
	WaitSeconds       int32
	VisibilitySeconds int32
	MaxMessages       int32
	PollErrorDelay    time.Duration
}

func DefaultConfig(queue string) Config {
	return Config{QueueURL: queue, ConsumerName: "wager-transactions", WaitSeconds: 10, VisibilitySeconds: 60, MaxMessages: 10, PollErrorDelay: time.Second}
}
func (c Config) Validate() error {
	if c.QueueURL == "" || c.ConsumerName == "" || c.WaitSeconds < 0 || c.WaitSeconds > 20 || c.VisibilitySeconds < 1 || c.VisibilitySeconds > 43200 || c.MaxMessages < 1 || c.MaxMessages > 10 || c.PollErrorDelay <= 0 {
		return errors.New("invalid SQS consumer configuration")
	}
	return nil
}

type Processor interface {
	Execute(context.Context, contract.Operation, string, string, *wager.Inbox) (wager.Result, error)
}
type Observer interface {
	CountRetry()
	CountDLQ()
	CountDuplicate()
	CountConflict()
}

type Worker struct {
	API      API
	Service  Processor
	Config   Config
	Logger   *slog.Logger
	Observer Observer
	cancel   context.CancelFunc
	done     chan struct{}
	mu       sync.Mutex
}

func New(api API, svc Processor, c Config, logger *slog.Logger, observers ...Observer) (*Worker, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if api == nil || svc == nil {
		return nil, errors.New("SQS API and wagering service required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	var observer Observer
	if len(observers) > 0 {
		observer = observers[0]
	}
	return &Worker{API: api, Service: svc, Config: c, Logger: logger, Observer: observer}, nil
}
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go func() { defer close(w.done); w.Run(ctx) }()
}
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			w.Logger.ErrorContext(ctx, "SQS poll failed", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.Config.PollErrorDelay):
			}
		}
	}
}
func (w *Worker) RunOnce(ctx context.Context) error {
	c := w.Config
	out, err := w.API.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: aws.String(c.QueueURL), MaxNumberOfMessages: c.MaxMessages, WaitTimeSeconds: c.WaitSeconds, VisibilityTimeout: c.VisibilitySeconds, MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameApproximateReceiveCount}})
	if err != nil {
		return err
	}
	for i, message := range out.Messages {
		if ctx.Err() != nil {
			for _, unstarted := range out.Messages[i:] {
				_ = w.release(context.Background(), unstarted)
			}
			return ctx.Err()
		}
		if err := w.handle(ctx, message); err != nil {
			w.Logger.WarnContext(ctx, "SQS message not committed", "sqsMessageId", aws.ToString(message.MessageId), "error", err)
			if ctx.Err() != nil {
				_ = w.release(context.Background(), message)
			}
		}
	}
	return nil
}
func (w *Worker) handle(ctx context.Context, m types.Message) error {
	body := aws.ToString(m.Body)
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	receipt := aws.ToString(m.ReceiptHandle)
	request, err := contract.ParseSQS([]byte(body))
	if err != nil {
		return w.deferPoison(ctx, m, err)
	}
	result, err := w.Service.Execute(ctx, request.Data.Operation, request.Data.IdempotencyKey, request.MessageID, &wager.Inbox{Consumer: w.Config.ConsumerName, MessageID: request.MessageID, PayloadHash: hash})
	if err != nil {
		w.Logger.WarnContext(ctx, "SQS wager was not durably accepted", "messageId", request.MessageID, "correlationId", request.MessageID, "providerId", request.Data.ProviderID, "externalTransactionId", request.Data.ExternalTransactionID, "error", err)
		var de *domain.Error
		if errors.As(err, &de) && de.Kind == domain.ErrKindConflict && w.Observer != nil {
			w.Observer.CountConflict()
		}
		if permanent(err) {
			return w.deferPoison(ctx, m, err)
		}
		if w.Observer != nil {
			w.Observer.CountRetry()
		}
		count := receiveCount(m)
		delay := backoffSeconds(count)
		_, changeErr := w.API.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.Config.QueueURL), ReceiptHandle: aws.String(receipt), VisibilityTimeout: delay})
		if changeErr != nil {
			return errors.Join(err, changeErr)
		}
		return err
	}
	// PROCESSED, REJECTED and PENDING_REFERENCE all have durable records. The
	// latter is resumed by the reference worker, independent of this SQS receipt.
	if result.Replay && w.Observer != nil {
		w.Observer.CountDuplicate()
	}
	attrs := []any{"messageId", request.MessageID, "correlationId", request.MessageID, "providerId", request.Data.ProviderID, "externalTransactionId", request.Data.ExternalTransactionID, "idempotentReplay", result.Replay}
	if result.Transaction != nil {
		attrs = append(attrs, "transactionId", result.Transaction.ID().String(), "walletId", result.Transaction.WalletID().String(), "status", string(result.Transaction.Status()))
	}
	w.Logger.InfoContext(ctx, "SQS wager durably processed", attrs...)
	if err := afterDurableCommit(ctx, w.API, w.Config.QueueURL, m); err != nil {
		return err
	}
	_, err = w.API.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(w.Config.QueueURL), ReceiptHandle: aws.String(receipt)})
	return err
}
func (w *Worker) deferPoison(ctx context.Context, m types.Message, cause error) error {
	if w.Observer != nil {
		w.Observer.CountDLQ()
	}
	// SQS redrive moves a poison message to the configured DLQ after its max
	// receive count. Leave the receipt unacknowledged and shorten visibility.
	_, err := w.API.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.Config.QueueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 1})
	if err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
func (w *Worker) release(ctx context.Context, m types.Message) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := w.API.ChangeMessageVisibility(c, &sqs.ChangeMessageVisibilityInput{QueueUrl: aws.String(w.Config.QueueURL), ReceiptHandle: m.ReceiptHandle, VisibilityTimeout: 0})
	return err
}
func receiveCount(m types.Message) int {
	n, _ := strconv.Atoi(m.Attributes[string(types.MessageSystemAttributeNameApproximateReceiveCount)])
	if n < 1 {
		return 1
	}
	return n
}
func backoffSeconds(attempt int) int32 {
	n := int32(1)
	for i := 1; i < attempt && n < 300; i++ {
		n *= 2
	}
	if n > 300 {
		return 300
	}
	return n
}
func permanent(err error) bool {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Kind == domain.ErrKindConflict || de.Kind == domain.ErrKindInvalid || de.Kind == domain.ErrKindPermanent
	}
	return errors.Is(err, wager.ErrExternalIDConflict) || errors.Is(err, wager.ErrInboxIncomplete)
}
