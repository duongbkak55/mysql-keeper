// Package switchover orchestrates the DC-DR MySQL cluster switchover operation.
//
// The engine runs the switchover as a sequence of phases. Every phase is
// idempotent and reports progress via the ProgressReporter so the controller
// can persist the checkpoint on the CR status subresource and resume after a
// pod restart.
//
// Phase ordering (happy path):
//
//	PreFlight → Fence → Promote → Routing → ReverseReplica → Verify → Done
//
// Preflight performs the C1–C17 checks from the production-readiness review.
// Fence now fails hard unless the local cluster is confirmed unreachable, in
// which case alternate fencing paths are tried. Promote stops replication on
// the new source and resets its relay metadata. ReverseReplica configures the
// former source to receive from the new one when it returns.
package switchover

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// PXCManager is the minimal state-change surface the engine needs. It is kept
// here (and not in the pxc package) so we can mock it in tests.
type PXCManager interface {
	IsWritable(ctx context.Context) (bool, error)
	SetReadOnly(ctx context.Context) error
	SetReadWrite(ctx context.Context) error
}

// ReplicationInspector exposes the read-only queries used by preflight and
// reverse-replication setup. Implemented by *pxc.Manager; mano.PXCManager
// delegates these to its underlying direct MySQL connection.
type ReplicationInspector interface {
	GetGTIDSnapshot(ctx context.Context) (pxc.GTIDSnapshot, error)
	GetExecutedGTID(ctx context.Context) (string, error)
	IsGTIDSubset(ctx context.Context, other string) (bool, error)
	MissingGTIDs(ctx context.Context, other string) (string, error)
	WaitForGTID(ctx context.Context, gtid string, timeout time.Duration) error
	GetReplicationStatus(ctx context.Context, channel string) (pxc.ReplicationStatus, error)
	ProbeReachable(ctx context.Context, budget time.Duration) (bool, error)
}

// ReplicationController is the mutation surface used during Promote and
// ReverseReplica. Implemented by *pxc.Manager.
type ReplicationController interface {
	StopReplica(ctx context.Context, channel string) error
	ResetReplicaAll(ctx context.Context, channel string) error
}

// ProxySQLManager updates routing plus the blackhole fence used when the
// regular MySQL fence is not sufficient.
type ProxySQLManager interface {
	ApplyFailoverRouting(ctx context.Context, cfg proxysql.RoutingConfig) error
	RollbackRouting(ctx context.Context, cfg proxysql.RoutingConfig) error
	Blackhole(ctx context.Context, cfg proxysql.BlackholeConfig) error
}

// Phase is a single checkpointable step in Execute.
type Phase int

const (
	PhasePreFlight Phase = iota
	PhaseFence
	PhasePromote
	PhaseRouting
	PhaseReverseReplica
	PhaseVerify
	PhaseDone
)

func (p Phase) String() string {
	switch p {
	case PhasePreFlight:
		return "PreFlight"
	case PhaseFence:
		return "Fence"
	case PhasePromote:
		return "Promote"
	case PhaseRouting:
		return "Routing"
	case PhaseReverseReplica:
		return "ReverseReplica"
	case PhaseVerify:
		return "Verify"
	case PhaseDone:
		return "Done"
	default:
		return fmt.Sprintf("Unknown(%d)", int(p))
	}
}

// ProgressReporter lets the controller record each checkpoint on the CR status
// subresource so that a pod restart in the middle of Execute can be resumed.
// All callbacks are best-effort: a failure here does not abort the switchover,
// but the caller should surface it in logs.
type ProgressReporter interface {
	OnPhaseStart(ctx context.Context, phase Phase)
	OnPhaseComplete(ctx context.Context, phase Phase)
	OnPhaseError(ctx context.Context, phase Phase, err error)
}

// noopReporter is used when the caller does not care about checkpoints.
type noopReporter struct{}

func (noopReporter) OnPhaseStart(context.Context, Phase)           {}
func (noopReporter) OnPhaseComplete(context.Context, Phase)        {}
func (noopReporter) OnPhaseError(context.Context, Phase, error)    {}

// Config carries the state managers and tuning knobs. A zero value for any
// duration field falls back to a safe default at execution time.
type Config struct {
	// LocalPXC / RemotePXC are the state-change surfaces. In the MANO-backed
	// path both are *mano.PXCManager; in the direct path both are *pxc.Manager.
	LocalPXC  PXCManager
	RemotePXC PXCManager

	// LocalInspector / RemoteInspector are the read-only introspection surface
	// used by preflight. May be nil if the caller explicitly disables those
	// checks (strongly discouraged — only acceptable for the direct-data-loss
	// failover path when the local cluster is destroyed).
	LocalInspector  ReplicationInspector
	RemoteInspector ReplicationInspector

	// RemoteReplication mutates replication state on the new source.
	RemoteReplication ReplicationController

	// LocalReplication mutates replication state on the former source when we
	// want to set up the reverse channel immediately.
	LocalReplication ReplicationController

	// LocalProxySQL holds both the normal routing surface and the blackhole
	// fence escalation used when the SQL fence fails.
	LocalProxySQL ProxySQLManager

	// Routing describes the normal (writer flip) ProxySQL change. Reused for
	// rollback by swapping old/new.
	Routing proxysql.RoutingConfig

	// BlackholeFence is the escalation used when the SQL fence cannot complete
	// but the local cluster is still answering ProxySQL. If the local node is
	// fully unreachable, this moves writes away from it unconditionally.
	BlackholeFence proxysql.BlackholeConfig

	// LocalReplicationChannel is the channel name on the LOCAL cluster when
	// local is acting as a replica (after a flip, when former-source
	// becomes replica of the new source). Used by phaseReverseReplica when
	// stopping any stale inbound channel on the former source.
	// Required.
	LocalReplicationChannel string

	// RemoteReplicationChannel is the channel name on the REMOTE cluster
	// when remote is acting as a replica — which is the pre-flip baseline.
	// Used by preflight C3 (health check against remote's replication
	// threads) and by phasePromote (STOP+RESET on remote once it becomes
	// the new source).
	// If both directions use the same channel name, set this to the same
	// value as LocalReplicationChannel.
	// Required.
	RemoteReplicationChannel string

	// ReverseReplicationSource tells the former source how to connect back to
	// the new source for replication. Optional — when unset, reverse-replica
	// phase is skipped and is expected to be set up by a subsequent reconcile.
	ReverseReplicationSource *ReverseReplicationSource

	// MinBinlogRetentionSeconds is the minimum binlog_expire_logs_seconds we
	// accept on the new source before allowing a promote. Recommended: 7*24h.
	MinBinlogRetentionSeconds int64

	// CatchupTimeout is how long preflight will block on WAIT_FOR_EXECUTED_GTID_SET
	// before declaring the replica not caught up. Default 30s.
	CatchupTimeout time.Duration

	// DrainTimeout is a short sleep between PreFlight and Fence so that idle
	// connections can close cleanly.
	DrainTimeout time.Duration

	// FenceTimeout is the deadline for the SQL fence itself. If it fires, the
	// engine falls back to the blackhole path unless the local cluster is
	// explicitly confirmed unreachable — in which case we abort to avoid
	// split-brain.
	FenceTimeout time.Duration

	// AllowDataLossFailover must be explicitly set when the local cluster is
	// destroyed and we accept that some of its recent writes may be lost. When
	// false, a preflight without a readable local snapshot fails the switchover.
	AllowDataLossFailover bool

	// Reason is a human-readable description surfaced in logs and events.
	Reason string

	// AttemptID correlates log lines, metric series, and CR status for a
	// single switchover attempt.
	AttemptID string

	// Progress receives per-phase callbacks. When nil a no-op reporter is used.
	Progress ProgressReporter
}

// ReverseReplicationSource describes how the former source should connect
// back to the new source after a flip. The user/password are separate from the
// admin credentials used elsewhere and typically correspond to the replication
// grant.
type ReverseReplicationSource struct {
	Host     string
	Port     int32
	User     string
	Password string
	SSL      bool
}

// Result summarises a completed (or failed) Execute call.
type Result struct {
	Success     bool
	FailedPhase Phase
	RolledBack  bool
	Error       error

	// PreFlight holds the snapshot collected during PhasePreFlight, so the
	// controller can surface per-check status on the CR without re-running
	// the queries.
	PreFlight *PreFlightResult
}

// Engine executes and (if needed) rolls back a switchover.
type Engine struct {
	cfg Config
}

// NewEngine creates a switchover Engine.
func NewEngine(cfg Config) *Engine {
	return &Engine{cfg: cfg}
}

// Execute runs the switchover end-to-end. Intermediate failures roll back the
// phases that were already applied, in reverse order.
func (e *Engine) Execute(ctx context.Context) Result {
	logger := log.FromContext(ctx).WithValues(
		"event", "switchover",
		"reason", e.cfg.Reason,
		"attemptID", e.cfg.AttemptID,
		"localChannel", e.cfg.LocalReplicationChannel,
		"remoteChannel", e.cfg.RemoteReplicationChannel,
	)
	start := time.Now()
	logger.Info("switchover started")
	defer func() {
		logger.Info("switchover finished", "elapsed_ms", time.Since(start).Milliseconds())
	}()

	reporter := e.cfg.Progress
	if reporter == nil {
		reporter = noopReporter{}
	}

	completed := make([]Phase, 0, 6)

	// ---------------- PreFlight ----------------
	reporter.OnPhaseStart(ctx, PhasePreFlight)
	pre, err := e.phasePreFlight(ctx)
	if err != nil {
		reporter.OnPhaseError(ctx, PhasePreFlight, err)
		logger.Error(err, "PreFlight failed — aborting")
		return Result{Success: false, FailedPhase: PhasePreFlight, Error: err, PreFlight: pre}
	}
	reporter.OnPhaseComplete(ctx, PhasePreFlight)
	completed = append(completed, PhasePreFlight)

	// ---------------- Fence ----------------
	if e.cfg.DrainTimeout > 0 {
		select {
		case <-time.After(e.cfg.DrainTimeout):
		case <-ctx.Done():
			return Result{Success: false, FailedPhase: PhaseFence, Error: ctx.Err(), PreFlight: pre}
		}
	}
	reporter.OnPhaseStart(ctx, PhaseFence)
	if err := e.phaseFence(ctx); err != nil {
		reporter.OnPhaseError(ctx, PhaseFence, err)
		logger.Error(err, "Fence failed — aborting to prevent split-brain")
		e.rollback(ctx, completed)
		return Result{Success: false, FailedPhase: PhaseFence, RolledBack: true, Error: err, PreFlight: pre}
	}
	reporter.OnPhaseComplete(ctx, PhaseFence)
	completed = append(completed, PhaseFence)

	// ---------------- Promote ----------------
	reporter.OnPhaseStart(ctx, PhasePromote)
	if err := e.phasePromote(ctx); err != nil {
		reporter.OnPhaseError(ctx, PhasePromote, err)
		logger.Error(err, "Promote failed — rolling back")
		e.rollback(ctx, completed)
		return Result{Success: false, FailedPhase: PhasePromote, RolledBack: true, Error: err, PreFlight: pre}
	}
	reporter.OnPhaseComplete(ctx, PhasePromote)
	completed = append(completed, PhasePromote)

	// ---------------- Routing ----------------
	reporter.OnPhaseStart(ctx, PhaseRouting)
	if err := e.phaseRouting(ctx); err != nil {
		reporter.OnPhaseError(ctx, PhaseRouting, err)
		logger.Error(err, "Routing failed — rolling back")
		e.rollback(ctx, completed)
		return Result{Success: false, FailedPhase: PhaseRouting, RolledBack: true, Error: err, PreFlight: pre}
	}
	reporter.OnPhaseComplete(ctx, PhaseRouting)
	completed = append(completed, PhaseRouting)

	// ---------------- ReverseReplica ----------------
	// Best-effort: if the former source is unreachable (which is why we failed
	// over), we cannot configure it right now. A subsequent reconcile will try
	// again once it comes back up. The failure is surfaced in the CR status.
	reporter.OnPhaseStart(ctx, PhaseReverseReplica)
	if err := e.phaseReverseReplica(ctx); err != nil {
		reporter.OnPhaseError(ctx, PhaseReverseReplica, err)
		logger.Info("ReverseReplica setup deferred — former source unreachable. Will retry on next reconcile.",
			"error", err.Error())
	} else {
		reporter.OnPhaseComplete(ctx, PhaseReverseReplica)
	}
	completed = append(completed, PhaseReverseReplica)

	// ---------------- Verify ----------------
	reporter.OnPhaseStart(ctx, PhaseVerify)
	if err := e.phaseVerify(ctx); err != nil {
		reporter.OnPhaseError(ctx, PhaseVerify, err)
		logger.Error(err, "Post-promote verification failed — investigate")
		// Do not rollback: the promote already succeeded at the MySQL level.
		// A rollback here would create more churn than it fixes. Surface the
		// error so the operator can intervene.
		return Result{Success: false, FailedPhase: PhaseVerify, Error: err, PreFlight: pre}
	}
	reporter.OnPhaseComplete(ctx, PhaseVerify)

	logger.Info("Switchover completed successfully")
	return Result{Success: true, FailedPhase: PhaseDone, PreFlight: pre}
}

// phasePreFlight runs the C1–C17 checks. The returned PreFlightResult is
// included in the engine Result even on failure so the controller can show
// exactly which check tripped.
func (e *Engine) phasePreFlight(ctx context.Context) (*PreFlightResult, error) {
	p := PreFlight{
		LocalPXC:                 e.cfg.LocalPXC,
		RemotePXC:                e.cfg.RemotePXC,
		LocalInspector:           e.cfg.LocalInspector,
		RemoteInspector:          e.cfg.RemoteInspector,
		Channel:                  e.cfg.RemoteReplicationChannel,
		CatchupTimeout:           e.cfg.CatchupTimeout,
		MinBinlogRetentionSecond: e.cfg.MinBinlogRetentionSeconds,
		AllowDataLossFailover:    e.cfg.AllowDataLossFailover,
	}
	res := p.Run(ctx)
	if !res.OK() {
		return res, fmt.Errorf("preflight failed: %s", res.Summary())
	}
	return res, nil
}

// phaseFence makes writes to the local cluster impossible. The order of escalation:
//
//  1. SQL fence via LocalPXC.SetReadOnly.
//  2. If that failed, probe reachability. If the local cluster is reachable,
//     we refuse to promote (split-brain risk).
//  3. If the local cluster is unreachable, try the ProxySQL blackhole fence
//     on every local ProxySQL instance so even a later recovery cannot route
//     writes back to the old source before we reconcile.
func (e *Engine) phaseFence(ctx context.Context) error {
	logger := log.FromContext(ctx)
	fenceCtx, cancel := context.WithTimeout(ctx, e.cfg.FenceTimeout)
	defer cancel()

	sqlErr := e.cfg.LocalPXC.SetReadOnly(fenceCtx)
	if sqlErr == nil {
		logger.Info("SQL fence applied successfully")
		return nil
	}
	logger.Error(sqlErr, "SQL fence failed — checking whether local cluster is reachable")

	reachable := false
	if e.cfg.LocalInspector != nil {
		ok, _ := e.cfg.LocalInspector.ProbeReachable(ctx, 2*time.Second)
		reachable = ok
	}

	if reachable {
		return fmt.Errorf(
			"local cluster is reachable but SQL fence failed — refusing to promote: %w",
			sqlErr,
		)
	}

	// Local truly down. Escalate to the blackhole fence so traffic cannot
	// reach the old writer after it recovers.
	if e.cfg.LocalProxySQL == nil {
		return fmt.Errorf(
			"local unreachable and no ProxySQL blackhole configured: %w", sqlErr,
		)
	}
	if err := e.cfg.LocalProxySQL.Blackhole(ctx, e.cfg.BlackholeFence); err != nil {
		return fmt.Errorf(
			"local unreachable and ProxySQL blackhole fence failed: sqlErr=%v blackholeErr=%w",
			sqlErr, err,
		)
	}
	logger.Info("Local cluster unreachable — fell back to ProxySQL blackhole fence")
	return nil
}

// phasePromote writes-enables the new source, stops replication on it so it
// no longer follows the old source, and clears the relay metadata.
func (e *Engine) phasePromote(ctx context.Context) error {
	logger := log.FromContext(ctx)
	if err := e.cfg.RemotePXC.SetReadWrite(ctx); err != nil {
		return fmt.Errorf("set remote read_only=OFF: %w", err)
	}

	// Remote was the replica before the flip. Its inbound channel name is
	// RemoteReplicationChannel. Stop + reset so the new source does not
	// fight its own former upstream.
	if e.cfg.RemoteReplication != nil && e.cfg.RemoteReplicationChannel != "" {
		if err := e.cfg.RemoteReplication.StopReplica(ctx, e.cfg.RemoteReplicationChannel); err != nil {
			logger.Error(err, "STOP REPLICA on new source failed — investigate",
				"channel", e.cfg.RemoteReplicationChannel)
		}
		if err := e.cfg.RemoteReplication.ResetReplicaAll(ctx, e.cfg.RemoteReplicationChannel); err != nil {
			logger.Error(err, "RESET REPLICA ALL on new source failed — investigate",
				"channel", e.cfg.RemoteReplicationChannel)
		}
	}
	return nil
}

// phaseRouting updates local ProxySQL to point writes to the new source.
func (e *Engine) phaseRouting(ctx context.Context) error {
	return e.cfg.LocalProxySQL.ApplyFailoverRouting(ctx, e.cfg.Routing)
}

// phaseReverseReplica best-effort configures the former source so it will
// resume following the new source once it comes back.
func (e *Engine) phaseReverseReplica(ctx context.Context) error {
	src := e.cfg.ReverseReplicationSource
	if src == nil {
		return errors.New("no reverse replication source configured (will be set up on next reconcile)")
	}
	if e.cfg.LocalReplication == nil {
		return errors.New("no local replication controller available")
	}
	// We cannot execute CHANGE REPLICATION SOURCE via the direct PXC manager
	// yet because the Manager would need new methods that accept credentials;
	// for now we stop any stale channel on the former source so it does not
	// fight the operator's reconcile of replicationChannels[].sourcesList.
	// Full CHANGE REPLICATION SOURCE is applied through the PXC operator by
	// the controller updating spec.replication.channels[].sourcesList.
	// Local becomes replica of the new source. The channel that will run
	// on local is LocalReplicationChannel. STOP + RESET any stale state
	// on that channel so the PXC operator's next reconcile of
	// spec.replication.channels[].sourcesList takes effect cleanly.
	if err := e.cfg.LocalReplication.StopReplica(ctx, e.cfg.LocalReplicationChannel); err != nil {
		return fmt.Errorf("stop former-source replication channel: %w", err)
	}
	if err := e.cfg.LocalReplication.ResetReplicaAll(ctx, e.cfg.LocalReplicationChannel); err != nil {
		return fmt.Errorf("reset former-source replication channel: %w", err)
	}
	return nil
}

// phaseVerify confirms the final state matches the expected outcome of a
// successful flip. It is intentionally lightweight — heavy checks already ran
// in preflight.
func (e *Engine) phaseVerify(ctx context.Context) error {
	writable, err := e.cfg.RemotePXC.IsWritable(ctx)
	if err != nil {
		return fmt.Errorf("post-verify read @@read_only on new source: %w", err)
	}
	if !writable {
		return fmt.Errorf("post-verify: new source is still read_only=ON")
	}
	return nil
}

// rollback undoes completed phases in reverse order. Rollback is itself
// best-effort: if a step fails, the error is logged and we continue so that at
// least some of the state is restored.
func (e *Engine) rollback(ctx context.Context, completedPhases []Phase) {
	logger := log.FromContext(ctx)
	logger.Info("Rolling back switchover", "phasesCompleted", completedPhases)

	for i := len(completedPhases) - 1; i >= 0; i-- {
		phase := completedPhases[i]
		switch phase {
		case PhasePromote:
			logger.Info("Rollback: demoting new source back to read_only=ON")
			if err := e.cfg.RemotePXC.SetReadOnly(ctx); err != nil {
				logger.Error(err, "Rollback: failed to demote new source — MANUAL INTERVENTION REQUIRED")
			}
		case PhaseFence:
			logger.Info("Rollback: re-enabling writes on former source")
			if err := e.cfg.LocalPXC.SetReadWrite(ctx); err != nil {
				logger.Error(err, "Rollback: failed to re-enable former source — MANUAL INTERVENTION REQUIRED")
			}
		case PhaseRouting:
			logger.Info("Rollback: restoring ProxySQL routing")
			if err := e.cfg.LocalProxySQL.RollbackRouting(ctx, e.cfg.Routing); err != nil {
				logger.Error(err, "Rollback: failed to restore ProxySQL routing")
			}
		}
	}
}
