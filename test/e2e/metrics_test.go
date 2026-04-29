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

// TestE2E_MetricsExposed covers 5.1. We create a CSP so the controller
// reconciles it and calls updateMetrics (which registers the GaugeVec labels).
// Only after at least one reconcile cycle will mysql_keeper_cluster_healthy
// appear in the /metrics exposition — polling without a CSP would always see
// an empty keeper namespace.
func TestE2E_MetricsExposed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c := newClient(t)
	name := "e2e-metrics-" + shortUUID(t)

	csp := fixtureCSP(name)
	mustCreate(t, ctx, c, csp)
	t.Cleanup(func() { removeFinalizerAndDelete(t, c, name) })

	// Wait for the controller to reconcile the CSP at least once. The
	// finalizer being attached is the cheapest proof of a completed reconcile.
	waitForFinalizer(t, ctx, c, name, 45*time.Second)

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
		// for that plus mysql_keeper_cluster_writable to prove both gauges
		// are wired.
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
