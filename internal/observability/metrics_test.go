package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wagering/internal/domain"
	"wagering/internal/workers/reference"
)

func TestMetricsExposeBoundedCounters(t *testing.T) {
	m := NewMetrics()
	m.RecordHTTP(201, 25*time.Millisecond, false)
	m.CountDuplicate()
	m.CountConflict()
	m.RecordReconciliation(false)
	m.RecordOutboxPublished(3 * time.Second)
	m.CountRetry()
	m.CountDLQ()
	r := httptest.NewRecorder()
	m.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{"wagering_http_requests_total{status=\"201\"} 1", "wagering_duplicate_requests_total 1", "wagering_conflicts_total 1", "wagering_retries_total 1", "wagering_dlq_redrive_candidates_total 1", "wagering_reconciliation_divergences_total 1", "wagering_outbox_lag_milliseconds_total 3000"} {
		if !strings.Contains(r.Body.String(), want) {
			t.Fatalf("metrics missing %q: %s", want, r.Body.String())
		}
	}
}

func TestLiveMetricsScrapeTracksBoundedOutcomesAndLatency(t *testing.T) {
	m := NewMetrics()
	m.RecordTransaction("http", domain.StatusProcessed)
	m.RecordTransaction("sqs", domain.StatusPendingReference)
	m.RecordTransaction("sqs", domain.StatusFailed)
	m.RecordTransaction("untrusted-id", domain.StatusProcessed)
	m.RecordProcessing("sqs", 12*time.Millisecond)
	NewReferenceObserver(m).Attempted(reference.Attempt{Result: reference.ResultResolved, Duration: 7 * time.Millisecond})
	NewReferenceObserver(m).Attempted(reference.Attempt{Result: reference.ResultRejected, Status: domain.StatusFailed, Duration: 9 * time.Millisecond})
	m.OutboxUnpublished.Store(3)
	m.OutboxOldestAgeMillis.Store(4200)
	m.DLQVisible.Store(2)
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{
		`wagering_transactions_total{source="http",status="PROCESSED"} 1`,
		`wagering_transactions_total{source="sqs",status="PENDING_REFERENCE"} 1`,
		`wagering_transactions_total{source="sqs",status="FAILED"} 1`,
		`wagering_transactions_total{source="reference",status="PROCESSED"} 1`,
		`wagering_transactions_total{source="reference",status="FAILED"} 1`,
		`wagering_processing_duration_milliseconds_total{source="sqs"} 12`,
		`wagering_processing_duration_count{source="reference"} 2`,
		`wagering_outbox_unpublished_count 3`,
		`wagering_outbox_oldest_unpublished_age_milliseconds 4200`,
		`wagering_dlq_visible_messages 2`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("scrape missing %q", want)
		}
	}
	if strings.Contains(got, "untrusted-id") {
		t.Fatal("unbounded source leaked into metrics")
	}
}
