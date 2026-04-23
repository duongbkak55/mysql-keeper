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

		if resp.StatusCode == http.StatusOK &&
			strings.Contains(lastBody, "mysql_keeper_switchover_total") {
			// Every metric this controller registers uses the "mysql_keeper"
			// prefix. Check for cluster_healthy too to guard against someone
			// inadvertently registering a collector without its labels.
			if strings.Contains(lastBody, "mysql_keeper_cluster_healthy") {
				return
			}
			t.Errorf("metrics body missing mysql_keeper_cluster_healthy; head=\n%s",
				head(lastBody, 1024))
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
