//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// TestInteg_DetectWorkerErrors_NoErrorsOnHealthyReplica covers the baseline
// case: with replication running cleanly, DetectWorkerErrors returns an
// empty slice — neither the worker view nor the coordinator fallback should
// surface a row.
func TestInteg_DetectWorkerErrors_NoErrorsOnHealthyReplica(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Quiesce replication state from any prior test that may have stopped a
	// thread, then make sure DR is in a known caught-up position.
	target := writeOnDC(ctx, t, 1)
	waitForReplica(ctx, t, target)

	rm := pxc.NewRemoteManager(drDSN, 5*time.Second)
	errs, err := rm.DetectWorkerErrors(ctx, channelName)
	if err != nil {
		t.Fatalf("DetectWorkerErrors: %v", err)
	}
	if len(errs) != 0 {
		t.Errorf("expected zero worker errors on a healthy replica, got %+v", errs)
	}
}

// TestInteg_SkipNextTransaction_RejectsInvalidGTID covers input validation:
// SkipNextTransaction must refuse a literal that contains characters outside
// the [0-9a-fA-F:-] allowlist before issuing any SQL. We rely on this guard
// to keep the embedded literal injection-safe, so a regression in the
// validator would defeat that defence.
func TestInteg_SkipNextTransaction_RejectsInvalidGTID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rm := pxc.NewRemoteManager(drDSN, 5*time.Second)
	cases := []string{
		"abc:1' OR '1'='1",
		"abc:1; DROP TABLE foo",
		"",
	}
	for _, gtid := range cases {
		err := rm.SkipNextTransaction(ctx, channelName, gtid)
		if err == nil {
			t.Errorf("expected SkipNextTransaction to reject %q, got nil error", gtid)
		}
	}
}

// TestInteg_SkipNextTransaction_HappyPath_1062 is the end-to-end regression
// test: inject a duplicate-key conflict on DR's replica channel, verify the
// applier stops with errno 1062, call SkipNextTransaction with the failed
// GTID, then verify replication resumes and a fresh write from DC lands on
// DR.
//
// This is the path operators rely on: a transient row-level divergence
// causes replication to halt, the controller skips it via empty-transaction
// injection, and replication resumes without manual intervention.
func TestInteg_SkipNextTransaction_HappyPath_1062(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Unique table per run avoids collisions with prior tests that may have
	// left rows in smoketest.pings. NOT replicated yet — we will write on DC
	// then DR poisons the same row before replication catches up.
	tableSuffix := fmt.Sprintf("skip_%d", time.Now().UnixNano())
	createTableStmt := fmt.Sprintf(`
		CREATE TABLE smoketest.%s (
			id BIGINT PRIMARY KEY,
			origin VARCHAR(8) NOT NULL
		)`, tableSuffix)

	dc, err := sql.Open("mysql", dcDSN)
	if err != nil {
		t.Fatalf("open DC: %v", err)
	}
	defer dc.Close()
	if _, err := dc.ExecContext(ctx, createTableStmt); err != nil {
		t.Fatalf("create table on DC: %v", err)
	}

	// Wait for the CREATE TABLE to land on DR via replication.
	creationTarget := dcGTID(ctx, t, dc)
	waitForReplica(ctx, t, creationTarget)
	t.Cleanup(func() {
		dropCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_, _ = dc.ExecContext(dropCtx, fmt.Sprintf("DROP TABLE IF EXISTS smoketest.%s", tableSuffix))
	})

	// Stop the SQL applier on DR so we can preload a row that DC will then
	// also insert. With the applier paused, DC's INSERT lands in DR's relay
	// log but is not yet applied.
	pauseReplicaSQL(ctx, t)
	t.Cleanup(func() { restartReplicaThread(t, "SQL_THREAD") })

	// Pre-seed conflict row on DR (origin=dr) bypassing replication.
	withDRWritable(ctx, t, func(dr *sql.DB) {
		if _, err := dr.ExecContext(ctx,
			fmt.Sprintf("INSERT INTO smoketest.%s (id, origin) VALUES (1, 'dr')",
				tableSuffix)); err != nil {
			t.Fatalf("seed DR row: %v", err)
		}
	})

	// Now write the conflicting row on DC.
	if _, err := dc.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO smoketest.%s (id, origin) VALUES (1, 'dc')",
			tableSuffix)); err != nil {
		t.Fatalf("insert on DC: %v", err)
	}

	// Restart the SQL applier and wait for the duplicate-key error to surface.
	restartReplicaThread(t, "SQL_THREAD")
	rm := pxc.NewRemoteManager(drDSN, 5*time.Second)
	failed := waitForWorkerError(ctx, t, rm, 1062, 30*time.Second)
	if failed.FailedGTID == "" {
		t.Fatalf("worker reported errno=1062 but FailedGTID was empty: %+v", failed)
	}
	t.Logf("observed failed worker: workerID=%d errno=%d gtid=%s msg=%s",
		failed.WorkerID, failed.Errno, failed.FailedGTID,
		truncateForLog(failed.Message, 120))

	// Skip the failing transaction.
	if err := rm.SkipNextTransaction(ctx, channelName, failed.FailedGTID); err != nil {
		t.Fatalf("SkipNextTransaction: %v", err)
	}

	// After the skip, replication must resume Running with no last error.
	if err := waitReplicationHealthy(ctx, rm, 30*time.Second); err != nil {
		t.Fatalf("replication did not recover: %v", err)
	}

	// And a subsequent DC write must land on DR — proving the channel is
	// genuinely caught up rather than just having the error code cleared.
	if _, err := dc.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO smoketest.%s (id, origin) VALUES (2, 'dc')",
			tableSuffix)); err != nil {
		t.Fatalf("post-skip insert on DC: %v", err)
	}
	postTarget := dcGTID(ctx, t, dc)
	waitForReplica(ctx, t, postTarget)

	// Sanity: DR's row 1 must still be the local 'dr' value (the skip kept
	// the pre-seeded row, did not apply DC's conflicting INSERT).
	withDRWritable(ctx, t, func(dr *sql.DB) {
		var origin string
		if err := dr.QueryRowContext(ctx,
			fmt.Sprintf("SELECT origin FROM smoketest.%s WHERE id = 1",
				tableSuffix)).Scan(&origin); err != nil {
			t.Fatalf("read DR row 1: %v", err)
		}
		if origin != "dr" {
			t.Errorf("expected DR row 1 origin=dr (skip preserves local), got %q", origin)
		}
		var origin2 string
		if err := dr.QueryRowContext(ctx,
			fmt.Sprintf("SELECT origin FROM smoketest.%s WHERE id = 2",
				tableSuffix)).Scan(&origin2); err != nil {
			t.Fatalf("read DR row 2: %v", err)
		}
		if origin2 != "dc" {
			t.Errorf("expected DR row 2 origin=dc (replicated post-skip), got %q", origin2)
		}
	})

	// And a final DetectWorkerErrors must show no error rows now that the
	// SQL thread is happy again.
	finalErrs, err := rm.DetectWorkerErrors(ctx, channelName)
	if err != nil {
		t.Fatalf("DetectWorkerErrors after skip: %v", err)
	}
	if len(finalErrs) != 0 {
		t.Errorf("expected no worker errors after recovery, got %+v", finalErrs)
	}
}

// --- helpers -------------------------------------------------------------

// dcGTID reads @@global.gtid_executed on DC for use as a replication target.
func dcGTID(ctx context.Context, t *testing.T, dc *sql.DB) string {
	t.Helper()
	var gtid string
	if err := dc.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_executed`).Scan(&gtid); err != nil {
		t.Fatalf("read DC gtid: %v", err)
	}
	return gtid
}

// pauseReplicaSQL stops the SQL applier on DR. Used to stage a conflict row
// before letting the applier run into the duplicate.
func pauseReplicaSQL(ctx context.Context, t *testing.T) {
	t.Helper()
	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()
	if _, err := dr.ExecContext(ctx,
		fmt.Sprintf("STOP REPLICA SQL_THREAD FOR CHANNEL '%s'", channelName)); err != nil {
		t.Fatalf("STOP REPLICA SQL_THREAD: %v", err)
	}
}

// withDRWritable temporarily flips DR out of read_only / super_read_only,
// runs fn, then restores the read-only state. Required because DR is
// configured as a strict replica during TestMain.
func withDRWritable(ctx context.Context, t *testing.T, fn func(*sql.DB)) {
	t.Helper()
	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()
	for _, s := range []string{
		"SET GLOBAL super_read_only=OFF",
		"SET GLOBAL read_only=OFF",
	} {
		if _, err := dr.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	defer func() {
		for _, s := range []string{
			"SET GLOBAL super_read_only=ON",
			"SET GLOBAL read_only=ON",
		} {
			if _, err := dr.ExecContext(context.Background(), s); err != nil {
				t.Logf("restore read-only %q: %v", s, err)
			}
		}
	}()
	fn(dr)
}

// waitForWorkerError polls DetectWorkerErrors until a worker reports the
// given errno or the budget expires. Returns the matching WorkerError so
// callers can pass FailedGTID into SkipNextTransaction.
func waitForWorkerError(
	ctx context.Context,
	t *testing.T,
	rm *pxc.Manager,
	wantErrno int32,
	budget time.Duration,
) pxc.WorkerError {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		errs, err := rm.DetectWorkerErrors(ctx, channelName)
		if err != nil {
			t.Fatalf("DetectWorkerErrors: %v", err)
		}
		for _, e := range errs {
			if e.Errno == wantErrno {
				return e
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("worker did not report errno=%d within %s", wantErrno, budget)
	return pxc.WorkerError{} // unreachable
}

// waitReplicationHealthy polls until both threads are Running and no last
// error is reported. Returns nil on success.
func waitReplicationHealthy(ctx context.Context, rm *pxc.Manager, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		status, err := rm.GetReplicationStatus(ctx, channelName)
		if err == nil && status.Running() {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	status, _ := rm.GetReplicationStatus(ctx, channelName)
	return fmt.Errorf("not healthy within %s: %s", budget, status.HumanMessage())
}

// truncateForLog clips long error messages so t.Logf output stays readable.
func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…"
}
