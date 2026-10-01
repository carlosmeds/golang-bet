package httpauth_test

import (
	"net/http"
	"strings"
	"testing"

	"wagering/tests/integration/auth/idptest"
)

func TestComposedServiceReadinessAndMetrics(t *testing.T) {
	app := idptest.StartApp(t)
	ready := app.Do(idptest.Request{Method: http.MethodGet, Path: "/health/ready"})
	if ready.Status != http.StatusOK || string(ready.Raw) != "ready\n" {
		t.Fatalf("readiness: status %d body %q", ready.Status, ready.Raw)
	}
	metrics := app.Do(idptest.Request{Method: http.MethodGet, Path: "/metrics"})
	if metrics.Status != http.StatusOK || !strings.Contains(string(metrics.Raw), "wagering_duplicate_requests_total") {
		t.Fatalf("metrics: status %d body %q", metrics.Status, metrics.Raw)
	}
}
