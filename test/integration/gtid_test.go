//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

// TestInteg_GTIDSubsetPass is the happy path: DC and DR are in sync, every
// hard preflight check must pass and OK() returns true.
func TestInteg_GTIDSubsetPass(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Let DR catch up to a known DC state.
	target := writeOnDC(ctx, t, 10)
	waitForReplica(ctx, t, target)

	pf := buildPreflight()
	res := pf.Run(ctx)
	if !res.OK() {
		t.Fatalf("expected preflight OK; failures: %s", res.Summary())
	}
}

// TestInteg_GTIDSubsetFail_LagInjected is the direct regression test for the
// production incident root cause. We stop the SQL thread on DR, write on DC,
// and expect preflight to refuse the flip with C5 failing.
func TestInteg_GTIDSubsetFail_LagInjected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()

	// Pause applier — binlog still arrives but does not get applied.
	if _, err := dr.ExecContext(ctx,
		`STOP REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'`); err != nil {
		t.Fatalf("STOP REPLICA SQL_THREAD: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dr.ExecContext(context.Background(),
			`START REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'`)
	})

	// Write 100 sentinels on DC. These will not land on DR while the applier
	// is stopped. GTID_SUBTRACT(dc, dr) will therefore be non-empty.
	_ = writeOnDC(ctx, t, 100)

	pf := buildPreflight()
	pf.CatchupTimeout = 3 * time.Second // short — DR will not catch up
	res := pf.Run(ctx)

	if res.OK() {
		t.Fatal("preflight should refuse when DR is behind — this is the exact production incident root cause")
	}

	var c5Failed, c6Failed bool
	for _, c := range res.Checks {
		if c.Name == "C5_GTIDSubset" && !c.Passed {
			c5Failed = true
		}
		if c.Name == "C6_GTIDCatchup" && !c.Passed {
			c6Failed = true
		}
	}
	if !c5Failed {
		t.Errorf("expected C5_GTIDSubset to fail; summary: %s", res.Summary())
	}
	if !c6Failed {
		t.Errorf("expected C6_GTIDCatchup to fail; summary: %s", res.Summary())
	}
}

// TestInteg_WaitForGTIDTimeout covers C6 specifically: WaitForGTID must time
// out when the replica cannot converge within the budget.
func TestInteg_WaitForGTIDTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()

	if _, err := dr.ExecContext(ctx,
		`STOP REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'`); err != nil {
		t.Fatalf("STOP: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dr.ExecContext(context.Background(),
			`START REPLICA SQL_THREAD FOR CHANNEL 'dc-to-dr'`)
	})
	dcGtid := writeOnDC(ctx, t, 50)

	rm := pxc.NewRemoteManager(drDSN, 5*time.Second)
	err = rm.WaitForGTID(ctx, dcGtid, 2*time.Second)
	if err == nil {
		t.Fatal("expected WaitForGTID to time out")
	}
	if !strings.Contains(err.Error(), "did not reach target GTID") &&
		!strings.Contains(err.Error(), "WAIT_FOR_EXECUTED_GTID_SET") {
		t.Errorf("unexpected error shape: %v", err)
	}
}

// TestInteg_ReplicationChannelBroken asserts C3 fires when the IO thread is
// in error state (e.g. wrong password). We simulate this by stopping IO and
// pointing at a non-existent user via CHANGE REPLICATION SOURCE.
func TestInteg_ReplicationChannelBroken(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()

	if _, err := dr.ExecContext(ctx,
		`STOP REPLICA IO_THREAD FOR CHANNEL 'dc-to-dr'`); err != nil {
		t.Fatalf("STOP IO: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dr.ExecContext(context.Background(),
			`START REPLICA IO_THREAD FOR CHANNEL 'dc-to-dr'`)
	})

	pf := buildPreflight()
	res := pf.Run(ctx)
	if res.OK() {
		t.Fatal("expected C3 to fail when IO thread is stopped")
	}
	var c3Failed bool
	for _, c := range res.Checks {
		if c.Name == "C3_RemoteReplicationRunning" && !c.Passed {
			c3Failed = true
		}
	}
	if !c3Failed {
		t.Errorf("expected C3_RemoteReplicationRunning to fail; summary: %s", res.Summary())
	}
}

// TestInteg_BinlogRetentionShort triggers C11 (soft) by setting
// binlog_expire_logs_seconds=3600 on DR and observing that the check reports
// FAILED but OK() is still true (soft checks do not block).
func TestInteg_BinlogRetentionShort(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Make DR fully in sync first so the hard checks pass.
	target := writeOnDC(ctx, t, 5)
	waitForReplica(ctx, t, target)

	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		t.Fatalf("open DR: %v", err)
	}
	defer dr.Close()
	if _, err := dr.ExecContext(ctx, `SET GLOBAL binlog_expire_logs_seconds = 3600`); err != nil {
		t.Fatalf("SET binlog_expire_logs_seconds: %v", err)
	}
	t.Cleanup(func() {
		_, _ = dr.ExecContext(context.Background(), `SET GLOBAL binlog_expire_logs_seconds = 604800`)
	})

	pf := buildPreflight()
	pf.MinBinlogRetentionSecond = 86400 // 1 day threshold for the check
	res := pf.Run(ctx)

	if !res.OK() {
		t.Fatalf("soft-fail only should not block OK; failures: %s", res.Summary())
	}
	found := false
	for _, c := range res.Checks {
		if c.Name == "C11_RemoteBinlogRetention" && !c.Passed {
			found = true
		}
	}
	if !found {
		t.Errorf("expected C11_RemoteBinlogRetention to be flagged")
	}
}

// --- helpers ---------------------------------------------------------------

// waitForReplica blocks until DR has applied the given GTID set, using the
// same WAIT_FOR_EXECUTED_GTID_SET primitive that the real preflight uses.
func waitForReplica(ctx context.Context, t *testing.T, gtid string) {
	t.Helper()
	rm := pxc.NewRemoteManager(drDSN, 5*time.Second)
	if err := rm.WaitForGTID(ctx, gtid, 15*time.Second); err != nil {
		t.Fatalf("waitForReplica: %v", err)
	}
}

// buildPreflight wires the standard set of managers used across every test
// so each case only has to override the fields it specifically cares about.
func buildPreflight() switchover.PreFlight {
	local := pxc.NewRemoteManager(dcDSN, 5*time.Second)
	remote := pxc.NewRemoteManager(drDSN, 5*time.Second)
	return switchover.PreFlight{
		LocalPXC:                 local,
		RemotePXC:                remote,
		LocalInspector:           local,
		RemoteInspector:          remote,
		Channel:                  channelName,
		CatchupTimeout:           10 * time.Second,
		MinBinlogRetentionSecond: 7 * 24 * 3600,
	}
}
