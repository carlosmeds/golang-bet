package observability

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposeBoundedCounters(t *testing.T) {
	m := NewMetrics()
	m.RecordHTTP(201, 25*time.Millisecond, false)
	m.CountDuplicate()
	m.CountConflict()
	m.RecordReconciliation(false)
	m.RecordOutboxPublished(3 * time.Second)
	r := httptest.NewRecorder()
	m.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
	for _, want := range []string{"wagering_http_requests_total{status=\"201\"} 1", "wagering_duplicate_requests_total 1", "wagering_conflicts_total 1", "wagering_reconciliation_divergences_total 1", "wagering_outbox_lag_milliseconds_total 3000"} {
		if !strings.Contains(r.Body.String(), want) {
			t.Fatalf("metrics missing %q: %s", want, r.Body.String())
		}
	}
}
