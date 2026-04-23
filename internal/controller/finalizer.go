// Finalizer handling for ClusterSwitchPolicy.
//
// When a CR is deleted mid-switchover we cannot safely let Kubernetes garbage-
// collect it. The engine may have partially fenced the local cluster, or have
// a half-applied ProxySQL routing, or be polling a MANO lcm-op-occ. The
// finalizer blocks deletion until the reconciler reaches a quiescent state
// and releases it explicitly.
package controller

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
)

// clusterSwitchFinalizer is attached to every managed CR so Kubernetes will
// not drop it while the controller is mid-operation.
const clusterSwitchFinalizer = "mysql.keeper.io/finalizer"

// ensureFinalizer adds the finalizer on an unmarked CR so any later deletion
// will block until we call removeFinalizer. Returns the patched CR and a flag
// indicating whether the patch actually changed anything.
func (r *ClusterSwitchPolicyReconciler) ensureFinalizer(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) (bool, error) {
	if controllerutil.ContainsFinalizer(policy, clusterSwitchFinalizer) {
		return false, nil
	}
	patch := client.MergeFrom(policy.DeepCopy())
	controllerutil.AddFinalizer(policy, clusterSwitchFinalizer)
	if err := r.Patch(ctx, policy, patch); err != nil {
		return false, fmt.Errorf("add finalizer: %w", err)
	}
	return true, nil
}

// handleDeletion runs the finalizer path when the CR has a deletionTimestamp.
// Returns a non-nil result when the caller should stop further processing.
//
// The current policy is conservative: if we are actively mid-switchover
// (Phase == SwitchingOver with a non-nil SwitchoverProgress whose StartedAt is
// recent) we refuse to remove the finalizer and log. An operator must either
// wait for the in-flight attempt to complete or abandon it manually via the
// Degraded path.
func (r *ClusterSwitchPolicyReconciler) handleDeletion(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) (*ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(policy, clusterSwitchFinalizer) {
		// Nothing to do; k8s will GC the object.
		return &ctrl.Result{}, nil
	}

	if policy.Status.Phase == mysqlv1alpha1.PhaseSwitchingOver {
		prog := policy.Status.SwitchoverProgress
		stuckLimit := policy.Spec.Switchover.ResumeStuckTimeout.Duration
		if stuckLimit <= 0 {
			stuckLimit = 10 * time.Minute
		}
		if prog != nil && time.Since(prog.StartedAt.Time) < stuckLimit {
			logger.Info("Refusing to finalize CR: switchover is in flight",
				"attemptID", prog.AttemptID,
				"currentPhase", prog.CurrentPhase)
			return &ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
		logger.Info("Finalizing CR mid-switchover (exceeded ResumeStuckTimeout)",
			"currentPhase", func() string {
				if prog == nil {
					return ""
				}
				return prog.CurrentPhase
			}())
	}

	patch := client.MergeFrom(policy.DeepCopy())
	controllerutil.RemoveFinalizer(policy, clusterSwitchFinalizer)
	if err := r.Patch(ctx, policy, patch); err != nil {
		return nil, fmt.Errorf("remove finalizer: %w", err)
	}
	logger.Info("Finalizer removed; object will be deleted")
	return &ctrl.Result{}, nil
}
