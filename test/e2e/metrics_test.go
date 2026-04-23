//go:build e2e

package e2e_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestE2E_MetricsExposed covers 5.1. After the controller pod is up we hit
// its /metrics endpoint and look for the keeper namespace in the exposition
// text. We do not verify every metric — the unit tests already cover which
// metrics exist; this is the smoke test that the scrape path is actually
// wired on a real cluster.
//
// The NodePort route (127.0.0.1:30080 → Service → Pod:8080) can be slow to
// settle after a fresh kind cluster. Retry for up to 60s on empty bodies
// and transient connection errors before giving up.
func TestE2E_MetricsExposed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	url := metricsURL(t)
	deadline := time.Now().Add(60 * time.Second)
	var lastBody, lastStatus string

	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastStatus = "dial error: " + err.Error()
			time.Sleep(3 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastStatus = resp.Status
		lastBody = string(body)

		// We assert on Gauges only. CounterVec metrics like
		// mysql_keeper_switchover_total are lazy-registered — they appear in
		// /metrics exposition only after the first WithLabelValues(...).Inc(),
		// which requires an actual switchover attempt and is out of scope
		// for this smoke test. The reconciler calls Set() on
		// mysql_keeper_cluster_healthy every reconcile cycle, so we look
		// for that plus the generic "mysql_keeper_" prefix HELP line to
		// prove the whole keeper namespace is wired.
		if resp.StatusCode == http.StatusOK &&
			strings.Contains(lastBody, "mysql_keeper_cluster_healthy") &&
			strings.Contains(lastBody, "mysql_keeper_cluster_writable") {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("metrics never returned expected series; last status=%q bodyLen=%d head=\n%s",
		lastStatus, len(lastBody), head(lastBody, 1024))
}

// head returns the first n bytes of s for log-friendly truncation.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
