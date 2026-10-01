package reference

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"wagering/internal/domain"
	wagering "wagering/internal/usecase/wagering"
)

// Claimer leases due PENDING/PENDING_REFERENCE rows; *pg.Store implements it.
type Claimer interface {
	ClaimTransactions(ctx context.Context, owner string, limit int, lease time.Duration) ([]domain.UUID, error)
}

// Retrier applies one claimed row under the wallet lock; *wagering.Service
// implements it. It is the only path that writes financial state, and it
// refuses (wagering.ErrClaimNotHeld) unless owner holds a live lease on a due row.
type Retrier interface {
	RetryPending(ctx context.Context, id domain.UUID, owner string) (wagering.Result, error)
}

// Result classifies what one attempt did to its row.
type Result string

const (
	// ResultResolved: the reference arrived and the operation was applied.
	ResultResolved Result = "RESOLVED"
	// ResultRejected: definitive rejection (expiry, failed reference, ...).
	ResultRejected Result = "REJECTED"
	// ResultPending: still waiting; another attempt was scheduled with backoff.
	ResultPending Result = "PENDING"
	// ResultStale: another worker already finished or took over the row, or the
	// lease expired before the attempt; no effect here and no attempt consumed.
	ResultStale Result = "STALE"
	// ResultError: a transient or unexpected error; the row keeps its lease
	// and becomes claimable again when the lease expires.
	ResultError Result = "ERROR"
)

// Attempt describes one processed claim for metrics hooks (REQ-076).
type Attempt struct {
	ID          domain.UUID
	Result      Result
	FailureCode domain.FailureCode // set for ResultRejected
	Duration    time.Duration
	Err         error // set for ResultError
}

// Observer receives worker events. Implementations must not block.
type Observer interface {
	Claimed(n int)
	Attempted(Attempt)
}

type nopObserver struct{}

func (nopObserver) Claimed(int)       {}
func (nopObserver) Attempted(Attempt) {}

// Stats summarizes one pass.
type Stats struct {
	Claimed, Resolved, Rejected, Pending, Stale, Errors int
}

// Worker claims and resumes pending references. It is safe to run any number
// of instances against one database.
type Worker struct {
	cfg      Config
	claimer  Claimer
	retrier  Retrier
	observer Observer
	log      *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	abort  context.CancelFunc
	done   chan struct{}
}

// NewWorker validates cfg and fills in a lease owner when none is set.
func NewWorker(cfg Config, claimer Claimer, retrier Retrier, observer Observer, log *slog.Logger) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if claimer == nil || retrier == nil {
		return nil, errors.New("reference worker: claimer and retrier are required")
	}
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		cfg.Owner = fmt.Sprintf("reference:%s:%d:%s", host, os.Getpid(), domain.NewUUID().String()[:8])
	}
	if observer == nil {
		observer = nopObserver{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Worker{cfg: cfg, claimer: claimer, retrier: retrier, observer: observer, log: log.With("worker", "reference", "owner", cfg.Owner)}, nil
}

// Owner returns the lease owner written to claimed rows.
func (w *Worker) Owner() string { return w.cfg.Owner }

// RunOnce claims one batch of due rows and processes it. A claim error is
// returned; per-row errors are counted in Stats and observed, never returned,
// so one bad row cannot hold back the rest of the batch.
func (w *Worker) RunOnce(ctx context.Context) (Stats, error) {
	return w.runOnce(ctx, context.WithoutCancel(ctx))
}

// runOnce claims under claimCtx and processes rows under itemParent, so a
// graceful stop can let in-flight rows finish while refusing new claims.
func (w *Worker) runOnce(claimCtx, itemParent context.Context) (Stats, error) {
	ids, err := w.claimer.ClaimTransactions(claimCtx, w.cfg.Owner, w.cfg.BatchSize, w.cfg.Lease)
	if err != nil {
		return Stats{}, fmt.Errorf("claim pending references: %w", err)
	}
	w.observer.Claimed(len(ids))
	st := Stats{Claimed: len(ids)}
	if len(ids) == 0 {
		return st, nil
	}
	work := make(chan domain.UUID)
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := min(w.cfg.Concurrency, len(ids))
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range work {
				a := w.attempt(itemParent, id)
				mu.Lock()
				switch a.Result {
				case ResultResolved:
					st.Resolved++
				case ResultRejected:
					st.Rejected++
				case ResultPending:
					st.Pending++
				case ResultStale:
					st.Stale++
				default:
					st.Errors++
				}
				mu.Unlock()
			}
		}()
	}
	// Rows not handed out before cancellation keep their lease and are taken
	// over by the next instance (or this one after restart) once it expires.
feed:
	for _, id := range ids {
		select {
		case work <- id:
		case <-claimCtx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	return st, nil
}

func (w *Worker) attempt(parent context.Context, id domain.UUID) Attempt {
	ctx, cancel := context.WithTimeout(parent, w.cfg.ItemTimeout)
	defer cancel()
	started := time.Now()
	res, err := w.retrier.RetryPending(ctx, id, w.cfg.Owner)
	a := Attempt{ID: id, Duration: time.Since(started)}
	switch {
	case errors.Is(err, wagering.ErrClaimNotHeld):
		a.Result = ResultStale
		w.log.Info("reference claim lost before retry; leaving the row to its current owner", "transactionId", id.String())
	case err != nil:
		a.Result, a.Err = ResultError, err
		if parent.Err() == nil {
			w.log.Warn("reference retry failed; lease expiry will reschedule", "transactionId", id.String(), "error", err)
		}
	case res.Replay:
		a.Result = ResultStale
	default:
		switch res.Transaction.Status() {
		case domain.StatusProcessed:
			a.Result = ResultResolved
		case domain.StatusRejected, domain.StatusFailed:
			a.Result, a.FailureCode = ResultRejected, res.Transaction.FailureCode()
		default:
			a.Result = ResultPending
		}
	}
	if a.Result == ResultRejected {
		w.log.Info("pending reference rejected", "transactionId", id.String(), "failureCode", string(a.FailureCode))
	}
	w.observer.Attempted(a)
	return a
}

// Run polls until ctx is cancelled. Cancelling ctx stops claiming; rows already
// in flight finish, each bounded by ItemTimeout.
func (w *Worker) Run(ctx context.Context) error {
	return w.run(ctx, context.WithoutCancel(ctx))
}

func (w *Worker) run(claimCtx, itemParent context.Context) error {
	for claimCtx.Err() == nil {
		st, err := w.runOnce(claimCtx, itemParent)
		if err != nil && claimCtx.Err() == nil {
			w.log.Error("claim failed", "error", err)
		}
		if err == nil && st.Claimed >= w.cfg.BatchSize {
			continue // a full batch suggests more due work
		}
		t := time.NewTimer(w.cfg.PollInterval)
		select {
		case <-claimCtx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	return nil
}

// Start launches the polling loop in the background. It is idempotent.
func (w *Worker) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return
	}
	loopCtx, cancel := context.WithCancel(context.Background())
	hardCtx, abort := context.WithCancel(context.Background())
	done := make(chan struct{})
	w.cancel, w.abort, w.done = cancel, abort, done
	go func() {
		defer close(done)
		_ = w.run(loopCtx, hardCtx)
	}()
}

// Stop stops claiming and waits for in-flight rows. If ctx expires first it
// cancels them: each runs in one SQL transaction, so the rollback loses no
// state, and the row stays leased until the lease expires.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	cancel, abort, done := w.cancel, w.abort, w.done
	w.cancel, w.abort, w.done = nil, nil, nil
	w.mu.Unlock()
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
		return fmt.Errorf("reference worker stopped before in-flight work finished: %w", ctx.Err())
	}
}
