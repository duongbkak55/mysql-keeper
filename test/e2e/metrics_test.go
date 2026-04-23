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
func TestE2E_MetricsExposed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	url := metricsURL(t)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Every metric this controller registers uses the "mysql_keeper" prefix.
	// Asserting the prefix guards against someone inadvertently registering
	// the collector without its labels.
	text := string(body)
	if !strings.Contains(text, "mysql_keeper_cluster_healthy") {
		t.Errorf("metrics body missing mysql_keeper_cluster_healthy; head=\n%s",
			head(text, 1024))
	}
	if !strings.Contains(text, "mysql_keeper_switchover_total") {
		t.Errorf("metrics body missing mysql_keeper_switchover_total; head=\n%s",
			head(text, 1024))
	}
}

// head returns the first n bytes of s for log-friendly truncation.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
