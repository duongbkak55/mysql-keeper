package switchover

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
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
		ReplicationChannel: "dc-to-dr",
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
	if proxy.blackholeCalls != 0 {
		t.Errorf("expected Blackhole NOT to be called when local is reachable; calls=%d", proxy.blackholeCalls)
	}
	if !containsEvent(reporter.events, "error:Fence") {
		t.Errorf("expected error:Fence event; got %v", reporter.events)
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
		ReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if !res.Success {
		t.Fatalf("expected engine to succeed via blackhole fallback; err=%v failedPhase=%s",
			res.Error, res.FailedPhase)
	}
	if proxy.blackholeCalls == 0 {
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
		ReplicationChannel: "dc-to-dr",
		FenceTimeout:       time.Second,
		Progress:           reporter,
	})

	res := e.Execute(context.Background())
	if res.Success || res.FailedPhase != PhasePreFlight {
		t.Fatalf("expected PreFlight failure; got success=%v failed=%s", res.Success, res.FailedPhase)
	}
	if local.setReadOnlyCalls != 0 {
		t.Errorf("expected no fence invocation after preflight failed; calls=%d", local.setReadOnlyCalls)
	}
	if proxy.applyCalls != 0 {
		t.Errorf("expected no routing change after preflight failed; calls=%d", proxy.applyCalls)
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
	setReadOnlyCalls  int
	setReadWriteErr   error
	setReadWriteCalls int
}

func (s *stubPXC) IsWritable(context.Context) (bool, error) { return s.writable, nil }
func (s *stubPXC) SetReadOnly(context.Context) error {
	s.setReadOnlyCalls++
	if s.setReadOnlyErr == nil {
		s.writable = false
	}
	return s.setReadOnlyErr
}
func (s *stubPXC) SetReadWrite(context.Context) error {
	s.setReadWriteCalls++
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
