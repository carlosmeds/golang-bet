package observability

import (
	"fmt"
	"go.uber.org/fx"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"wagering/internal/domain"
	"wagering/internal/workers/outbox"
	"wagering/internal/workers/reference"
)

// Metrics is a small Prometheus text endpoint with bounded label values. It
// deliberately never records IDs, credentials, payloads, or caller input.
type Metrics struct {
	mu                    sync.Mutex
	http                  map[string]*atomic.Uint64
	Duplicates            atomic.Uint64
	Retries               atomic.Uint64
	DLQ                   atomic.Uint64
	Conflicts             atomic.Uint64
	Reconciliations       atomic.Uint64
	Divergences           atomic.Uint64
	OutboxLagMillis       atomic.Uint64
	OutboxPublished       atomic.Uint64
	ProcessingMillis      atomic.Uint64
	ProcessingCount       atomic.Uint64
	transactions          [3][4]atomic.Uint64
	latency               [3]atomic.Uint64
	latencyCount          [3]atomic.Uint64
	OutboxUnpublished     atomic.Int64
	OutboxOldestAgeMillis atomic.Int64
	DLQVisible            atomic.Int64
}

var sources = [...]string{"http", "sqs", "reference"}
var statuses = [...]domain.Status{domain.StatusProcessed, domain.StatusRejected, domain.StatusPendingReference, domain.StatusFailed}

// RecordTransaction accepts only known sources and statuses, keeping labels bounded.
func (m *Metrics) RecordTransaction(source string, status domain.Status) {
	for i, s := range sources {
		if s != source {
			continue
		}
		for j, st := range statuses {
			if st == status {
				m.transactions[i][j].Add(1)
				return
			}
		}
	}
}

func (m *Metrics) RecordProcessing(source string, elapsed time.Duration) {
	for i, s := range sources {
		if s == source {
			m.latency[i].Add(uint64(max(0, elapsed.Milliseconds())))
			m.latencyCount[i].Add(1)
			return
		}
	}
}

type ReferenceObserver struct{ Metrics *Metrics }

func (o ReferenceObserver) Claimed(_ int) {}
func (o ReferenceObserver) Attempted(a reference.Attempt) {
	o.Metrics.RecordProcessing("reference", a.Duration)
	switch a.Result {
	case reference.ResultResolved:
		o.Metrics.RecordTransaction("reference", domain.StatusProcessed)
	case reference.ResultRejected:
		o.Metrics.RecordTransaction("reference", a.Status)
	case reference.ResultPending:
		o.Metrics.RecordTransaction("reference", domain.StatusPendingReference)
	}
	if a.Result == reference.ResultPending || a.Result == reference.ResultError {
		o.Metrics.Retries.Add(1)
	}
}

type OutboxObserver struct{ Metrics *Metrics }

func (o OutboxObserver) Claimed(_ int) {}
func (o OutboxObserver) Attempted(a outbox.Attempt) {
	switch a.Result {
	case outbox.ResultRetry, outbox.ResultError:
		o.Metrics.Retries.Add(1)
	case outbox.ResultPublished:
		o.Metrics.RecordOutboxPublished(a.Lag)
	}
}

func NewReferenceObserver(m *Metrics) ReferenceObserver { return ReferenceObserver{Metrics: m} }
func NewOutboxObserver(m *Metrics) OutboxObserver       { return OutboxObserver{Metrics: m} }

var Module = fx.Module("observability",
	fx.Provide(NewMetrics,
		NewSnapshot,
		fx.Annotate(NewReferenceObserver, fx.As(new(reference.Observer))),
		fx.Annotate(NewOutboxObserver, fx.As(new(outbox.Observer))),
	),
	fx.Invoke(StartSnapshot),
)

func NewMetrics() *Metrics { return &Metrics{http: make(map[string]*atomic.Uint64)} }

func (m *Metrics) RecordHTTP(status int, elapsed time.Duration, duplicate bool) {
	key := "other"
	switch status {
	case 200, 201, 202, 400, 401, 403, 404, 409, 422, 429, 500, 503:
		key = fmt.Sprintf("%d", status)
	}
	m.mu.Lock()
	counter := m.http[key]
	if counter == nil {
		counter = &atomic.Uint64{}
		m.http[key] = counter
	}
	m.mu.Unlock()
	counter.Add(1)
	m.ProcessingMillis.Add(uint64(max(0, elapsed.Milliseconds())))
	m.ProcessingCount.Add(1)
	m.RecordProcessing("http", elapsed)
	if duplicate {
		m.Duplicates.Add(1)
	}
	if status == http.StatusConflict {
		m.Conflicts.Add(1)
	}
}

func (m *Metrics) RecordReconciliation(consistent bool) {
	m.Reconciliations.Add(1)
	if !consistent {
		m.Divergences.Add(1)
	}
}

func (m *Metrics) RecordOutboxPublished(lag time.Duration) {
	m.OutboxPublished.Add(1)
	m.OutboxLagMillis.Add(uint64(max(0, lag.Milliseconds())))
}

func (m *Metrics) CountRetry()     { m.Retries.Add(1) }
func (m *Metrics) CountDLQ()       { m.DLQ.Add(1) }
func (m *Metrics) CountDuplicate() { m.Duplicates.Add(1) }
func (m *Metrics) CountConflict()  { m.Conflicts.Add(1) }

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		m.mu.Lock()
		keys := make([]string, 0, len(m.http))
		for key := range m.http {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, key := range keys {
			fmt.Fprintf(&b, "wagering_http_requests_total{status=%q} %d\n", key, m.http[key].Load())
		}
		m.mu.Unlock()
		fmt.Fprintf(&b, "wagering_duplicate_requests_total %d\n", m.Duplicates.Load())
		fmt.Fprintf(&b, "wagering_retries_total %d\n", m.Retries.Load())
		fmt.Fprintf(&b, "wagering_dlq_redrive_candidates_total %d\n", m.DLQ.Load())
		fmt.Fprintf(&b, "wagering_conflicts_total %d\n", m.Conflicts.Load())
		fmt.Fprintf(&b, "wagering_outbox_published_total %d\n", m.OutboxPublished.Load())
		fmt.Fprintf(&b, "wagering_outbox_lag_milliseconds_total %d\n", m.OutboxLagMillis.Load())
		fmt.Fprintf(&b, "wagering_processing_duration_milliseconds_total %d\n", m.ProcessingMillis.Load())
		fmt.Fprintf(&b, "wagering_processing_duration_count %d\n", m.ProcessingCount.Load())
		fmt.Fprintf(&b, "wagering_reconciliations_total %d\n", m.Reconciliations.Load())
		fmt.Fprintf(&b, "wagering_reconciliation_divergences_total %d\n", m.Divergences.Load())
		for i, source := range sources {
			for j, status := range statuses {
				fmt.Fprintf(&b, "wagering_transactions_total{source=%q,status=%q} %d\n", source, status, m.transactions[i][j].Load())
			}
			fmt.Fprintf(&b, "wagering_processing_duration_milliseconds_total{source=%q} %d\n", source, m.latency[i].Load())
			fmt.Fprintf(&b, "wagering_processing_duration_count{source=%q} %d\n", source, m.latencyCount[i].Load())
		}
		fmt.Fprintf(&b, "wagering_outbox_unpublished_count %d\n", m.OutboxUnpublished.Load())
		fmt.Fprintf(&b, "wagering_outbox_oldest_unpublished_age_milliseconds %d\n", m.OutboxOldestAgeMillis.Load())
		fmt.Fprintf(&b, "wagering_dlq_visible_messages %d\n", m.DLQVisible.Load())
		_, _ = w.Write([]byte(b.String()))
	})
}
