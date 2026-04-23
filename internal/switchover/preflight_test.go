package switchover

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// fakeInspector is a hand-rolled test double for ReplicationInspector. It is
// intentionally minimal: tests configure the fields they care about and leave
// the rest zero.
type fakeInspector struct {
	snapshot         pxc.GTIDSnapshot
	snapshotErr      error
	missingResult    string
	missingErr       error
	waitErr          error
	waitInvoked      bool
	waitArg          string
	repStatus        pxc.ReplicationStatus
	repErr           error
	probeReachable   bool
	executedOverride string
}

func (f *fakeInspector) GetGTIDSnapshot(context.Context) (pxc.GTIDSnapshot, error) {
	return f.snapshot, f.snapshotErr
}
func (f *fakeInspector) GetExecutedGTID(context.Context) (string, error) {
	if f.executedOverride != "" {
		return f.executedOverride, nil
	}
	return f.snapshot.Executed, f.snapshotErr
}
func (f *fakeInspector) IsGTIDSubset(_ context.Context, other string) (bool, error) {
	return f.missingResult == "" && other != "", f.missingErr
}
func (f *fakeInspector) MissingGTIDs(_ context.Context, _ string) (string, error) {
	return f.missingResult, f.missingErr
}
func (f *fakeInspector) WaitForGTID(_ context.Context, gtid string, _ time.Duration) error {
	f.waitInvoked = true
	f.waitArg = gtid
	return f.waitErr
}
func (f *fakeInspector) GetReplicationStatus(context.Context, string) (pxc.ReplicationStatus, error) {
	return f.repStatus, f.repErr
}
func (f *fakeInspector) ProbeReachable(context.Context, time.Duration) (bool, error) {
	return f.probeReachable, nil
}

// fakePXC satisfies the tiny state-change surface; it never talks to SQL.
type fakePXC struct {
	writable    bool
	writableErr error
}

func (f *fakePXC) IsWritable(context.Context) (bool, error) { return f.writable, f.writableErr }
func (f *fakePXC) SetReadOnly(context.Context) error        { return nil }
func (f *fakePXC) SetReadWrite(context.Context) error       { return nil }

// TestPreFlight_GTIDSubsetBlocksWhenRemoteMissing is the regression test for
// the root cause of the production incident: the remote has not yet applied
// every GTID the local source has, so the preflight must refuse the flip.
func TestPreFlight_GTIDSubsetBlocksWhenRemoteMissing(t *testing.T) {
	local := &fakeInspector{
		snapshot: goodSnapshot("dc-uuid:1-1000"),
	}
	remote := &fakeInspector{
		snapshot:      goodSnapshot("dc-uuid:1-990"),
		missingResult: "dc-uuid:991-1000",
		waitErr:       errors.New("replica did not reach target GTID within 5s"),
	}

	p := PreFlight{
		LocalPXC:        &fakePXC{writable: true},
		RemotePXC:       &fakePXC{writable: false},
		LocalInspector:  local,
		RemoteInspector: remote,
		Channel:         "dc-to-dr",
		CatchupTimeout:  5 * time.Second,
	}

	res := p.Run(context.Background())
	if res.OK() {
		t.Fatalf("expected preflight to fail when remote is missing GTIDs; got: %s", res.Summary())
	}
	if !remote.waitInvoked {
		t.Errorf("expected WaitForGTID to be invoked")
	}

	// The check names we specifically care about:
	mustHaveCheckFailed(t, res, "C5_GTIDSubset")
	mustHaveCheckFailed(t, res, "C6_GTIDCatchup")
}

// TestPreFlight_SubsetPassesWhenRemoteCaughtUp confirms that the OK path lets
// the switchover through.
func TestPreFlight_SubsetPassesWhenRemoteCaughtUp(t *testing.T) {
	gtid := "dc-uuid:1-1000"
	local := &fakeInspector{snapshot: goodSnapshot(gtid)}
	remote := &fakeInspector{
		snapshot:      goodSnapshot(gtid),
		missingResult: "", // caught up
		repStatus: pxc.ReplicationStatus{
			ChannelName:     "dc-to-dr",
			ConfigExists:    true,
			IOServiceState:  "ON",
			SQLServiceState: "ON",
		},
	}

	p := PreFlight{
		LocalPXC:                 &fakePXC{writable: true},
		RemotePXC:                &fakePXC{writable: false},
		LocalInspector:           local,
		RemoteInspector:          remote,
		Channel:                  "dc-to-dr",
		MinBinlogRetentionSecond: 7 * 24 * 3600,
	}

	res := p.Run(context.Background())
	if !res.OK() {
		t.Fatalf("expected preflight to pass; got: %s", res.Summary())
	}
}

// TestPreFlight_LogReplicaUpdatesIsHard proves we block the flip when
// log_replica_updates is OFF on the remote — the invariant that prevents the
// "Error 1236 after next flip" chain reaction.
func TestPreFlight_LogReplicaUpdatesIsHard(t *testing.T) {
	gtid := "dc-uuid:1-100"
	local := &fakeInspector{snapshot: goodSnapshot(gtid)}
	snap := goodSnapshot(gtid)
	snap.LogReplicaUpdatesOn = false
	remote := &fakeInspector{
		snapshot: snap,
		repStatus: pxc.ReplicationStatus{
			ChannelName:     "dc-to-dr",
			ConfigExists:    true,
			IOServiceState:  "ON",
			SQLServiceState: "ON",
		},
	}

	p := PreFlight{
		LocalPXC:        &fakePXC{writable: true},
		RemotePXC:       &fakePXC{writable: false},
		LocalInspector:  local,
		RemoteInspector: remote,
		Channel:         "dc-to-dr",
	}

	res := p.Run(context.Background())
	if res.OK() {
		t.Fatalf("expected preflight to fail with log_replica_updates=0; got: %s", res.Summary())
	}
	mustHaveCheckFailed(t, res, "C7_RemoteLogReplicaUpdates")
}

// TestPreFlight_RemoteAlreadyWritableIsSplitBrainGuard covers C1.
func TestPreFlight_RemoteAlreadyWritableIsSplitBrainGuard(t *testing.T) {
	gtid := "dc-uuid:1-100"
	p := PreFlight{
		LocalPXC:        &fakePXC{writable: true},
		RemotePXC:       &fakePXC{writable: true}, // already writable — this is the guard
		LocalInspector:  &fakeInspector{snapshot: goodSnapshot(gtid)},
		RemoteInspector: &fakeInspector{snapshot: goodSnapshot(gtid)},
		Channel:         "dc-to-dr",
	}
	res := p.Run(context.Background())
	if res.OK() {
		t.Fatalf("expected preflight to fail the split-brain guard")
	}
	mustHaveCheckFailed(t, res, "C1_RemoteReachableReadOnly")
}

// TestPreFlight_SoftChecksDoNotBlock confirms C11 (binlog retention) does not
// fail OK() even when tripped.
func TestPreFlight_SoftChecksDoNotBlock(t *testing.T) {
	gtid := "dc-uuid:1-100"
	snap := goodSnapshot(gtid)
	snap.BinlogExpireLogsSeconds = 3600 // 1h — less than the 7d recommendation
	local := &fakeInspector{snapshot: goodSnapshot(gtid)}
	remote := &fakeInspector{
		snapshot: snap,
		repStatus: pxc.ReplicationStatus{
			ChannelName:     "dc-to-dr",
			ConfigExists:    true,
			IOServiceState:  "ON",
			SQLServiceState: "ON",
		},
	}

	p := PreFlight{
		LocalPXC:                 &fakePXC{writable: true},
		RemotePXC:                &fakePXC{writable: false},
		LocalInspector:           local,
		RemoteInspector:          remote,
		Channel:                  "dc-to-dr",
		MinBinlogRetentionSecond: 7 * 24 * 3600,
	}
	res := p.Run(context.Background())
	if !res.OK() {
		t.Fatalf("soft retention check should not block overall OK; got: %s", res.Summary())
	}
	// But the individual check should still be flagged.
	found := false
	for _, c := range res.Checks {
		if c.Name == "C11_RemoteBinlogRetention" && !c.Passed {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected C11_RemoteBinlogRetention to be flagged as failed (soft)")
	}
}

// goodSnapshot returns a minimally-complete snapshot so tests only need to
// override the fields they actually care about.
func goodSnapshot(gtid string) pxc.GTIDSnapshot {
	return pxc.GTIDSnapshot{
		Executed:                gtid,
		Purged:                  "",
		BinlogExpireLogsSeconds: 7 * 24 * 3600,
		LogReplicaUpdatesOn:     true,
		BinlogFormat:            "ROW",
		GTIDMode:                "ON",
	}
}

func mustHaveCheckFailed(t *testing.T, r *PreFlightResult, name string) {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name != name {
			continue
		}
		if !c.Passed {
			return
		}
		t.Fatalf("expected %s to have failed, but it passed: %s", name, c.Message)
	}
	t.Fatalf("expected check %s to appear in result; got checks=%v", name, r.Checks)
}
