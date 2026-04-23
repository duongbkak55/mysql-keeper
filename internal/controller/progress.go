// Progress bridges the switchover.ProgressReporter interface to the CR status
// subresource so that every phase transition is durably checkpointed. That is
// what lets the reconciler resume after a pod restart instead of leaving the
// CR stuck in PhaseSwitchingOver.
package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

// progressReporter is the controller-owned implementation of
// switchover.ProgressReporter. It patches the CR status on every phase
// transition. Errors are logged and swallowed: a failed checkpoint write
// must never block the engine.
type progressReporter struct {
	client    ctrlclient.Client
	policyKey ctrlclient.ObjectKey
	attemptID string
	reason    string

	// mu guards completed so OnPhaseComplete callbacks do not race; in practice
	// Execute is single-threaded but we guard anyway.
	mu        sync.Mutex
	completed []string
}

func newProgressReporter(c ctrlclient.Client, key ctrlclient.ObjectKey, attemptID, reason string) *progressReporter {
	return &progressReporter{
		client:    c,
		policyKey: key,
		attemptID: attemptID,
		reason:    reason,
	}
}

func (p *progressReporter) OnPhaseStart(ctx context.Context, phase switchover.Phase) {
	log.FromContext(ctx).Info("phase_transition",
		"event", "phase_transition",
		"attemptID", p.attemptID,
		"phase", phase.String(),
		"state", "start",
	)
	p.writeProgress(ctx, phase.String(), "", "", p.snapshotCompleted())
}

func (p *progressReporter) OnPhaseComplete(ctx context.Context, phase switchover.Phase) {
	p.mu.Lock()
	p.completed = append(p.completed, phase.String())
	completed := append([]string(nil), p.completed...)
	p.mu.Unlock()

	log.FromContext(ctx).Info("phase_transition",
		"event", "phase_transition",
		"attemptID", p.attemptID,
		"phase", phase.String(),
		"state", "complete",
	)
	p.writeProgress(ctx, phase.String(), "", "", completed)
}

func (p *progressReporter) OnPhaseError(ctx context.Context, phase switchover.Phase, err error) {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	log.FromContext(ctx).Info("phase_transition",
		"event", "phase_transition",
		"attemptID", p.attemptID,
		"phase", phase.String(),
		"state", "error",
		"error", errMsg,
	)
	p.writeProgress(ctx, phase.String(), phase.String(), errMsg, p.snapshotCompleted())
}

func (p *progressReporter) snapshotCompleted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.completed) == 0 {
		return nil
	}
	return append([]string(nil), p.completed...)
}

func (p *progressReporter) writeProgress(
	ctx context.Context,
	currentPhase, failedPhase, errMsg string,
	completed []string,
) {
	logger := log.FromContext(ctx).WithValues(
		"attemptID", p.attemptID,
		"currentPhase", currentPhase,
	)

	obj := &mysqlv1alpha1.ClusterSwitchPolicy{}
	if err := p.client.Get(ctx, p.policyKey, obj); err != nil {
		logger.Error(err, "checkpoint: get CR failed")
		return
	}

	patch := ctrlclient.MergeFrom(obj.DeepCopy())
	if obj.Status.SwitchoverProgress == nil ||
		obj.Status.SwitchoverProgress.AttemptID != p.attemptID {
		obj.Status.SwitchoverProgress = &mysqlv1alpha1.SwitchoverProgress{
			AttemptID: p.attemptID,
			StartedAt: metav1.NewTime(time.Now()),
			Reason:    p.reason,
		}
	}
	prog := obj.Status.SwitchoverProgress
	prog.CurrentPhase = currentPhase
	prog.CompletedPhases = completed
	if failedPhase != "" {
		prog.FailedPhase = failedPhase
		prog.Error = errMsg
	}

	if err := p.client.Status().Patch(ctx, obj, patch); err != nil {
		logger.Error(err, "checkpoint: patch CR status failed (continuing)")
	}
}

// translatePreFlight flattens a switchover.PreFlightResult onto the CR-facing
// representation, truncating message strings that might be very long.
func translatePreFlight(r *switchover.PreFlightResult) *mysqlv1alpha1.PreFlightStatus {
	if r == nil {
		return nil
	}
	out := &mysqlv1alpha1.PreFlightStatus{
		PassedAt: metav1.NewTime(time.Now()),
		Checks:   make([]mysqlv1alpha1.PreFlightCheck, 0, len(r.Checks)),
	}
	if r.LocalSnapshot != nil {
		out.LocalGTIDExecuted = truncateGTID(r.LocalSnapshot.Executed)
	}
	if r.RemoteSnapshot != nil {
		out.RemoteGTIDExecuted = truncateGTID(r.RemoteSnapshot.Executed)
	}
	for _, c := range r.Checks {
		out.Checks = append(out.Checks, mysqlv1alpha1.PreFlightCheck{
			Name:     c.Name,
			Severity: c.Severity.String(),
			Passed:   c.Passed,
			Message:  truncateMessage(c.Message, 400),
		})
	}
	return out
}

func truncateGTID(s string) string {
	const maxLen = 2048 // plenty for the longest practical GTID set
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + fmt.Sprintf("…(%d more chars)", len(s)-maxLen)
}

func truncateMessage(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
