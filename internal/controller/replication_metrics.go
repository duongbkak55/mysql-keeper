// Per-reconcile metrics pump for replication-channel health and GTID drift.
//
// These metrics do NOT drive failover decisions — the decision flow reads the
// underlying state directly. They exist so alerts can fire before the flip is
// even attempted: "IO thread down for 2m", "GTID drift > 30s", etc.
package controller

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

// observeReplicationMetrics queries replication_connection_status and
// replication_applier_status for both clusters and exports the 0/1 running
// gauges. Called once per reconcile from Reconcile(); failures are logged
// via logr but never surfaced up as errors because metrics must not block.
func (r *ClusterSwitchPolicyReconciler) observeReplicationMetrics(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
) {
	localChannel := policy.Spec.ReplicationChannelName
	remoteChannel := policy.Spec.PeerReplicationChannelName
	if remoteChannel == "" {
		remoteChannel = localChannel
	}
	if localChannel == "" {
		return
	}
	role := policy.Spec.ClusterRole

	observe := func(inspector switchover.ReplicationInspector, scope, channel string) {
		if inspector == nil {
			return
		}
		status, err := inspector.GetReplicationStatus(ctx, channel)
		if err != nil || !status.ConfigExists {
			// Absence of a configured row is legitimate on the source side.
			// Record 0 so the gauge still has a data point rather than a hole.
			metrics.ReplicationChannelIORunning.WithLabelValues(role, scope, channel).Set(0)
			metrics.ReplicationChannelSQLRunning.WithLabelValues(role, scope, channel).Set(0)
			return
		}
		ioVal := 0.0
		if status.IOServiceState == "ON" && status.IOErrorNumber == 0 {
			ioVal = 1
		}
		sqlVal := 0.0
		if status.SQLServiceState == "ON" && status.SQLErrorNumber == 0 {
			sqlVal = 1
		}
		metrics.ReplicationChannelIORunning.WithLabelValues(role, scope, channel).Set(ioVal)
		metrics.ReplicationChannelSQLRunning.WithLabelValues(role, scope, channel).Set(sqlVal)

		snap, err := inspector.GetGTIDSnapshot(ctx)
		if err == nil {
			metrics.BinlogExpireLogsSeconds.WithLabelValues(role, scope).Set(
				float64(snap.BinlogExpireLogsSeconds))
		}
	}

	observe(comps.localInspector, "local", localChannel)
	observe(comps.remoteInspector, "remote", remoteChannel)
}

// lagQuerier is satisfied by *pxc.Manager. It is intentionally local so we
// avoid a breaking change to switchover.ReplicationInspector — callers that
// don't implement it (e.g. mano.PXCManager) simply skip the lag-seconds metric.
type lagQuerier interface {
	GetReplicationLagSeconds(ctx context.Context, channel string) (int64, error)
}

// observeGTIDLag measures the GTID transaction gap between the source (local)
// and replica (remote) clusters. It exports GTIDMissingTransactions and
// ReplicationLagSeconds Prometheus metrics and returns the measurement so the
// caller can persist it on the CR status and fire Warning events.
//
// Returns measured=false when prerequisites are absent (no channel name,
// missing inspectors, or GTID queries fail). This is non-fatal — the caller
// should skip the status update and event in that case.
func (r *ClusterSwitchPolicyReconciler) observeGTIDLag(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
) (missing, lagSec int64, measured bool) {
	localChannel := policy.Spec.ReplicationChannelName
	remoteChannel := policy.Spec.PeerReplicationChannelName
	if remoteChannel == "" {
		remoteChannel = localChannel
	}
	if localChannel == "" || comps.localInspector == nil || comps.remoteInspector == nil {
		return 0, -1, false
	}

	logger := ctrl.LoggerFrom(ctx)
	role := policy.Spec.ClusterRole

	// Step 1: snapshot the source (local) GTID set.
	localGTID, err := comps.localInspector.GetExecutedGTID(ctx)
	if err != nil {
		logger.V(1).Info("gtid lag: GetExecutedGTID on local failed", "err", err)
		return 0, -1, false
	}

	// Step 2: compute what the replica is missing.
	missingSet, err := comps.remoteInspector.MissingGTIDs(ctx, localGTID)
	if err != nil {
		logger.V(1).Info("gtid lag: MissingGTIDs on remote failed", "err", err)
		return 0, -1, false
	}
	missing = pxc.CountGTIDTransactions(missingSet)
	metrics.GTIDMissingTransactions.WithLabelValues(role).Set(float64(missing))

	// Step 3: replication lag in seconds — only available when the remote
	// inspector is *pxc.Manager (direct MySQL), not via MANO.
	lagSec = -1
	if lq, ok := comps.remoteInspector.(lagQuerier); ok {
		if s, err := lq.GetReplicationLagSeconds(ctx, remoteChannel); err == nil {
			lagSec = s
		} else {
			logger.V(1).Info("gtid lag: GetReplicationLagSeconds failed", "err", err)
		}
	}
	if lagSec >= 0 {
		metrics.ReplicationLagSeconds.WithLabelValues(role, remoteChannel).Set(float64(lagSec))
	}

	return missing, lagSec, true
}

// gtidLagStatusFor builds the GTIDLagStatus value to persist on the CR.
func gtidLagStatusFor(missing, lagSec int64) *mysqlv1alpha1.GTIDLagStatus {
	return &mysqlv1alpha1.GTIDLagStatus{
		MissingTransactions: missing,
		LagSeconds:          lagSec,
		MeasuredAt:          metav1.Now(),
	}
}
