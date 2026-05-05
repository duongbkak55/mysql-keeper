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
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/health"
	"github.com/duongnguyen/mysql-keeper/internal/mano"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

// AnnotationRecoverDegraded is patched by operators to trigger an immediate
// health re-evaluation while the policy is in Degraded phase. The controller
// compares the current annotation value against status.lastRecoveryAnnotation;
// any new, non-empty value fires one recovery attempt regardless of
// spec.recovery.autoRecoveryInterval. Use a timestamp or UUID as the value so
// each patch is distinct.
const AnnotationRecoverDegraded = "mysql.keeper.io/recover-degraded"

// ClusterSwitchPolicyReconciler reconciles a ClusterSwitchPolicy object.
// +kubebuilder:rbac:groups=mysql.keeper.io,resources=clusterswitchpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mysql.keeper.io,resources=clusterswitchpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=pxc.percona.com,resources=perconaxtradbclusters,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
type ClusterSwitchPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// schemaEnsured caches which "<policyUID>/<side>" keys have had their
	// keeper schema (keeper.probe + keeper.leader) confirmed on MySQL.
	// Keyed by policy UID + "/local" or "/remote". Avoids sending DDL on
	// every reconcile cycle — the Manager is rebuilt per-reconcile so the
	// guard cannot live there. Populated on first success; cleared on error
	// so the next reconcile retries. Reset to empty on controller restart,
	// which is acceptable: the tables already exist so the single DDL on
	// restart is a cheap IF NOT EXISTS no-op.
	schemaEnsured sync.Map

	// activeReplicationErrorLabels tracks which (policyUID,role,channel,errno)
	// label combinations are currently set to 1 on mysql_keeper_replication_error.
	// Map key: "<policyUID>:<role>:<channel>:<errno>". Used so that a label set
	// in cycle N but absent in cycle N+1 is explicitly reset to 0 — without
	// this, stale "1" gauges would linger forever. Process-local (gauges are too).
	activeReplicationErrorLabels sync.Map
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
		// Register label combinations even on failure so the gauge time-series
		// are always present in /metrics (Prometheus GaugeVec only appears
		// after the first WithLabelValues().Set() call).
		r.updateMetrics(policy, health.ClusterHealth{}, health.ClusterHealth{})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Snapshot cluster health first.
	localHealth := comps.localChecker.Check(ctx)
	remoteHealth := comps.remoteChecker.Check(ctx)

	r.updateMetrics(policy, localHealth, remoteHealth)
	r.observeReplicationMetrics(ctx, policy, comps)
	gtidMissing, gtidLagSec, gtidLocalExecuted, gtidMissingSet, gtidMeasured := r.observeGTIDLag(ctx, policy, comps)
	replErrOutcome := r.observeReplicationErrors(ctx, policy, comps, localHealth, gtidMissing, gtidMeasured)

	// Persist the counters/health derived from this cycle before any further
	// branching. We keep the merge base cloned before we mutate the status so
	// the patch is a pure diff.
	healthPatch := client.MergeFrom(policy.DeepCopy())
	r.updateHealthStatus(policy, localHealth, remoteHealth)
	r.updateSplitBrainCondition(policy, localHealth, remoteHealth)
	if gtidMeasured {
		policy.Status.GTIDLag = gtidLagStatusFor(gtidMissing, gtidLagSec, gtidLocalExecuted, gtidMissingSet)
	}
	updateReplicationErrorStatus(policy, replErrOutcome, time.Now())
	if err := r.Status().Patch(ctx, policy, healthPatch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch health status: %w", err)
	}

	// Emit a Warning event when the replica is falling behind. This shows up
	// in "kubectl describe" and streams to any event watchers. The Prometheus
	// metrics (GTIDMissingTransactions / ReplicationLagSeconds) are the primary
	// alerting surface; the event is an early visual signal for on-call.
	//
	// The newer spec.replicationErrorHandling.gtidGapAlertThreshold supersedes
	// healthCheck.GTIDLagAlertThresholdTransactions: when the new field is
	// non-zero, observeReplicationErrors handles the alarm (event + condition +
	// metric) and we skip this legacy emit to avoid duplicate events for the
	// same incident. Operators can migrate at their own pace; until then this
	// path remains active for back-compat.
	newThreshold := int64(0)
	if reh := policy.Spec.ReplicationErrorHandling; reh != nil {
		newThreshold = reh.GTIDGapAlertThreshold
	}
	if threshold := policy.Spec.HealthCheck.GTIDLagAlertThresholdTransactions; gtidMeasured &&
		r.Recorder != nil && threshold > 0 && newThreshold == 0 && gtidMissing > threshold {
		r.Recorder.Event(policy, corev1.EventTypeWarning, "GTIDLagHigh",
			fmt.Sprintf("GTID lag: %d unapplied transactions exceeds threshold %d — replication falling behind",
				gtidMissing, threshold))
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

	// Degraded phase: only exit via deliberate recovery (annotation change or
	// auto-interval). Bypass the normal "nothing to switch → Monitoring" path so
	// the policy stays Degraded until the operator explicitly acknowledges it or
	// the configured auto-recovery interval elapses and health checks pass.
	if policy.Status.Phase == mysqlv1alpha1.PhaseDegraded {
		return r.handleDegradedRecovery(ctx, policy, localHealth, remoteHealth)
	}

	reason, shouldSwitch := r.decideSwitchover(ctx, policy, comps, localHealth, remoteHealth)
	logger.Info("should_switchover",
		"event", "should_switchover",
		"result", shouldSwitch,
		"reason", reason,
		"localHealthy", localHealth.Healthy,
		"remoteHealthy", remoteHealth.Healthy,
		"localWritable", localHealth.Writable.String(),
		"remoteWritable", remoteHealth.Writable.String(),
		"consecutiveLocalFailures", policy.Status.ConsecutiveLocalFailures,
	)
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

	// If the operator requested a manual switchover while the replica is
	// quarantined, refuse it up-front — do NOT clear the trigger so they
	// don't have to re-set it after releasing quarantine.
	if policy.Spec.ManualSwitchoverTarget != "" && replicaQuarantined(policy) {
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeWarning, "ManualSwitchoverRefused",
				"manual switchover requested but replica is quarantined — clear quarantine first")
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

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
	// Already failed — FailedPhase is recorded, this is not a stuck in-progress
	// operation. Let handleDegradedRecovery own the exit path.
	if prog.FailedPhase != "" {
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

	decision := EvaluateSwitchover(policy, localHealth, remoteHealth, time.Now())
	switch decision.Blocker {
	case "cooldown":
		metrics.CooldownBlocked.WithLabelValues(policy.Spec.ClusterRole).Inc()
	case "both_readonly":
		metrics.BothClustersReadOnly.WithLabelValues(policy.Spec.ClusterRole).Inc()
	}
	if !decision.Should {
		logger.Info("EvaluateSwitchover rejected flip", "reason", decision.Reason)
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

	return decision.Reason, true
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

	// Resolve the two channel names. For symmetric setups (same name both
	// directions), PeerReplicationChannelName is empty and we reuse the
	// primary name.
	localChannel := policy.Spec.ReplicationChannelName
	remoteChannel := policy.Spec.PeerReplicationChannelName
	if remoteChannel == "" {
		remoteChannel = localChannel
	}

	engine := switchover.NewEngine(switchover.Config{
		LocalPXC:                 comps.localPXC,
		RemotePXC:                comps.remotePXC,
		LocalInspector:           comps.localInspector,
		RemoteInspector:          comps.remoteInspector,
		LocalReplication:         comps.localReplication,
		RemoteReplication:        comps.remoteReplication,
		LocalProxySQL:            comps.proxySQLMgr,
		LocalReplicationChannel:  localChannel,
		RemoteReplicationChannel: remoteChannel,
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
		LocalReplicaQuarantined:   replicaQuarantined(policy),
		LocalQuarantineReason:     replicaQuarantineReason(policy),
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
		policy.Status.ConsecutiveRemoteUnreachable = 0

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

// ensureKeeperSchema runs EnsureKeeperSchema + EnsureLeaderLeaseSchema on
// whichever cluster is currently writable, but only once per controller
// lifetime per side. The Manager is rebuilt on every reconcile so the guard
// lives here on the singleton reconciler via schemaEnsured (sync.Map).
//
// Cache key: "<policyUID>/local" or "<policyUID>/remote".
// On DDL error the key is NOT stored so the next reconcile retries.
// On controller restart the cache is empty; the single DDL that fires is a
// cheap IF NOT EXISTS no-op because the tables already exist.
func (r *ClusterSwitchPolicyReconciler) ensureKeeperSchema(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	localHealth, remoteHealth health.ClusterHealth,
) {
	logger := log.FromContext(ctx)
	uid := string(policy.UID)

	if localHealth.Writable == health.WritableYes && comps.localSchemaInit != nil {
		key := uid + "/local"
		if _, done := r.schemaEnsured.Load(key); !done {
			ok := true
			if err := comps.localSchemaInit.EnsureKeeperSchema(ctx); err != nil {
				logger.Error(err, "EnsureKeeperSchema on local writable cluster failed")
				ok = false
			}
			if ok {
				if err := comps.localSchemaInit.EnsureLeaderLeaseSchema(ctx); err != nil {
					logger.Error(err, "EnsureLeaderLeaseSchema on local writable cluster failed")
					ok = false
				}
			}
			if ok {
				r.schemaEnsured.Store(key, struct{}{})
			}
		}
	}

	if remoteHealth.Writable == health.WritableYes && comps.remoteSchemaInit != nil {
		key := uid + "/remote"
		if _, done := r.schemaEnsured.Load(key); !done {
			ok := true
			if err := comps.remoteSchemaInit.EnsureKeeperSchema(ctx); err != nil {
				logger.Error(err, "EnsureKeeperSchema on remote writable cluster failed")
				ok = false
			}
			if ok {
				if err := comps.remoteSchemaInit.EnsureLeaderLeaseSchema(ctx); err != nil {
					logger.Error(err, "EnsureLeaderLeaseSchema on remote writable cluster failed")
					ok = false
				}
			}
			if ok {
				r.schemaEnsured.Store(key, struct{}{})
			}
		}
	}
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

	switch {
	case remoteH.Healthy:
		policy.Status.ConsecutiveRemoteFailures = 0
		policy.Status.ConsecutiveRemoteUnreachable = 0
	case remoteH.Writable == health.WritableUnknown:
		// TCP-level unreachable: drives the remoteUnreachableThreshold gate.
		policy.Status.ConsecutiveRemoteUnreachable++
	default:
		// Reachable but unhealthy (replication broken, quorum lost, etc.)
		policy.Status.ConsecutiveRemoteFailures++
		policy.Status.ConsecutiveRemoteUnreachable = 0
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

	// Self-correct activeCluster when the local cluster is writable but the
	// status still points at the remote. This happens when the peer controller
	// executed a successful switchover that this controller was not involved in
	// (e.g. DC controller failed over to DR; DR controller's CR never updated).
	if localH.Writable == health.WritableYes &&
		policy.Status.ActiveCluster != "" &&
		policy.Status.ActiveCluster != policy.Spec.ClusterRole {
		policy.Status.ActiveCluster = policy.Spec.ClusterRole
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
	metrics.ConsecutiveFailures.WithLabelValues(role, "local_unreachable").Set(
		float64(policy.Status.ConsecutiveLocalUnreachable),
	)
	metrics.ConsecutiveFailures.WithLabelValues(role, "remote").Set(
		float64(policy.Status.ConsecutiveRemoteFailures),
	)
	metrics.ConsecutiveFailures.WithLabelValues(role, "remote_unreachable").Set(
		float64(policy.Status.ConsecutiveRemoteUnreachable),
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
	// parseTime=true is required so go-sql-driver returns DATETIME columns
	// as time.Time. Without it, Scan into *time.Time (keeper.leader.
	// acquired_at, heartbeat_at) fails with "Scan error on column index N,
	// ... []uint8 into type *time.Time" at runtime.
	localDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/?parseTime=true&loc=Local",
		localUser, localPass,
		policy.Spec.LocalMySQL.Host, policy.Spec.LocalMySQL.Port,
	)

	remoteUser, remotePass, err := r.readSecret(ctx, policy.Spec.RemoteMySQL.CredentialsSecretRef)
	if err != nil {
		return nil, fmt.Errorf("remote MySQL credentials: %w", err)
	}
	remoteDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/?parseTime=true&loc=Local",
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

	proxySQLMgrEndpoints, proxySQLEndpoints, err := r.resolveProxySQLEndpoints(ctx, policy)
	if err != nil {
		return nil, err
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
		policy.Spec.PXCNamespace, policy.Spec.PXCName, policy.Spec.ReplicationChannelName,
		int(policy.Spec.Switchover.CRDApplyRetries))
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
				int(policy.Spec.Switchover.CRDApplyRetries),
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
		return mano.NewClientWithCredentials(cfg.Host, username, password, cfg.TLSInsecureSkipVerify), nil
	}
	if cfg.TokenSecretRef != nil {
		token, err := r.readSecretKey(ctx, *cfg.TokenSecretRef, "token")
		if err != nil {
			return nil, fmt.Errorf("read MANO token: %w", err)
		}
		return mano.NewClient(cfg.Host, token, cfg.TLSInsecureSkipVerify), nil
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

// handleDegradedRecovery is the sole exit path from Degraded phase. It fires
// when either:
//   - the mysql.keeper.io/recover-degraded annotation value has changed (manual
//     one-shot trigger — patch any new non-empty string value to invoke), or
//   - spec.recovery.autoRecoveryInterval > 0 and enough time has elapsed since
//     the last attempt.
//
// Recovery succeeds when both clusters are healthy and exactly one is writable.
// On success the phase transitions to Monitoring and SwitchoverProgress is
// cleared. On failure the policy stays Degraded and the next attempt time is
// scheduled via the returned Result.
func (r *ClusterSwitchPolicyReconciler) handleDegradedRecovery(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localHealth, remoteHealth health.ClusterHealth,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	annotationVal := policy.Annotations[AnnotationRecoverDegraded]
	annotationChanged := annotationVal != "" && annotationVal != policy.Status.LastRecoveryAnnotation

	autoInterval := policy.Spec.Recovery.AutoRecoveryInterval.Duration
	autoReady := autoInterval > 0 && (policy.Status.LastDegradedRecoveryAttempt == nil ||
		time.Since(policy.Status.LastDegradedRecoveryAttempt.Time) >= autoInterval)

	if !annotationChanged && !autoReady {
		requeueAfter := policy.Spec.HealthCheck.Interval.Duration
		if autoInterval > 0 && autoInterval < requeueAfter {
			requeueAfter = autoInterval
		}
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	trigger := "auto"
	if annotationChanged {
		trigger = "annotation"
	}
	logger.Info("Evaluating degraded recovery",
		"trigger", trigger,
		"localHealthy", localHealth.Healthy,
		"remoteHealthy", remoteHealth.Healthy,
		"localWritable", localHealth.Writable.String(),
		"remoteWritable", remoteHealth.Writable.String(),
	)

	now := metav1.Now()
	patch := client.MergeFrom(policy.DeepCopy())
	policy.Status.LastDegradedRecoveryAttempt = &now
	if annotationChanged {
		policy.Status.LastRecoveryAnnotation = annotationVal
	}

	bothHealthy := localHealth.Healthy && remoteHealth.Healthy
	exactlyOneWritable := (localHealth.Writable == health.WritableYes) != (remoteHealth.Writable == health.WritableYes)

	if bothHealthy && exactlyOneWritable {
		logger.Info("Degraded recovery succeeded — transitioning to Monitoring", "trigger", trigger)
		policy.Status.Phase = mysqlv1alpha1.PhaseMonitoring
		policy.Status.SwitchoverProgress = nil
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeNormal, "DegradedRecovery",
				fmt.Sprintf("cluster health restored (%s trigger) — returned to Monitoring", trigger))
		}
	} else {
		logger.Info("Degraded recovery: cluster not yet healthy, staying Degraded", "trigger", trigger)
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeWarning, "DegradedRecoveryFailed",
				fmt.Sprintf("recovery check (%s trigger): local_healthy=%v remote_healthy=%v localWritable=%s remoteWritable=%s — staying Degraded",
					trigger, localHealth.Healthy, remoteHealth.Healthy,
					localHealth.Writable.String(), remoteHealth.Writable.String()))
		}
	}

	if err := r.Status().Patch(ctx, policy, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status during degraded recovery: %w", err)
	}

	requeueAfter := policy.Spec.HealthCheck.Interval.Duration
	if policy.Status.Phase == mysqlv1alpha1.PhaseDegraded && autoInterval > 0 && autoInterval < requeueAfter {
		requeueAfter = autoInterval
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterSwitchPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Filter watch events so that status-subresource patches (which do not
	// bump .metadata.generation and do not change annotations) do not
	// re-trigger an immediate reconcile. Without this filter every
	// Status().Patch() call causes a watch event → new reconcile → new
	// Status().Patch(), creating a tight loop that fires far faster than
	// spec.healthCheck.interval.
	//
	// GenerationChangedPredicate passes spec changes (generation bump).
	// AnnotationChangedPredicate passes annotation writes, which is how
	// the recover-degraded manual trigger is delivered.
	// RequeueAfter in the reconcile return drives the periodic cadence.
	p := predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.AnnotationChangedPredicate{},
	)
	return ctrl.NewControllerManagedBy(mgr).
		For(&mysqlv1alpha1.ClusterSwitchPolicy{}, builder.WithPredicates(p)).
		Complete(r)
}

// resolveProxySQLEndpoints returns the ProxySQL endpoint lists for the manager
// and health checker. When proxySQLSelector is set it discovers pods dynamically
// via the k8s API; otherwise it falls back to the static proxySQL list.
// Returns an error if neither is configured or if pod listing fails.
func (r *ClusterSwitchPolicyReconciler) resolveProxySQLEndpoints(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) ([]proxysql.Endpoint, []health.ProxySQLEndpointInfo, error) {
	if policy.Spec.ProxySQLSelector != nil {
		return r.resolveProxySQLBySelector(ctx, policy.Spec.ProxySQLSelector)
	}
	if len(policy.Spec.ProxySQL) == 0 {
		return nil, nil, fmt.Errorf("spec.proxySQL and spec.proxySQLSelector are both unset; at least one is required")
	}
	var mgrEps []proxysql.Endpoint
	var healthEps []health.ProxySQLEndpointInfo
	for _, ep := range policy.Spec.ProxySQL {
		u, p, err := r.readSecret(ctx, ep.CredentialsSecretRef)
		if err != nil {
			return nil, nil, fmt.Errorf("ProxySQL creds for %s: %w", ep.Host, err)
		}
		port := ep.AdminPort
		if port == 0 {
			port = 6032
		}
		mgrEps = append(mgrEps, proxysql.Endpoint{Host: ep.Host, Port: port, Username: u, Password: p})
		healthEps = append(healthEps, health.ProxySQLEndpointInfo{Host: ep.Host, Port: port, Username: u, Password: p})
	}
	return mgrEps, healthEps, nil
}

// resolveProxySQLBySelector lists all Running+Ready pods matching the selector
// and builds endpoint slices from their pod IPs. Called at every reconcile so
// endpoints are always fresh after Deployment pod restarts or IP changes.
func (r *ClusterSwitchPolicyReconciler) resolveProxySQLBySelector(
	ctx context.Context,
	cfg *mysqlv1alpha1.ProxySQLSelectorConfig,
) ([]proxysql.Endpoint, []health.ProxySQLEndpointInfo, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(cfg.Namespace),
		client.MatchingLabels(cfg.MatchLabels),
	); err != nil {
		return nil, nil, fmt.Errorf("list ProxySQL pods (selector=%v): %w", cfg.MatchLabels, err)
	}

	u, p, err := r.readSecret(ctx, cfg.CredentialsSecretRef)
	if err != nil {
		return nil, nil, fmt.Errorf("ProxySQL selector creds: %w", err)
	}
	port := cfg.AdminPort
	if port == 0 {
		port = 6032
	}

	var mgrEps []proxysql.Endpoint
	var healthEps []health.ProxySQLEndpointInfo
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		if !proxySQLPodReady(pod) {
			continue
		}
		mgrEps = append(mgrEps, proxysql.Endpoint{Host: pod.Status.PodIP, Port: port, Username: u, Password: p})
		healthEps = append(healthEps, health.ProxySQLEndpointInfo{Host: pod.Status.PodIP, Port: port, Username: u, Password: p})
	}
	return mgrEps, healthEps, nil
}

// proxySQLPodReady returns true when the pod's Ready condition is True.
func proxySQLPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
