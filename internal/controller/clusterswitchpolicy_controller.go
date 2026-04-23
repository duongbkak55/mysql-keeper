// Package controller implements the ClusterSwitchPolicy reconciler.
//
// The reconciler has five responsibilities:
//
//  1. Run the health checks that decide whether a failover is even a
//     reasonable response. "Both clusters ReadOnly" is an incident, not a
//     failover condition — the guard that distinguishes these two cases is
//     what prevents the Error 1236 divergence that was observed in production.
//
//  2. Initialise keeper.probe / keeper.leader on whichever cluster happens to
//     be writable, so that a subsequent promotion does not fail its write
//     verification because the schema does not yet exist.
//
//  3. Execute the switchover engine, threading an attemptID and a progress
//     reporter so every phase transition is durable in the CR status.
//
//  4. Resume or abandon a stuck switchover after a controller restart. If the
//     CR shows Phase=SwitchingOver and SwitchoverProgress points to a step we
//     have no way to safely continue, we flip to Degraded and let a human
//     decide.
//
//  5. Keep the lightweight counters and cooldown bookkeeping up to date so the
//     downstream metrics / alerts make sense.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/health"
	"github.com/duongnguyen/mysql-keeper/internal/mano"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

// ClusterSwitchPolicyReconciler reconciles a ClusterSwitchPolicy object.
// +kubebuilder:rbac:groups=mysql.keeper.io,resources=clusterswitchpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mysql.keeper.io,resources=clusterswitchpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pxc.percona.com,resources=perconaxtradbclusters,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
type ClusterSwitchPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// componentSet groups everything we build per-reconcile so the signature of
// functions that need them stays readable.
type componentSet struct {
	localChecker  *health.Checker
	remoteChecker *health.Checker

	// localPXC / remotePXC are the state-change surface the engine drives.
	localPXC  switchover.PXCManager
	remotePXC switchover.PXCManager

	// localInspector / remoteInspector are the same objects cast to the
	// read-only inspector interface when available (direct *pxc.Manager always
	// satisfies it; *mano.PXCManager also satisfies it by delegating to its
	// embedded direct-SQL helper).
	localInspector  switchover.ReplicationInspector
	remoteInspector switchover.ReplicationInspector

	// Replication controllers for Promote / ReverseReplica phases.
	localReplication  switchover.ReplicationController
	remoteReplication switchover.ReplicationController

	// schemaInit is the direct-MySQL handle used for EnsureKeeperSchema. It is
	// always a *pxc.Manager regardless of whether state changes flow through
	// MANO, because MANO has no equivalent for CREATE TABLE.
	localSchemaInit  *pxc.Manager
	remoteSchemaInit *pxc.Manager

	proxySQLMgr *proxysql.Manager
}

// Reconcile is the main reconciliation loop entry point.
//
// Trigger sources:
//   - changes to a ClusterSwitchPolicy resource
//   - periodic requeue controlled by Spec.HealthCheck.Interval
//
// On every call we run the health checks, fold them into the CR status, then
// decide what to do:
//
//   - resume a previously-started switchover, or abandon it if it has been
//     stuck long enough
//   - run a brand-new switchover (manual trigger, or auto-failover conditions
//     met with all of the new guards passing)
//   - just go back to Monitoring
func (r *ClusterSwitchPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := &mysqlv1alpha1.ClusterSwitchPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Deletion path: let the finalizer logic decide whether it is safe to
	// release the CR, then stop.
	if !policy.DeletionTimestamp.IsZero() {
		if res, err := r.handleDeletion(ctx, policy); err != nil {
			return ctrl.Result{}, err
		} else if res != nil {
			return *res, nil
		}
		return ctrl.Result{}, nil
	}

	// First-time: attach the finalizer so deletes block on us instead of
	// racing against a mid-switchover.
	if added, err := r.ensureFinalizer(ctx, policy); err != nil {
		return ctrl.Result{}, err
	} else if added {
		// We just mutated the object — return so the next reconcile sees the
		// finalizer in place before we change anything else.
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize Phase so we never observe an empty string.
	if policy.Status.Phase == "" {
		patch := client.MergeFrom(policy.DeepCopy())
		policy.Status.Phase = mysqlv1alpha1.PhaseInitializing
		if err := r.Status().Patch(ctx, policy, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	comps, err := r.buildComponents(ctx, policy)
	if err != nil {
		logger.Error(err, "buildComponents failed")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Snapshot cluster health first.
	localHealth := comps.localChecker.Check(ctx)
	remoteHealth := comps.remoteChecker.Check(ctx)

	r.updateMetrics(policy, localHealth, remoteHealth)

	// Persist the counters/health derived from this cycle before any further
	// branching. We keep the merge base cloned before we mutate the status so
	// the patch is a pure diff.
	healthPatch := client.MergeFrom(policy.DeepCopy())
	r.updateHealthStatus(policy, localHealth, remoteHealth)
	r.updateSplitBrainCondition(policy, localHealth, remoteHealth)
	if err := r.Status().Patch(ctx, policy, healthPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch health status: %w", err)
	}

	// Try to ensure keeper.probe / keeper.leader on the writable side. This is
	// idempotent and cheap; skipping the error keeps unrelated reconciles
	// moving in the presence of transient connectivity issues.
	r.ensureKeeperSchema(ctx, policy, comps, localHealth, remoteHealth)

	// Heartbeat the leader lease on the active cluster. The lease is used by
	// the peer controller to decide whether it can promote; a fresh lease
	// means "don't flip, the source is alive". Failure here is logged but
	// non-fatal — we do not want a transient DB blip to cause a split-brain
	// decision on the other side.
	r.heartbeatLeaderLease(ctx, policy, comps, localHealth)

	// If a switchover checkpoint exists, decide resume vs abandon before we
	// consider starting a new one.
	if resumeResult := r.handleStuckSwitchover(ctx, policy); resumeResult != nil {
		return *resumeResult, nil
	}

	reason, shouldSwitch := r.decideSwitchover(ctx, policy, comps, localHealth, remoteHealth)
	if !shouldSwitch {
		if policy.Status.Phase != mysqlv1alpha1.PhaseSwitchingOver {
			patch := client.MergeFrom(policy.DeepCopy())
			policy.Status.Phase = mysqlv1alpha1.PhaseMonitoring
			if err := r.Status().Patch(ctx, policy, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: policy.Spec.HealthCheck.Interval.Duration}, nil
	}

	attemptID := uuid.NewString()
	logger = logger.WithValues("attemptID", attemptID, "reason", reason)
	logger.Info("Starting switchover attempt")

	// Clear the manual trigger BEFORE we run the switchover. If we left it set
	// and the attempt failed, the next reconcile would see the trigger again
	// and re-enter Execute — exactly the ping-pong we want to avoid. The
	// manual trigger should represent "one attempt" regardless of outcome.
	if policy.Spec.ManualSwitchoverTarget != "" {
		specPatch := client.MergeFrom(policy.DeepCopy())
		policy.Spec.ManualSwitchoverTarget = ""
		if err := r.Patch(ctx, policy, specPatch); err != nil {
			logger.Error(err, "Failed to clear manualSwitchoverTarget before execute — aborting")
			return ctrl.Result{}, err
		}
	}

	// Mark Phase=SwitchingOver up front so concurrent reconciles do not
	// retrigger. The progressReporter will take over writing SwitchoverProgress
	// from this point on.
	startPatch := client.MergeFrom(policy.DeepCopy())
	policy.Status.Phase = mysqlv1alpha1.PhaseSwitchingOver
	policy.Status.SwitchoverProgress = &mysqlv1alpha1.SwitchoverProgress{
		AttemptID:    attemptID,
		StartedAt:    metav1.NewTime(time.Now()),
		CurrentPhase: switchover.PhasePreFlight.String(),
		Reason:       reason,
	}
	if err := r.Status().Patch(ctx, policy, startPatch); err != nil {
		return ctrl.Result{}, err
	}

	result := r.executeSwitchover(ctx, policy, comps, reason, attemptID)
	return r.writeSwitchoverResult(ctx, policy, result, reason)
}

// handleStuckSwitchover inspects SwitchoverProgress. If a previous attempt did
// not reach Done and we have been there longer than ResumeStuckTimeout, we
// abandon it — the engine has no safe resume step for most phases, so the
// best we can do is mark Degraded and let an operator intervene.
//
// The alternative (try to resume) is unsafe: without durable side-effect
// identifiers (e.g. MANO lcmOpOccId) we cannot distinguish "Promote partially
// succeeded" from "Promote never started". That work is tracked in Sprint 2.
func (r *ClusterSwitchPolicyReconciler) handleStuckSwitchover(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) *ctrl.Result {
	prog := policy.Status.SwitchoverProgress
	if prog == nil {
		return nil
	}
	logger := log.FromContext(ctx)

	stuckFor := time.Since(prog.StartedAt.Time)
	stuckLimit := policy.Spec.Switchover.ResumeStuckTimeout.Duration
	if stuckLimit <= 0 {
		stuckLimit = 10 * time.Minute
	}
	if stuckFor < stuckLimit {
		// Still within the window. We do not resume yet — we let the in-flight
		// Reconcile that is running the engine finish. If the controller pod
		// restarted during Execute, stuckFor will pass stuckLimit on a later
		// reconcile and we will end up in the branch below.
		return nil
	}

	logger.Info("Abandoning stuck switchover — moving to Degraded",
		"attemptID", prog.AttemptID,
		"currentPhase", prog.CurrentPhase,
		"stuckFor", stuckFor.String())

	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.Phase = mysqlv1alpha1.PhaseDegraded
	prog.FailedPhase = prog.CurrentPhase
	if prog.Error == "" {
		prog.Error = fmt.Sprintf("abandoned after %s — manual intervention required", stuckFor)
	}

	metrics.ManualInterventionRequired.WithLabelValues(
		policy.Spec.ClusterRole, prog.CurrentPhase,
	).Inc()

	if err := r.Status().Patch(ctx, policy, patch); err != nil {
		logger.Error(err, "failed to mark policy as Degraded")
	}
	return &ctrl.Result{RequeueAfter: policy.Spec.HealthCheck.Interval.Duration}
}

// decideSwitchover is the single place that answers "should we flip now?".
// It combines:
//
//   - the manual trigger on Spec.ManualSwitchoverTarget
//   - automatic failover when the local cluster has been unhealthy for enough
//     cycles AND the remote side is reachable AND read-only (split-brain guard)
//     AND not both-ReadOnly (cluster-wide incident guard)
//   - cooldown from a recent switchover
func (r *ClusterSwitchPolicyReconciler) decideSwitchover(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	localHealth, remoteHealth health.ClusterHealth,
) (reason string, should bool) {
	logger := log.FromContext(ctx)

	// 1. Cooldown check applies to both manual and auto triggers.
	if policy.Status.LastSwitchoverTime != nil {
		cooldown := policy.Spec.Switchover.CooldownPeriod.Duration
		if cooldown <= 0 {
			cooldown = 10 * time.Minute
		}
		since := time.Since(policy.Status.LastSwitchoverTime.Time)
		if since < cooldown {
			logger.Info("Switchover cooldown active — skipping",
				"sinceLast", since.String(),
				"cooldown", cooldown.String())
			metrics.CooldownBlocked.WithLabelValues(policy.Spec.ClusterRole).Inc()
			return "", false
		}
	}

	// 2. Manual trigger.
	if policy.Spec.ManualSwitchoverTarget == "promote-remote" {
		return "manual switchover requested via spec.manualSwitchoverTarget", true
	}

	// 3. Automatic failover.
	if !policy.Spec.AutoFailover {
		return "", false
	}
	if policy.Status.ConsecutiveLocalFailures < policy.Spec.HealthCheck.FailureThreshold {
		return "", false
	}

	// Both-ReadOnly guard: this is the exact scenario that triggered the
	// production incident (both clusters RO because of cluster-wide quorum
	// loss). The right action is to alert a human, not to flip direction.
	if localHealth.Writable == health.WritableNo && remoteHealth.Writable == health.WritableNo {
		logger.Info("Both clusters ReadOnly — cluster-wide incident, refusing auto-failover",
			"localMsg", localHealth.Message,
			"remoteMsg", remoteHealth.Message)
		metrics.BothClustersReadOnly.WithLabelValues(policy.Spec.ClusterRole).Inc()
		return "", false
	}

	// Standard auto-failover conditions: local is unreachable / unhealthy, but
	// the remote is reachable and safely read-only.
	if remoteHealth.Writable != health.WritableNo {
		logger.Info("Local unhealthy but remote not safe to promote — skipping",
			"remoteWritable", remoteHealth.Writable.String(),
			"remoteHealthy", remoteHealth.Healthy)
		return "", false
	}

	if !remoteHealth.Healthy {
		logger.Info("Local unhealthy but remote is also unhealthy — skipping",
			"remoteMsg", remoteHealth.Message)
		return "", false
	}

	// Peer-lease gate: the cluster we are about to promote has a lease row
	// pointing at a live peer controller. If that lease is fresh and owned by
	// someone other than us, the peer is already running; stepping in would
	// race with their promotion decision. Only take over a stale lease.
	if comps.remoteSchemaInit != nil {
		ownerID := controllerOwnerID(policy)
		ttl := leaseTTL(policy)
		if err := switchover.CheckPeerLease(ctx, comps.remoteSchemaInit, time.Now(), ttl, ownerID); err != nil {
			logger.Info("Peer-lease gate denied switchover",
				"reason", err.Error())
			metrics.ManualInterventionRequired.WithLabelValues(
				policy.Spec.ClusterRole, "peer_lease",
			).Inc()
			return "", false
		}
	}

	return fmt.Sprintf("automatic failover: local unhealthy for %d consecutive checks",
		policy.Status.ConsecutiveLocalFailures), true
}

// executeSwitchover builds the engine config and invokes Execute.
func (r *ClusterSwitchPolicyReconciler) executeSwitchover(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	reason, attemptID string,
) switchover.Result {
	sw := policy.Spec.Switchover
	preflight := policy.Spec.PreFlight

	reporter := newProgressReporter(r.Client,
		types.NamespacedName{Namespace: policy.Namespace, Name: policy.Name},
		attemptID, reason,
	)

	engine := switchover.NewEngine(switchover.Config{
		LocalPXC:           comps.localPXC,
		RemotePXC:          comps.remotePXC,
		LocalInspector:     comps.localInspector,
		RemoteInspector:    comps.remoteInspector,
		LocalReplication:   comps.localReplication,
		RemoteReplication:  comps.remoteReplication,
		LocalProxySQL:      comps.proxySQLMgr,
		ReplicationChannel: policy.Spec.ReplicationChannelName,
		Routing: proxysql.RoutingConfig{
			OldWriterHost:      sw.LocalWriterHost,
			OldWriterPort:      3306,
			NewWriterHost:      sw.RemoteWriterHost,
			NewWriterPort:      sw.RemoteWriterPort,
			ReadWriteHostgroup: sw.ReadWriteHostgroup,
			ReadOnlyHostgroup:  sw.ReadOnlyHostgroup,
		},
		BlackholeFence: proxysql.BlackholeConfig{
			TargetHost:         sw.LocalWriterHost,
			TargetPort:         3306,
			BlackholeHostgroup: sw.BlackholeHostgroup,
		},
		CatchupTimeout:            preflight.CatchupTimeout.Duration,
		MinBinlogRetentionSeconds: preflight.MinBinlogRetentionSeconds,
		DrainTimeout:              sw.DrainTimeout.Duration,
		FenceTimeout:              sw.FenceTimeout.Duration,
		AllowDataLossFailover:     policy.Spec.AllowDataLossFailover,
		Reason:                    reason,
		AttemptID:                 attemptID,
		Progress:                  reporter,
	})

	switchCtx, cancel := context.WithTimeout(ctx, sw.Timeout.Duration)
	defer cancel()

	start := time.Now()
	result := engine.Execute(switchCtx)
	elapsed := time.Since(start).Seconds()

	resultLabel := "success"
	if !result.Success {
		if result.RolledBack {
			resultLabel = "rollback"
		} else {
			resultLabel = "failure"
		}
	}
	metrics.SwitchoverDurationSeconds.WithLabelValues(policy.Spec.ClusterRole, resultLabel).Observe(elapsed)

	return result
}

// writeSwitchoverResult updates the CR status after Execute returns,
// regardless of success or failure. On success it flips ActiveCluster, resets
// counters, and clears ManualSwitchoverTarget. On failure it leaves the CR in
// Degraded so alerts fire, while still surfacing the preflight snapshot.
func (r *ClusterSwitchPolicyReconciler) writeSwitchoverResult(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	result switchover.Result,
	reason string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	resultPatch := client.MergeFrom(policy.DeepCopy())
	policy.Status.LastPreFlight = translatePreFlight(result.PreFlight)

	if result.Success {
		now := metav1.Now()
		policy.Status.LastSwitchoverTime = &now
		policy.Status.LastSwitchoverReason = reason
		policy.Status.Phase = mysqlv1alpha1.PhaseMonitoring
		policy.Status.ConsecutiveLocalFailures = 0
		policy.Status.ConsecutiveLocalUnreachable = 0

		if policy.Status.ActiveCluster == mysqlv1alpha1.ClusterRoleDC {
			policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDR
		} else {
			policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDC
		}

		if policy.Status.SwitchoverProgress != nil {
			policy.Status.SwitchoverProgress.CurrentPhase = switchover.PhaseDone.String()
			policy.Status.SwitchoverProgress.FailedPhase = ""
			policy.Status.SwitchoverProgress.Error = ""
		}

		metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "success").Inc()
	} else {
		policy.Status.Phase = mysqlv1alpha1.PhaseDegraded
		logger.Error(result.Error, "Switchover failed",
			"phase", result.FailedPhase.String(),
			"rolledBack", result.RolledBack,
		)
		if policy.Status.SwitchoverProgress != nil {
			policy.Status.SwitchoverProgress.FailedPhase = result.FailedPhase.String()
			if result.Error != nil {
				policy.Status.SwitchoverProgress.Error = result.Error.Error()
			}
		}
		metrics.PreFlightFailures.WithLabelValues(policy.Spec.ClusterRole, result.FailedPhase.String()).Inc()

		if result.RolledBack {
			metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "rollback").Inc()
		} else {
			metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "failure").Inc()
		}
	}

	if err := r.Status().Patch(ctx, policy, resultPatch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: policy.Spec.HealthCheck.Interval.Duration}, nil
}

// ensureKeeperSchema runs EnsureKeeperSchema on whichever cluster is currently
// writable. This is the bug-4.4 fix: SetReadWrite's write probe needs the table
// to exist, and previously nothing ever created it. We only write to a cluster
// whose Writable == WritableYes so we never try SQL DDL on a read-only replica.
func (r *ClusterSwitchPolicyReconciler) ensureKeeperSchema(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	localHealth, remoteHealth health.ClusterHealth,
) {
	logger := log.FromContext(ctx)

	if localHealth.Writable == health.WritableYes && comps.localSchemaInit != nil {
		if err := comps.localSchemaInit.EnsureKeeperSchema(ctx); err != nil {
			logger.Error(err, "EnsureKeeperSchema on local writable cluster failed")
		}
		if err := comps.localSchemaInit.EnsureLeaderLeaseSchema(ctx); err != nil {
			logger.Error(err, "EnsureLeaderLeaseSchema on local writable cluster failed")
		}
	}
	if remoteHealth.Writable == health.WritableYes && comps.remoteSchemaInit != nil {
		if err := comps.remoteSchemaInit.EnsureKeeperSchema(ctx); err != nil {
			logger.Error(err, "EnsureKeeperSchema on remote writable cluster failed")
		}
		if err := comps.remoteSchemaInit.EnsureLeaderLeaseSchema(ctx); err != nil {
			logger.Error(err, "EnsureLeaderLeaseSchema on remote writable cluster failed")
		}
	}
	_ = policy // reserved for future use (config-driven enable/disable)
}

// heartbeatLeaderLease renews keeper.leader on the active (writable) cluster
// so the peer controller can see that we are alive. When the local cluster is
// the active one we talk to it directly; otherwise we skip (the peer is the
// one keeping its own lease fresh).
func (r *ClusterSwitchPolicyReconciler) heartbeatLeaderLease(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	localHealth health.ClusterHealth,
) {
	if comps.localSchemaInit == nil {
		return
	}
	if localHealth.Writable != health.WritableYes {
		return
	}
	logger := log.FromContext(ctx)

	ownerID := controllerOwnerID(policy)
	ttl := leaseTTL(policy)
	if _, err := comps.localSchemaInit.AcquireOrRenewLease(ctx, ownerID, ttl); err != nil {
		logger.Error(err, "lease heartbeat failed (non-fatal)",
			"owner", ownerID, "ttl", ttl.String())
	}
}

// controllerOwnerID is the identity this controller writes into keeper.leader
// so the peer can tell us apart from itself. The role+namespace+name tuple is
// unique per CR and survives pod restarts.
func controllerOwnerID(policy *mysqlv1alpha1.ClusterSwitchPolicy) string {
	return fmt.Sprintf("%s:%s/%s",
		policy.Spec.ClusterRole, policy.Namespace, policy.Name)
}

// leaseTTL derives the lease-expiry window from the configured health check
// interval. We want at least 3 missed heartbeats to declare the peer stale
// so a single blip does not cause a mistaken takeover.
func leaseTTL(policy *mysqlv1alpha1.ClusterSwitchPolicy) time.Duration {
	interval := policy.Spec.HealthCheck.Interval.Duration
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ttl := interval * 3
	if ttl < 30*time.Second {
		ttl = 30 * time.Second
	}
	return ttl
}

// updateHealthStatus persists the freshly-observed health snapshots and the
// consecutive-failure counters on the CR. Separate counters for "unhealthy"
// (can reach MySQL but something is wrong) and "unreachable" (TCP fails) so
// that short network blips do not count toward the failover threshold.
func (r *ClusterSwitchPolicyReconciler) updateHealthStatus(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localH health.ClusterHealth,
	remoteH health.ClusterHealth,
) {
	now := metav1.Now()

	policy.Status.LocalHealth = mysqlv1alpha1.ClusterHealthStatus{
		Healthy:            localH.Healthy,
		PXCState:           localH.PXCState,
		PXCReadyNodes:      localH.PXCReadyNodes,
		WsrepClusterSize:   localH.WsrepClusterSize,
		WsrepClusterStatus: localH.WsrepClusterStatus,
		IsWritable:         localH.IsWritable,
		ProxySQLHealthy:    localH.ProxySQLHealthy,
		LastChecked:        &now,
		Message:            localH.Message,
	}

	policy.Status.RemoteHealth = mysqlv1alpha1.ClusterHealthStatus{
		Healthy:            remoteH.Healthy,
		WsrepClusterSize:   remoteH.WsrepClusterSize,
		WsrepClusterStatus: remoteH.WsrepClusterStatus,
		IsWritable:         remoteH.IsWritable,
		LastChecked:        &now,
		Message:            remoteH.Message,
	}

	// Tri-state counter updates.
	switch {
	case localH.Healthy:
		policy.Status.ConsecutiveLocalFailures = 0
		policy.Status.ConsecutiveLocalUnreachable = 0
	case localH.Writable == health.WritableUnknown:
		policy.Status.ConsecutiveLocalUnreachable++
		// "unreachable" does not (yet) count toward failover; we want a
		// separate threshold for that if the operator wants it later.
	default:
		policy.Status.ConsecutiveLocalFailures++
	}

	if remoteH.Healthy {
		policy.Status.ConsecutiveRemoteFailures = 0
	} else {
		policy.Status.ConsecutiveRemoteFailures++
	}

	if policy.Status.ActiveCluster == "" {
		if localH.IsWritable {
			policy.Status.ActiveCluster = policy.Spec.ClusterRole
		} else if remoteH.IsWritable {
			if policy.Spec.ClusterRole == mysqlv1alpha1.ClusterRoleDC {
				policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDR
			} else {
				policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDC
			}
		}
	}
}

// updateSplitBrainCondition folds the tri-state writable flags into the
// standard ConditionSplitBrainSafe condition so kubectl describe and
// conditions-based alerting surface it uniformly.
func (r *ClusterSwitchPolicyReconciler) updateSplitBrainCondition(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localH, remoteH health.ClusterHealth,
) {
	cond := metav1.Condition{
		Type:               mysqlv1alpha1.ConditionSplitBrainSafe,
		LastTransitionTime: metav1.Now(),
	}
	switch {
	case localH.Writable == health.WritableYes && remoteH.Writable == health.WritableYes:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "BothWritable"
		cond.Message = "CRITICAL: both clusters are read_only=OFF"
		metrics.BothClustersWritable.WithLabelValues(policy.Spec.ClusterRole).Inc()
	case localH.Writable == health.WritableNo && remoteH.Writable == health.WritableNo:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "BothReadOnly"
		cond.Message = "Both clusters are read_only=ON — auto-failover suppressed"
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Normal"
		cond.Message = "Exactly one cluster is writable or state is being determined"
	}
	meta.SetStatusCondition(&policy.Status.Conditions, cond)
}

// updateMetrics pushes Prometheus gauge values from health check results.
func (r *ClusterSwitchPolicyReconciler) updateMetrics(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localH health.ClusterHealth,
	remoteH health.ClusterHealth,
) {
	role := policy.Spec.ClusterRole

	boolToFloat := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}

	metrics.ClusterHealthy.WithLabelValues(role, "local").Set(boolToFloat(localH.Healthy))
	metrics.ClusterHealthy.WithLabelValues(role, "remote").Set(boolToFloat(remoteH.Healthy))

	metrics.ClusterWritable.WithLabelValues(role, "local").Set(boolToFloat(localH.IsWritable))
	metrics.ClusterWritable.WithLabelValues(role, "remote").Set(boolToFloat(remoteH.IsWritable))

	metrics.ConsecutiveFailures.WithLabelValues(role, "local").Set(
		float64(policy.Status.ConsecutiveLocalFailures),
	)
	metrics.ConsecutiveFailures.WithLabelValues(role, "remote").Set(
		float64(policy.Status.ConsecutiveRemoteFailures),
	)

	metrics.ProxySQLHealthyInstances.WithLabelValues(role).Set(float64(localH.ProxySQLHealthy))
}

// buildComponents constructs all the health checkers and managers from the
// policy spec. Values are newed per-reconcile; connection pooling across
// reconciles is tracked in Sprint 2 as an optimization.
func (r *ClusterSwitchPolicyReconciler) buildComponents(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) (*componentSet, error) {
	timeout := policy.Spec.HealthCheck.MySQLCheckTimeout.Duration

	localUser, localPass, err := r.readSecret(ctx, policy.Spec.LocalMySQL.CredentialsSecretRef)
	if err != nil {
		return nil, fmt.Errorf("local MySQL credentials: %w", err)
	}
	localDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		localUser, localPass,
		policy.Spec.LocalMySQL.Host, policy.Spec.LocalMySQL.Port,
	)

	remoteUser, remotePass, err := r.readSecret(ctx, policy.Spec.RemoteMySQL.CredentialsSecretRef)
	if err != nil {
		return nil, fmt.Errorf("remote MySQL credentials: %w", err)
	}
	remoteDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		remoteUser, remotePass,
		policy.Spec.RemoteMySQL.Host, policy.Spec.RemoteMySQL.Port,
	)

	localPXCChecker := health.NewLocalPXCChecker(
		r.Client,
		policy.Spec.PXCNamespace,
		policy.Spec.PXCName,
		localDSN,
		timeout,
	)
	remotePXCChecker := health.NewRemotePXCChecker(remoteDSN, timeout)

	proxySQLEndpoints := make([]health.ProxySQLEndpointInfo, 0, len(policy.Spec.ProxySQL))
	proxySQLMgrEndpoints := make([]proxysql.Endpoint, 0, len(policy.Spec.ProxySQL))

	for _, ep := range policy.Spec.ProxySQL {
		u, p, err := r.readSecret(ctx, ep.CredentialsSecretRef)
		if err != nil {
			return nil, fmt.Errorf("ProxySQL creds for %s: %w", ep.Host, err)
		}
		proxySQLEndpoints = append(proxySQLEndpoints, health.ProxySQLEndpointInfo{
			Host: ep.Host, Port: ep.AdminPort, Username: u, Password: p,
		})
		proxySQLMgrEndpoints = append(proxySQLMgrEndpoints, proxysql.Endpoint{
			Host: ep.Host, Port: ep.AdminPort, Username: u, Password: p,
		})
	}

	proxySQLChecker := health.NewProxySQLChecker(proxySQLEndpoints, timeout)
	comps := &componentSet{
		localChecker:  health.NewLocalChecker(localPXCChecker, proxySQLChecker, policy.Spec.HealthCheck.ProxySQLMinHealthy),
		remoteChecker: health.NewRemoteChecker(remotePXCChecker),
		proxySQLMgr:   proxysql.NewManager(proxySQLMgrEndpoints, timeout),
	}

	// SQL-only managers are always available — they are what we use for
	// read-only introspection regardless of whether state changes flow
	// through MANO or direct k8s API.
	localSQL := pxc.NewManager(localDSN, timeout, r.Client,
		policy.Spec.PXCNamespace, policy.Spec.PXCName, policy.Spec.ReplicationChannelName)
	remoteSQL := pxc.NewRemoteManager(remoteDSN, timeout)
	comps.localSchemaInit = localSQL
	comps.remoteSchemaInit = remoteSQL
	comps.localInspector = localSQL
	comps.remoteInspector = remoteSQL
	comps.localReplication = localSQL
	comps.remoteReplication = remoteSQL

	if policy.Spec.MANO != nil {
		manoClient, err := r.buildMANOClient(ctx, policy.Spec.MANO)
		if err != nil {
			return nil, fmt.Errorf("build MANO client: %w", err)
		}
		pollInterval := policy.Spec.MANO.PollInterval.Duration
		if pollInterval == 0 {
			pollInterval = 5 * time.Second
		}
		pollTimeout := policy.Spec.MANO.PollTimeout.Duration
		if pollTimeout == 0 {
			pollTimeout = 5 * time.Minute
		}
		localMano := mano.NewPXCManager(manoClient,
			policy.Spec.MANO.LocalCNFName, policy.Spec.MANO.LocalVDUName,
			pollInterval, pollTimeout,
			localDSN, timeout,
		)
		remoteMano := mano.NewPXCManager(manoClient,
			policy.Spec.MANO.RemoteCNFName, policy.Spec.MANO.RemoteVDUName,
			pollInterval, pollTimeout,
			remoteDSN, timeout,
		)
		comps.localPXC = localMano
		comps.remotePXC = remoteMano
		// MANO-backed managers expose the introspection methods by delegation
		// — keep using the direct-SQL inspectors so we do not double-pool.
	} else {
		comps.localPXC = localSQL

		if policy.Spec.RemoteKubeAPI != nil {
			remoteKubeClient, err := r.buildRemoteKubeClient(ctx, policy.Spec.RemoteKubeAPI)
			if err != nil {
				return nil, fmt.Errorf("build remote k8s client: %w", err)
			}
			comps.remotePXC = pxc.NewRemoteManagerWithKubeAPI(remoteDSN, timeout,
				remoteKubeClient,
				policy.Spec.RemoteKubeAPI.PXCNamespace,
				policy.Spec.RemoteKubeAPI.PXCName,
				policy.Spec.ReplicationChannelName,
			)
		} else {
			comps.remotePXC = remoteSQL
		}
	}

	return comps, nil
}

// buildMANOClient constructs a mano.Client from the MANOConfig.
// Prefers CredentialsSecretRef (auto-login) over TokenSecretRef (static token).
func (r *ClusterSwitchPolicyReconciler) buildMANOClient(ctx context.Context, cfg *mysqlv1alpha1.MANOConfig) (*mano.Client, error) {
	if cfg.CredentialsSecretRef != nil {
		username, password, err := r.readSecret(ctx, *cfg.CredentialsSecretRef)
		if err != nil {
			return nil, fmt.Errorf("read MANO credentials: %w", err)
		}
		return mano.NewClientWithCredentials(cfg.Host, username, password), nil
	}
	if cfg.TokenSecretRef != nil {
		token, err := r.readSecretKey(ctx, *cfg.TokenSecretRef, "token")
		if err != nil {
			return nil, fmt.Errorf("read MANO token: %w", err)
		}
		return mano.NewClient(cfg.Host, token), nil
	}
	return nil, fmt.Errorf("MANO config requires either tokenSecretRef or credentialsSecretRef")
}

// buildRemoteKubeClient constructs a k8s client pointing to the remote cluster's API server.
func (r *ClusterSwitchPolicyReconciler) buildRemoteKubeClient(ctx context.Context, cfg *mysqlv1alpha1.RemoteKubeAPIConfig) (client.Client, error) {
	token, err := r.readSecretKey(ctx, cfg.TokenSecretRef, "token")
	if err != nil {
		return nil, fmt.Errorf("read remote k8s token: %w", err)
	}

	restCfg := &rest.Config{
		Host:        cfg.Host,
		BearerToken: token,
	}

	if cfg.CASecretRef != nil {
		caData, err := r.readSecretKey(ctx, *cfg.CASecretRef, "ca.crt")
		if err != nil {
			return nil, fmt.Errorf("read remote k8s CA cert: %w", err)
		}
		restCfg.TLSClientConfig = rest.TLSClientConfig{CAData: []byte(caData)}
	} else {
		restCfg.TLSClientConfig = rest.TLSClientConfig{Insecure: true}
	}

	scheme := runtime.NewScheme()
	if err := pxc.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add PXC scheme: %w", err)
	}

	remoteClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create remote k8s client: %w", err)
	}
	return remoteClient, nil
}

// readSecretKey reads a single key from a Kubernetes Secret.
func (r *ClusterSwitchPolicyReconciler) readSecretKey(ctx context.Context, ref mysqlv1alpha1.SecretRef, key string) (string, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = "mysql-keeper-system"
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("get Secret %s/%s: %w", ns, ref.Name, err)
	}
	val, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("Secret %s/%s missing %q key", ns, ref.Name, key)
	}
	return string(val), nil
}

// readSecret reads username and password from a Kubernetes Secret.
func (r *ClusterSwitchPolicyReconciler) readSecret(ctx context.Context, ref mysqlv1alpha1.SecretRef) (username, password string, err error) {
	ns := ref.Namespace
	if ns == "" {
		ns = "mysql-keeper-system"
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: ref.Name}, secret); err != nil {
		return "", "", fmt.Errorf("get Secret %s/%s: %w", ns, ref.Name, err)
	}
	u, ok := secret.Data["username"]
	if !ok {
		return "", "", fmt.Errorf("Secret %s/%s missing 'username' key", ns, ref.Name)
	}
	p, ok := secret.Data["password"]
	if !ok {
		return "", "", fmt.Errorf("Secret %s/%s missing 'password' key", ns, ref.Name)
	}
	return string(u), string(p), nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterSwitchPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mysqlv1alpha1.ClusterSwitchPolicy{}).
		Complete(r)
}
