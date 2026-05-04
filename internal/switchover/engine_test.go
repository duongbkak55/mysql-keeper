package switchover

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// recordingProxy is a ProxySQLManager test double that records calls for
// assertion and can be configured to return errors.
type recordingProxy struct {
	mu                    sync.Mutex
	applyCalls, backCalls int
	blackholeCalls        int
	applyErr              error
	blackholeErr          error
}

func (p *recordingProxy) ApplyFailoverRouting(context.Context, proxysql.RoutingConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applyCalls++
	return p.applyErr
}
func (p *recordingProxy) RollbackRouting(context.Context, proxysql.RoutingConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backCalls++
	return nil
}
func (p *recordingProxy) Blackhole(context.Context, proxysql.BlackholeConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.blackholeCalls++
	return p.blackholeErr
}

func (p *recordingProxy) ApplyCalls() int     { p.mu.Lock(); defer p.mu.Unlock(); return p.applyCalls }
func (p *recordingProxy) BackCalls() int      { p.mu.Lock(); defer p.mu.Unlock(); return p.backCalls }
func (p *recordingProxy) BlackholeCalls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.blackholeCalls }

// recordingReporter captures phase callbacks so tests can assert the exact
// sequence the engine drove.
type recordingReporter struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingReporter) OnPhaseStart(_ context.Context, phase Phase) {
	r.mu.Lock()
	r.events = append(r.events, "start:"+phase.String())
	r.mu.Unlock()
}
func (r *recordingReporter) OnPhaseComplete(_ context.Context, phase Phase) {
	r.mu.Lock()
	r.events = append(r.events, "complete:"+phase.String())
	r.mu.Unlock()
}
func (r *recordingReporter) OnPhaseError(_ context.Context, phase Phase, _ error) {
	r.mu.Lock()
	r.events = append(r.events, "error:"+phase.String())
	r.mu.Unlock()
}

func (r *recordingReporter) Events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// TestEngine_FenceFailsWhenLocalReachable is the key invariant: if the SQL
// fence fails but ProbeReachable says the local cluster answered, we MUST
// abort rather than escalate to blackhole + promote — otherwise the local
// cluster could re-admit writes while the remote is being promoted.
func TestEngine_FenceFailsWhenLocalReachable(t *testing.T) {
	// Build a local PXC that refuses the fence.
	local := &stubPXC{writable: true, setReadOnlyErr: errors.New("fence failed — server is busy")}
	remote := &stubPXC{writable: false}

	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-10"),
		probeReachable: true, // local is reachable!
	}

	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if res.Success {
		t.Fatalf("expected engine to abort fence; it succeeded")
	}
	if res.FailedPhase != PhaseFence {
		t.Errorf("expected FailedPhase=Fence, got %s", res.FailedPhase)
	}
	if proxy.BlackholeCalls() != 0 {
		t.Errorf("expected Blackhole NOT to be called when local is reachable; calls=%d", proxy.BlackholeCalls())
	}
	if !containsEvent(reporter.Events(), "error:Fence") {
		t.Errorf("expected error:Fence event; got %v", reporter.Events())
	}
}

// TestEngine_FenceEscalatesToBlackholeWhenLocalUnreachable is the inverse:
// when the local cluster is truly down we DO want to escalate to blackhole so
// its recovery cannot accept writes after the flip.
func TestEngine_FenceEscalatesToBlackholeWhenLocalUnreachable(t *testing.T) {
	local := &stubPXC{writable: false, setReadOnlyErr: errors.New("dial tcp: connection refused")}
	remote := &stubPXC{writable: false}

	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-10"),
		probeReachable: false, // local is truly down
	}

	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if !res.Success {
		t.Fatalf("expected engine to succeed via blackhole fallback; err=%v failedPhase=%s",
			res.Error, res.FailedPhase)
	}
	if proxy.BlackholeCalls() == 0 {
		t.Errorf("expected Blackhole fence to be invoked")
	}
}

// TestEngine_PreFlightFailShortCircuits verifies that a failed preflight
// never reaches Fence/Promote.
func TestEngine_PreFlightFailShortCircuits(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: true} // already writable — split-brain guard trips C1

	inspector := &fakeInspector{
		snapshot: goodSnapshot("dc:1-10"),
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if res.Success || res.FailedPhase != PhasePreFlight {
		t.Fatalf("expected PreFlight failure; got success=%v failed=%s", res.Success, res.FailedPhase)
	}
	if local.setReadOnlyCalls.Load() != 0 {
		t.Errorf("expected no fence invocation after preflight failed; calls=%d", local.setReadOnlyCalls.Load())
	}
	if proxy.ApplyCalls() != 0 {
		t.Errorf("expected no routing change after preflight failed; calls=%d", proxy.ApplyCalls())
	}
	if res.PreFlight == nil || res.PreFlight.OK() {
		t.Errorf("expected PreFlight result to be present and non-OK")
	}
}

// --- helpers -------------------------------------------------------------

// stubPXC extends fakePXC with call counters so assertions can check what
// happened. SetReadOnly / SetReadWrite flip the `writable` field so the
// final phaseVerify (IsWritable) observes the mutation.
type stubPXC struct {
	writable          bool
	setReadOnlyErr    error
	setReadOnlyCalls  atomic.Int32
	setReadWriteErr   error
	setReadWriteCalls atomic.Int32
}

func (s *stubPXC) IsWritable(context.Context) (bool, error) { return s.writable, nil }
func (s *stubPXC) SetReadOnly(context.Context) error {
	s.setReadOnlyCalls.Add(1)
	if s.setReadOnlyErr == nil {
		s.writable = false
	}
	return s.setReadOnlyErr
}
func (s *stubPXC) SetReadWrite(context.Context) error {
	s.setReadWriteCalls.Add(1)
	if s.setReadWriteErr == nil {
		s.writable = true
	}
	return s.setReadWriteErr
}

func containsEvent(events []string, needle string) bool {
	for _, e := range events {
		if strings.Contains(e, needle) {
			return true
		}
	}
	return false
}

// pxcVerifyFail implements PXCManager: SetReadWrite succeeds but IsWritable
// always returns false — simulates MANO saying COMPLETED while MySQL is still
// catching up on the read_only change.
type pxcVerifyFail struct {
	setReadWriteCalls atomic.Int32
	setReadOnlyCalls  atomic.Int32
}

func (s *pxcVerifyFail) IsWritable(_ context.Context) (bool, error) { return false, nil }
func (s *pxcVerifyFail) SetReadOnly(_ context.Context) error {
	s.setReadOnlyCalls.Add(1)
	return nil
}
func (s *pxcVerifyFail) SetReadWrite(_ context.Context) error {
	s.setReadWriteCalls.Add(1)
	return nil
}

// failingReplication always fails StopReplica — used to simulate the former
// source being down during the ReverseReplica phase.
type failingReplication struct{}

func (f *failingReplication) StopReplica(_ context.Context, _ string) error {
	return errors.New("former source unreachable: connection refused")
}
func (f *failingReplication) ResetReplicaAll(_ context.Context, _ string) error {
	return errors.New("former source unreachable: connection refused")
}

// TestEngine_VerifyFailDoesNotRollback: post-promote verification failing must
// NOT trigger a rollback. The MySQL promote already completed; rolling back
// would create more churn than it fixes. Manual intervention is required.
func TestEngine_VerifyFailDoesNotRollback(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &pxcVerifyFail{} // SetReadWrite OK but IsWritable stays false
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: true,
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:                local,
		RemotePXC:               remote,
		LocalInspector:          inspector,
		RemoteInspector:         inspector,
		LocalProxySQL:           proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:            time.Second,
		Progress:                reporter,
	})

	res := e.Execute(context.Background())
	if res.Success {
		t.Fatal("expected Verify failure")
	}
	if res.FailedPhase != PhaseVerify {
		t.Errorf("expected FailedPhase=Verify, got %s", res.FailedPhase)
	}
	if res.RolledBack {
		t.Error("Verify failure must NOT set RolledBack — no rollback should be attempted")
	}
	// Rollback of Fence would re-enable writes on local; must not happen.
	if local.setReadWriteCalls.Load() != 0 {
		t.Errorf("rollback SetReadWrite on local must not be called; calls=%d", local.setReadWriteCalls.Load())
	}
	// Rollback of Promote would demote remote back to RO; must not happen.
	if remote.setReadOnlyCalls.Load() != 0 {
		t.Errorf("rollback SetReadOnly on remote must not be called; calls=%d", remote.setReadOnlyCalls.Load())
	}
}

// TestEngine_ReverseReplicaFailIsNonFatal: when the former source is still
// down, StopReplica fails during ReverseReplica — the engine must still
// report Success=true and proceed to Verify, not abort.
func TestEngine_ReverseReplicaFailIsNonFatal(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: true,
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:                 local,
		RemotePXC:                remote,
		LocalInspector:           inspector,
		RemoteInspector:          inspector,
		LocalProxySQL:            proxy,
		LocalReplicationChannel:  "dc-to-dr",
		RemoteReplicationChannel: "dc-to-dr",
		LocalReplication:         &failingReplication{},
		ReverseReplicationSource: &ReverseReplicationSource{
			Host: "new-source.example.com",
			Port: 3306,
		},
		FenceTimeout: time.Second,
		Progress:     reporter,
	})

	res := e.Execute(context.Background())
	if !res.Success {
		t.Fatalf("expected Success=true despite ReverseReplica failure; got err=%v phase=%s",
			res.Error, res.FailedPhase)
	}
	// Reporter must have logged the deferral.
	if !containsEvent(reporter.Events(), "error:ReverseReplica") {
		t.Errorf("expected error:ReverseReplica in reporter events; got %v", reporter.Events())
	}
	// Verify must still have run and completed.
	if !containsEvent(reporter.Events(), "complete:Verify") {
		t.Errorf("expected complete:Verify after ReverseReplica error; got %v", reporter.Events())
	}
}

// TestEngine_BlackholeFenceFails_Aborts: when both the SQL fence and the
// ProxySQL blackhole fence fail, the engine must abort at Fence to prevent
// any promote path that could leave the cluster split-brain.
func TestEngine_BlackholeFenceFails_Aborts(t *testing.T) {
	local := &stubPXC{writable: true, setReadOnlyErr: errors.New("connection refused")}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: false, // local truly down
	}
	proxy := &recordingProxy{blackholeErr: errors.New("all ProxySQL instances unreachable")}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:                local,
		RemotePXC:               remote,
		LocalInspector:          inspector,
		RemoteInspector:         inspector,
		LocalProxySQL:           proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:            time.Second,
		Progress:                reporter,
	})

	res := e.Execute(context.Background())
	if res.Success {
		t.Fatal("expected engine to abort when both SQL fence and blackhole fence fail")
	}
	if res.FailedPhase != PhaseFence {
		t.Errorf("expected FailedPhase=Fence, got %s", res.FailedPhase)
	}
	if proxy.BlackholeCalls() == 0 {
		t.Error("expected Blackhole to have been attempted before abort")
	}
	if proxy.ApplyCalls() != 0 {
		t.Errorf("expected no routing change after total fence failure; applyCalls=%d", proxy.ApplyCalls())
	}
}

// TestEngine_FenceNoProxySQL_WhenLocalUnreachable_Aborts: if the SQL fence
// fails and the local cluster is down, but no ProxySQL blackhole is configured,
// the engine must abort — promoting without any write fence risks split-brain
// if local recovers.
func TestEngine_FenceNoProxySQL_WhenLocalUnreachable_Aborts(t *testing.T) {
	local := &stubPXC{writable: true, setReadOnlyErr: errors.New("connection refused")}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: false, // local truly down
	}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:                local,
		RemotePXC:               remote,
		LocalInspector:          inspector,
		RemoteInspector:         inspector,
		LocalProxySQL:           nil, // no blackhole configured
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:            time.Second,
		Progress:                reporter,
	})

	res := e.Execute(context.Background())
	if res.Success {
		t.Fatal("expected engine to abort when local is down and no ProxySQL blackhole configured")
	}
	if res.FailedPhase != PhaseFence {
		t.Errorf("expected FailedPhase=Fence, got %s", res.FailedPhase)
	}
}

// TestEngine_PromoteFailRollbacksFence confirms that a Promote failure
// triggers rollback of the Fence step so the former source becomes writable
// again; otherwise the cluster would be stuck RO after a recoverable hiccup.
func TestEngine_PromoteFailRollbacksFence(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{
		writable:        false,
		setReadWriteErr: errors.New("remote MySQL unreachable"),
	}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: true,
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())

	if res.Success {
		t.Fatal("expected failure at Promote")
	}
	if res.FailedPhase != PhasePromote {
		t.Errorf("expected FailedPhase=Promote, got %s", res.FailedPhase)
	}
	if !res.RolledBack {
		t.Error("expected RolledBack=true")
	}
	if !local.writable {
		t.Error("expected local to be writable again after rollback")
	}
}

// TestEngine_RoutingFailRollsBackPromoteAndFence verifies that when Routing
// fails, the engine rolls back both the Promote (remote back to RO) and the
// Fence (local back to writable) so operators can resolve the routing issue
// without emergency intervention.
func TestEngine_RoutingFailRollsBackPromoteAndFence(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: true,
	}
	proxy := &recordingProxy{applyErr: errors.New("2 of 3 ProxySQL unreachable")}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())

	if res.Success || res.FailedPhase != PhaseRouting {
		t.Fatalf("expected FailedPhase=Routing, got success=%v failed=%s",
			res.Success, res.FailedPhase)
	}
	if !res.RolledBack {
		t.Fatal("expected RolledBack=true")
	}
	if remote.writable {
		t.Error("expected remote to be demoted back to RO after rollback")
	}
	if !local.writable {
		t.Error("expected local to be re-promoted to writable after rollback")
	}
	// Routing's Apply failed before anything was committed, so there is
	// nothing to revert via RollbackRouting. The rollback covers Promote and
	// Fence only — a RollbackRouting call here would be a bug (it would
	// double-apply the original routing on instances that never changed).
	if proxy.BackCalls() != 0 {
		t.Errorf("expected no RollbackRouting call on failed Apply, got %d", proxy.BackCalls())
	}
}

// TestEngine_QuarantineFailsPreFlight verifies the C12 guard: when
// cfg.LocalReplicaQuarantined is true, Execute must return immediately with
// Success=false and FailedPhase=PhasePreFlight — no Fence, Promote, or routing
// changes must occur. A future refactor that removes checkReplicaNotQuarantined
// from PreFlight.Run should cause this test to fail.
func TestEngine_QuarantineFailsPreFlight(t *testing.T) {
	// Remote is read-only and fully caught up so every other preflight check
	// passes — C12 is the only thing that should block the switchover.
	gtid := "dc:1-100"
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot: goodSnapshot(gtid),
		repStatus: pxc.ReplicationStatus{
			ChannelName:     "dc-to-dr",
			ConfigExists:    true,
			IOServiceState:  "ON",
			SQLServiceState: "ON",
		},
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:                local,
		RemotePXC:               remote,
		LocalInspector:          inspector,
		RemoteInspector:         inspector,
		LocalProxySQL:           proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:            time.Second,
		Progress:                reporter,
		LocalReplicaQuarantined: true,
		LocalQuarantineReason:   "auto-skip threshold exceeded (5/3)",
	})

	res := e.Execute(context.Background())

	if res.Success {
		t.Fatal("expected Execute to fail when replica is quarantined")
	}
	if res.FailedPhase != PhasePreFlight {
		t.Errorf("expected FailedPhase=PreFlight, got %s", res.FailedPhase)
	}
	// C12 must be present, failed, and mention quarantine.
	if res.PreFlight == nil {
		t.Fatal("expected res.PreFlight to be populated")
	}
	foundC12 := false
	for _, c := range res.PreFlight.Checks {
		if c.Name != "C12_ReplicaNotQuarantined" {
			continue
		}
		foundC12 = true
		if c.Passed {
			t.Errorf("expected C12_ReplicaNotQuarantined to be failed, but it passed")
		}
		if !strings.Contains(strings.ToLower(c.Message), "quarantine") {
			t.Errorf("expected C12 failure message to mention quarantine; got: %q", c.Message)
		}
	}
	if !foundC12 {
		t.Error("expected C12_ReplicaNotQuarantined check to appear in PreFlightResult")
	}
	// No state mutations must have occurred.
	if local.setReadOnlyCalls.Load() != 0 {
		t.Errorf("Fence must not be reached; SetReadOnly called %d time(s)", local.setReadOnlyCalls.Load())
	}
	if remote.setReadWriteCalls.Load() != 0 {
		t.Errorf("Promote must not be reached; SetReadWrite called %d time(s)", remote.setReadWriteCalls.Load())
	}
	if proxy.ApplyCalls() != 0 {
		t.Errorf("Routing must not be reached; ApplyCalls=%d", proxy.ApplyCalls())
	}
}

// TestEngine_GTIDCatchupTimeoutAborts exercises C6: when the replica cannot
// catch up within CatchupTimeout, the flip must be aborted in PreFlight and
// no Fence / Promote should run.
func TestEngine_GTIDCatchupTimeoutAborts(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: false}
	inspectorLocal := &fakeInspector{snapshot: goodSnapshot("dc:1-1000")}
	inspectorRemote := &fakeInspector{
		snapshot:      goodSnapshot("dc:1-900"),
		missingResult: "dc:901-1000",
		waitErr:       errors.New("WAIT_FOR_EXECUTED_GTID_SET timed out"),
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspectorLocal,
		RemoteInspector:    inspectorRemote,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		CatchupTimeout:     500 * time.Millisecond,
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if res.Success || res.FailedPhase != PhasePreFlight {
		t.Fatalf("expected PreFlight abort due to C6 timeout; got success=%v phase=%s",
			res.Success, res.FailedPhase)
	}
	if local.setReadOnlyCalls.Load() != 0 || remote.setReadWriteCalls.Load() != 0 {
		t.Errorf("expected no state changes after PreFlight abort; fence=%d promote=%d",
			local.setReadOnlyCalls.Load(), remote.setReadWriteCalls.Load())
	}
	if !inspectorRemote.waitInvoked {
		t.Error("expected WaitForGTID to have been invoked")
	}
}

// TestEngine_ProgressReporterSeesAllPhases locks in the contract the
// controller checkpoint code relies on: every phase must report start+
// complete (or start+error) in the right order.
func TestEngine_ProgressReporterSeesAllPhases(t *testing.T) {
	local := &stubPXC{writable: true}
	remote := &stubPXC{writable: false}
	inspector := &fakeInspector{
		snapshot:       goodSnapshot("dc:1-100"),
		probeReachable: true,
	}
	proxy := &recordingProxy{}
	reporter := &recordingReporter{}

	e := NewEngine(Config{
		LocalPXC:           local,
		RemotePXC:          remote,
		LocalInspector:     inspector,
		RemoteInspector:    inspector,
		LocalProxySQL:      proxy,
		LocalReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if !res.Success {
		t.Fatalf("expected happy path; got: %v", res.Error)
	}

	events := reporter.Events()
	wantPhases := []string{"PreFlight", "Fence", "Promote", "Routing", "ReverseReplica", "Verify"}
	for _, phase := range wantPhases {
		if !containsEvent(events, "start:"+phase) {
			t.Errorf("missing start:%s in reporter events: %v", phase, events)
		}
	}
	// Every successful phase must have a paired complete event (ReverseReplica
	// is best-effort so it may report error without aborting, but Verify must
	// always complete on the happy path).
	if !containsEvent(events, "complete:Verify") {
		t.Errorf("missing complete:Verify in reporter events: %v", events)
	}
}
