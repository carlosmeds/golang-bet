package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"wagering/internal/domain"
	"wagering/internal/storage/pg"
)

// Store is the outbox persistence the publisher needs; *pg.Store implements it.
type Store interface {
	ClaimOutbox(ctx context.Context, owner string, limit int, lease time.Duration) ([]pg.ClaimedEvent, error)
	MarkPublished(ctx context.Context, event domain.UUID, owner string) error
	RetryOutbox(ctx context.Context, event domain.UUID, owner string, after time.Duration, reason string) error
}

// Message is one event ready for the outbound FIFO queue (D15). Body is the
// stored envelope exactly as PostgreSQL returns the jsonb snapshot (key order
// normalized by jsonb), so every publication of an event carries the same bytes. MessageGroupID is the aggregateId and
// MessageDeduplicationID the eventId, so republication keeps the same identity.
type Message struct {
	EventID                domain.UUID
	EventType              domain.EventType
	Body                   []byte
	MessageGroupID         string
	MessageDeduplicationID string
}

// Sender delivers a message to the outbound queue and returns only after the
// broker has accepted it. It must respect ctx. *sqsclient adapters implement it.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// Result classifies what one publish attempt did to its event.
type Result string

const (
	// ResultPublished: the broker accepted the event and the row is published.
	ResultPublished Result = "PUBLISHED"
	// ResultRetry: the send failed; another attempt is scheduled with backoff.
	ResultRetry Result = "RETRY"
	// ResultLeaseLost: the send succeeded (or failed) but the lease had expired
	// or been taken over, so the row was left for its new owner. A repeat
	// publication with the same eventId is possible.
	ResultLeaseLost Result = "LEASE_LOST"
	// ResultSkipped: not attempted because an earlier event of the same
	// aggregate failed, the lease budget ran out or the publisher is stopping.
	// The row keeps its lease and is claimed again when it expires.
	ResultSkipped Result = "SKIPPED"
	// ResultError: the outcome could not be recorded (database error) or the
	// stored event is malformed. The row is claimed again after the lease.
	ResultError Result = "ERROR"
)

// Attempt describes one processed claim for metrics hooks (REQ-076).
type Attempt struct {
	EventID   domain.UUID
	EventType domain.EventType
	Result    Result
	// Attempts is the number of earlier failed attempts recorded for the event.
	Attempts int
	// Lag is the time from occurrence to confirmed publication (outbox delay);
	// set for ResultPublished.
	Lag      time.Duration
	Duration time.Duration
	Err      error // send or bookkeeping error, without payload
}

// Observer receives publisher events. Implementations must not block.
type Observer interface {
	Claimed(n int)
	Attempted(Attempt)
}

type nopObserver struct{}

func (nopObserver) Claimed(int)       {}
func (nopObserver) Attempted(Attempt) {}

// Stats summarizes one pass.
type Stats struct {
	Claimed, Published, Retried, LeaseLost, Skipped, Errors int
}

// Publisher claims and publishes outbox events. It is safe to run any number of
// instances against one database.
type Publisher struct {
	cfg      Config
	store    Store
	sender   Sender
	observer Observer
	log      *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	abort  context.CancelFunc
	done   chan struct{}
}

// NewPublisher validates cfg and fills in a lease owner when none is set.
func NewPublisher(cfg Config, store Store, sender Sender, observer Observer, log *slog.Logger) (*Publisher, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if store == nil || sender == nil {
		return nil, errors.New("outbox publisher: store and sender are required")
	}
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		cfg.Owner = fmt.Sprintf("outbox:%s:%d:%s", host, os.Getpid(), domain.NewUUID().String()[:8])
	}
	if observer == nil {
		observer = nopObserver{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Publisher{cfg: cfg, store: store, sender: sender, observer: observer, now: time.Now,
		log: log.With("worker", "outbox", "owner", cfg.Owner)}, nil
}

// Owner returns the lease owner written to claimed rows.
func (p *Publisher) Owner() string { return p.cfg.Owner }

// RunOnce claims one batch of due events and publishes it. A claim error is
// returned; per-event errors are counted in Stats and observed, never returned,
// so one bad event cannot hold back the rest of the batch.
func (p *Publisher) RunOnce(ctx context.Context) (Stats, error) {
	return p.runOnce(ctx, context.WithoutCancel(ctx))
}

// runOnce claims under claimCtx and publishes under itemParent, so a graceful
// stop lets the event in flight finish while refusing new claims and new sends.
func (p *Publisher) runOnce(claimCtx, itemParent context.Context) (Stats, error) {
	// Taken before the claim, so the real lease (which starts at the database
	// clock after this point) always outlives the budget computed from it.
	claimedAt := p.now()
	events, err := p.store.ClaimOutbox(claimCtx, p.cfg.Owner, p.cfg.BatchSize, p.cfg.Lease)
	if err != nil {
		return Stats{}, fmt.Errorf("claim outbox events: %w", err)
	}
	if len(events) > 0 {
		if err := afterOutboxClaim(claimCtx); err != nil {
			return Stats{}, err
		}
	}
	p.observer.Claimed(len(events))
	st := Stats{Claimed: len(events)}
	if len(events) == 0 {
		return st, nil
	}
	budgetEnd := claimedAt.Add(p.cfg.Lease)

	// ClaimOutbox returns rows through UPDATE ... RETURNING, which does not keep
	// the claim query's ORDER BY, so restore outbox order explicitly.
	sort.SliceStable(events, func(i, j int) bool { return events[i].Seq < events[j].Seq })

	// Events of one aggregate keep their outbox order: they are published one
	// after another, and after a failure the rest of that aggregate is skipped.
	// Distinct aggregates are independent and run in parallel.
	groups := groupByAggregate(events)
	work := make(chan []pg.ClaimedEvent)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < min(p.cfg.Concurrency, len(groups)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				results := p.publishGroup(claimCtx, itemParent, g, budgetEnd)
				mu.Lock()
				for _, r := range results {
					switch r {
					case ResultPublished:
						st.Published++
					case ResultRetry:
						st.Retried++
					case ResultLeaseLost:
						st.LeaseLost++
					case ResultSkipped:
						st.Skipped++
					default:
						st.Errors++
					}
				}
				mu.Unlock()
			}
		}()
	}
	// Groups not handed out before cancellation keep their lease and are taken
	// over once it expires.
feed:
	for _, g := range groups {
		select {
		case work <- g:
		case <-claimCtx.Done():
			st.Skipped += len(g)
			break feed
		}
	}
	close(work)
	wg.Wait()
	return st, nil
}

type parsed struct {
	eventType     domain.EventType
	aggregateID   string
	correlationID string
	occurredAt    time.Time
}

// header reads the envelope header of a stored snapshot and checks that it
// belongs to the claimed row.
func header(e pg.ClaimedEvent) (parsed, error) {
	var h domain.EventMeta
	if err := json.Unmarshal(e.Payload, &h); err != nil {
		return parsed{}, errors.New("stored outbox payload is not a JSON event envelope")
	}
	if h.EventID != e.EventID || h.AggregateID.String() == "" || h.EventType == "" {
		return parsed{}, errors.New("stored outbox payload header does not match its row")
	}
	return parsed{eventType: h.EventType, aggregateID: h.AggregateID.String(), correlationID: h.CorrelationID, occurredAt: h.OccurredAt}, nil
}

func groupByAggregate(events []pg.ClaimedEvent) [][]pg.ClaimedEvent {
	index := map[string]int{}
	var groups [][]pg.ClaimedEvent
	for _, e := range events {
		key := ""
		if h, err := header(e); err == nil {
			key = h.aggregateID
		} else {
			key = "invalid:" + e.EventID.String() // isolated; reported as an error
		}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], e)
	}
	return groups
}

func (p *Publisher) publishGroup(claimCtx, itemParent context.Context, group []pg.ClaimedEvent, budgetEnd time.Time) []Result {
	results := make([]Result, 0, len(group))
	blocked := false
	for _, e := range group {
		switch {
		case blocked, claimCtx.Err() != nil, p.now().Add(p.cfg.SendTimeout + p.cfg.AckTimeout).After(budgetEnd):
			// Sending now could outlive the lease (or we are stopping, or an
			// earlier event of this aggregate failed and must go first).
			p.observer.Attempted(Attempt{EventID: e.EventID, Result: ResultSkipped, Attempts: e.AttemptCount})
			results = append(results, ResultSkipped)
			continue
		}
		a := p.publish(itemParent, e)
		results = append(results, a.Result)
		if a.Result != ResultPublished {
			blocked = true
		}
	}
	return results
}

// publish sends one event and records the outcome. The claimed lease is the
// only thing that makes the acknowledgement valid: if it expired meanwhile,
// the store refuses and the new owner decides what happens next.
func (p *Publisher) publish(parent context.Context, e pg.ClaimedEvent) Attempt {
	started := p.now()
	a := Attempt{EventID: e.EventID, Attempts: e.AttemptCount}
	h, err := header(e)
	if err != nil {
		a.Result, a.Err = ResultError, err
		p.log.Error("outbox event is malformed", "eventId", e.EventID.String(), "error", err)
		return p.finish(a, started)
	}
	a.EventType = h.eventType

	sendCtx, cancelSend := context.WithTimeout(parent, p.cfg.SendTimeout)
	sendErr := p.sender.Send(sendCtx, Message{
		EventID:                e.EventID,
		EventType:              h.eventType,
		Body:                   e.Payload,
		MessageGroupID:         h.aggregateID,
		MessageDeduplicationID: e.EventID.String(),
	})
	cancelSend()

	ackCtx, cancelAck := context.WithTimeout(parent, p.cfg.AckTimeout)
	defer cancelAck()
	if sendErr == nil {
		switch err := p.store.MarkPublished(ackCtx, e.EventID, p.cfg.Owner); {
		case err == nil:
			a.Result = ResultPublished
			a.Lag = max(0, p.now().Sub(h.occurredAt))
			p.log.Info("outbox event published", "eventId", e.EventID.String(), "eventType", string(h.eventType), "aggregateId", h.aggregateID, "correlationId", h.correlationID, "outboxLagMs", a.Lag.Milliseconds())
		case errors.Is(err, pg.ErrConcurrentUpdate):
			a.Result, a.Err = ResultLeaseLost, err
			p.log.Warn("outbox lease lost after publish; the event may be published again with the same eventId", "eventId", e.EventID.String(), "aggregateId", h.aggregateID, "correlationId", h.correlationID)
		default:
			// Published but not recorded: it will be sent again, same eventId.
			a.Result, a.Err = ResultError, err
			p.log.Warn("outbox publish not recorded; the event will be published again with the same eventId", "eventId", e.EventID.String(), "aggregateId", h.aggregateID, "correlationId", h.correlationID, "error", reason(err))
		}
		return p.finish(a, started)
	}

	a.Err = sendErr
	delay := p.cfg.Backoff.Delay(e.AttemptCount + 1)
	switch err := p.store.RetryOutbox(ackCtx, e.EventID, p.cfg.Owner, delay, reason(sendErr)); {
	case err == nil:
		a.Result = ResultRetry
		if parent.Err() == nil {
			p.log.Warn("outbox publish failed; retry scheduled", "eventId", e.EventID.String(), "eventType", string(h.eventType), "aggregateId", h.aggregateID, "correlationId", h.correlationID, "retryIn", delay, "error", reason(sendErr))
		}
	case errors.Is(err, pg.ErrConcurrentUpdate):
		a.Result = ResultLeaseLost
	default:
		a.Result, a.Err = ResultError, errors.Join(sendErr, err)
	}
	return p.finish(a, started)
}

func (p *Publisher) finish(a Attempt, started time.Time) Attempt {
	a.Duration = p.now().Sub(started)
	p.observer.Attempted(a)
	return a
}

// reason is the last_error text: the error message only, bounded, no payload.
func reason(err error) string {
	const limit = 500
	s := err.Error()
	if len(s) > limit {
		s = s[:limit]
	}
	return s
}

// Run polls until ctx is cancelled. Cancelling ctx stops claiming; an event
// already being sent finishes, bounded by SendTimeout and AckTimeout.
func (p *Publisher) Run(ctx context.Context) error {
	return p.run(ctx, context.WithoutCancel(ctx))
}

func (p *Publisher) run(claimCtx, itemParent context.Context) error {
	for claimCtx.Err() == nil {
		st, err := p.runOnce(claimCtx, itemParent)
		if err != nil && claimCtx.Err() == nil {
			p.log.Error("claim failed", "error", err)
		}
		if err == nil && st.Claimed >= p.cfg.BatchSize && st.Skipped == 0 {
			continue // a full, fully processed batch suggests more due work
		}
		t := time.NewTimer(p.cfg.PollInterval)
		select {
		case <-claimCtx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// Start launches the polling loop in the background. It is idempotent.
func (p *Publisher) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	hardCtx, abort := context.WithCancel(context.Background())
	done := make(chan struct{})
	p.cancel, p.abort, p.done = cancel, abort, done
	go func() {
		defer close(done)
		_ = p.run(loopCtx, hardCtx)
	}()
}

// Stop stops claiming and waits for events in flight. If ctx expires first it
// cancels them: the row keeps its lease, which expires, and the event is then
// published again with the same eventId.
func (p *Publisher) Stop(ctx context.Context) error {
	p.mu.Lock()
	cancel, abort, done := p.cancel, p.abort, p.done
	p.cancel, p.abort, p.done = nil, nil, nil
	p.mu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		abort()
		return nil
	case <-ctx.Done():
		abort()
		<-done
		return fmt.Errorf("outbox publisher stopped before in-flight work finished: %w", ctx.Err())
	}
}
