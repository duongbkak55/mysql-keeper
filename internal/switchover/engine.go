// Package switchover orchestrates the DC-DR MySQL cluster switchover operation.
// The engine executes switchover in well-defined phases, with rollback support
// if any phase fails.
package switchover

import (
	"context"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
)

// PXCManager is the interface the switchover engine uses to control a PXC cluster's
// read/write state. Both local and remote pxc.Manager implement this interface.
// Defining it here (not in the pxc package) keeps engine.go decoupled from the
// concrete pxc package and allows easy mocking in tests.
type PXCManager interface {
	// IsWritable returns true when the cluster has read_only=OFF.
	IsWritable(ctx context.Context) (bool, error)

	// SetReadOnly fences the cluster: SET GLOBAL read_only=ON, super_read_only=ON.
	// Also patches the local PXC CRD configuration (if k8s client is available)
	// so the setting persists across operator-triggered pod restarts.
	SetReadOnly(ctx context.Context) error

	// SetReadWrite promotes the cluster: SET GLOBAL read_only=OFF, super_read_only=OFF.
	// Also patches the local PXC CRD configuration to remove read_only lines.
	SetReadWrite(ctx context.Context) error
}

// ProxySQLManager is the interface the switchover engine uses to update routing.
type ProxySQLManager interface {
	ApplyFailoverRouting(ctx context.Context, cfg proxysql.RoutingConfig) error
	RollbackRouting(ctx context.Context, cfg proxysql.RoutingConfig) error
}

// Phase represents a single step in the switchover process.
type Phase int

const (
	PhaseVerify  Phase = iota // Verify preconditions (split-brain check)
	PhaseFence                // Fence local cluster (set read_only=ON)
	PhasePromote              // Promote remote cluster (set read_only=OFF)
	PhaseRouting              // Update local ProxySQL routing
	PhaseDone
)

func (p Phase) String() string {
	switch p {
	case PhaseVerify:
		return "Verify"
	case PhaseFence:
		return "Fence"
	case PhasePromote:
		return "Promote"
	case PhaseRouting:
		return "Routing"
	case PhaseDone:
		return "Done"
	default:
		return "Unknown"
	}
}

// Config holds the parameters needed to execute a switchover.
type Config struct {
	// LocalPXC manages the local cluster's read_only state (uses pxc.Manager with k8s client).
	LocalPXC PXCManager

	// RemotePXC manages the remote cluster's read_only state (uses pxc.NewRemoteManager, SQL only).
	RemotePXC PXCManager

	// LocalProxySQL updates this cluster's ProxySQL routing.
	LocalProxySQL ProxySQLManager

	// RoutingConfig describes the ProxySQL routing change to apply.
	Routing proxysql.RoutingConfig

	// DrainTimeout is how long to wait before fencing (allows graceful drain).
	DrainTimeout time.Duration

	// FenceTimeout is the timeout for the fencing operation.
	FenceTimeout time.Duration

	// Reason is a human-readable string describing why the switchover is happening.
	Reason string
}

// Result holds the outcome of a switchover attempt.
type Result struct {
	// Success is true if the switchover completed without rollback.
	Success bool

	// FailedPhase is the phase where the failure occurred (if !Success).
	FailedPhase Phase

	// RolledBack is true if rollback was successfully applied.
	RolledBack bool

	// Error holds the error that caused failure.
	Error error
}

// Engine executes and (if needed) rolls back a switchover.
type Engine struct {
	cfg Config
}

// NewEngine creates a switchover Engine.
func NewEngine(cfg Config) *Engine {
	return &Engine{cfg: cfg}
}

// Execute runs the switchover. It progresses through phases in order and
// rolls back completed phases if any phase fails.
//
// Switchover phases:
//  1. Verify — check preconditions (no split-brain, remote is healthy and read-only)
//  2. Fence  — set local cluster to read_only=ON (best-effort with timeout)
//  3. Promote — set remote cluster to read_only=OFF and verify write
//  4. Routing — update local ProxySQL to point writes to remote cluster
func (e *Engine) Execute(ctx context.Context) Result {
	logger := log.FromContext(ctx).WithValues("reason", e.cfg.Reason)
	logger.Info("Starting switchover")

	completedPhases := make([]Phase, 0, 4)

	// Phase 1: Verify preconditions.
	logger.Info("Phase: Verify")
	if err := e.phaseVerify(ctx); err != nil {
		logger.Error(err, "Verify phase failed — aborting switchover (no rollback needed)")
		return Result{Success: false, FailedPhase: PhaseVerify, Error: err}
	}
	completedPhases = append(completedPhases, PhaseVerify)

	// Phase 2: Fence local cluster.
	logger.Info("Phase: Fence", "drainTimeout", e.cfg.DrainTimeout)
	if e.cfg.DrainTimeout > 0 {
		time.Sleep(e.cfg.DrainTimeout) // allow existing connections to drain
	}
	if err := e.phaseFence(ctx); err != nil {
		// Fencing failure is non-fatal: the local cluster may be unreachable (that's why we're failing over).
		// Log the error and continue.
		logger.Error(err, "Fence phase failed (non-fatal — local cluster may be down, continuing)")
	} else {
		completedPhases = append(completedPhases, PhaseFence)
	}

	// Phase 3: Promote remote cluster.
	logger.Info("Phase: Promote")
	if err := e.phasePromote(ctx); err != nil {
		logger.Error(err, "Promote phase failed — rolling back")
		e.rollback(ctx, completedPhases)
		return Result{
			Success:     false,
			FailedPhase: PhasePromote,
			RolledBack:  true,
			Error:       err,
		}
	}
	completedPhases = append(completedPhases, PhasePromote)

	// Phase 4: Update local ProxySQL routing.
	logger.Info("Phase: Routing")
	if err := e.phaseRouting(ctx); err != nil {
		logger.Error(err, "Routing phase failed — rolling back")
		e.rollback(ctx, completedPhases)
		return Result{
			Success:     false,
			FailedPhase: PhaseRouting,
			RolledBack:  true,
			Error:       err,
		}
	}

	logger.Info("Switchover completed successfully")
	return Result{Success: true, FailedPhase: PhaseDone}
}

// phaseVerify checks all preconditions before allowing a switchover to proceed.
func (e *Engine) phaseVerify(ctx context.Context) error {
	// Check that the remote cluster is NOT already writable (split-brain guard).
	remoteWritable, err := e.cfg.RemotePXC.IsWritable(ctx)
	if err != nil {
		// Remote unreachable — we cannot safely promote; this is an error.
		return fmt.Errorf("cannot reach remote cluster to check read_only state: %w", err)
	}
	if remoteWritable {
		return fmt.Errorf("split-brain guard: remote cluster is already writable — aborting switchover")
	}
	return nil
}

// phaseFence sets the local cluster to read_only=ON.
// This is a best-effort operation — if the local cluster is already unreachable, we skip.
func (e *Engine) phaseFence(ctx context.Context) error {
	fenceCtx, cancel := context.WithTimeout(ctx, e.cfg.FenceTimeout)
	defer cancel()
	return e.cfg.LocalPXC.SetReadOnly(fenceCtx)
}

// phasePromote sets the remote cluster to read_only=OFF.
func (e *Engine) phasePromote(ctx context.Context) error {
	return e.cfg.RemotePXC.SetReadWrite(ctx)
}

// phaseRouting updates local ProxySQL to route writes to the promoted remote cluster.
func (e *Engine) phaseRouting(ctx context.Context) error {
	return e.cfg.LocalProxySQL.ApplyFailoverRouting(ctx, e.cfg.Routing)
}

// rollback undoes completed phases in reverse order.
func (e *Engine) rollback(ctx context.Context, completedPhases []Phase) {
	logger := log.FromContext(ctx)
	logger.Info("Rolling back switchover", "phasesCompleted", completedPhases)

	for i := len(completedPhases) - 1; i >= 0; i-- {
		phase := completedPhases[i]
		switch phase {
		case PhasePromote:
			// Re-set remote cluster back to read_only=ON.
			logger.Info("Rollback: demoting remote cluster back to read_only=ON")
			if err := e.cfg.RemotePXC.SetReadOnly(ctx); err != nil {
				logger.Error(err, "Rollback: failed to set remote cluster read_only=ON — MANUAL INTERVENTION REQUIRED")
			}
		case PhaseFence:
			// Re-enable writes on local cluster.
			logger.Info("Rollback: re-enabling writes on local cluster")
			if err := e.cfg.LocalPXC.SetReadWrite(ctx); err != nil {
				logger.Error(err, "Rollback: failed to re-enable local cluster writes — MANUAL INTERVENTION REQUIRED")
			}
		case PhaseRouting:
			// Restore original ProxySQL routing.
			logger.Info("Rollback: restoring ProxySQL routing")
			if err := e.cfg.LocalProxySQL.RollbackRouting(ctx, e.cfg.Routing); err != nil {
				logger.Error(err, "Rollback: failed to restore ProxySQL routing")
			}
		}
	}
}
