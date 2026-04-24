// Per-reconcile metrics pump for replication-channel health and GTID drift.
//
// These metrics do NOT drive failover decisions — the decision flow reads the
// underlying state directly. They exist so alerts can fire before the flip is
// even attempted: "IO thread down for 2m", "GTID drift > 30s", etc.
package controller

import (
	"context"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
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
