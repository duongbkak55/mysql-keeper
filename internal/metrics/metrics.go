// Package metrics registers Prometheus metrics for mysql-keeper.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ClusterHealthy tracks whether each cluster is currently healthy.
	// Labels: cluster_role ("dc"|"dr"), scope ("local"|"remote")
	ClusterHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "cluster_healthy",
			Help:      "1 if the cluster is healthy, 0 otherwise.",
		},
		[]string{"cluster_role", "scope"},
	)

	// ClusterWritable tracks whether each cluster currently has read_only=OFF.
	ClusterWritable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "cluster_writable",
			Help:      "1 if the cluster is writable (read_only=OFF), 0 otherwise.",
		},
		[]string{"cluster_role", "scope"},
	)

	// ConsecutiveFailures tracks consecutive health check failures per cluster.
	ConsecutiveFailures = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "consecutive_failures",
			Help:      "Number of consecutive health check failures for a cluster.",
		},
		[]string{"cluster_role", "scope"},
	)

	// SwitchoverTotal counts total switchover attempts.
	// Labels: cluster_role, result ("success"|"failure"|"rollback")
	SwitchoverTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "switchover_total",
			Help:      "Total number of switchover attempts.",
		},
		[]string{"cluster_role", "result"},
	)

	// SwitchoverDurationSeconds observes switchover durations.
	SwitchoverDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "mysql_keeper",
			Name:      "switchover_duration_seconds",
			Help:      "Duration of switchover operations in seconds.",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300},
		},
		[]string{"cluster_role", "result"},
	)

	// ProxySQLHealthyInstances tracks the count of healthy ProxySQL instances.
	ProxySQLHealthyInstances = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "proxysql_healthy_instances",
			Help:      "Number of ProxySQL instances currently reachable.",
		},
		[]string{"cluster_role"},
	)

	// BothClustersReadOnly counts events where both DC and DR were observed
	// read_only=ON simultaneously — a cluster-wide incident that must NOT
	// trigger automatic failover.
	BothClustersReadOnly = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "both_clusters_readonly_total",
			Help:      "Number of reconcile cycles where both clusters were observed ReadOnly.",
		},
		[]string{"cluster_role"},
	)

	// BothClustersWritable counts events where both clusters are read_only=OFF.
	// This is the split-brain writer condition and should page oncall.
	BothClustersWritable = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "both_clusters_writable_total",
			Help:      "Number of reconcile cycles where both clusters were observed writable (split-brain risk).",
		},
		[]string{"cluster_role"},
	)

	// CooldownBlocked counts how often a switchover was suppressed because the
	// cooldown window had not elapsed.
	CooldownBlocked = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "cooldown_block_total",
			Help:      "Number of times a switchover was suppressed by cooldown.",
		},
		[]string{"cluster_role"},
	)

	// PreFlightFailures counts per-phase preflight or execution failures so
	// operators can see which checkpoint is failing most often.
	PreFlightFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "preflight_failures_total",
			Help:      "Number of switchover attempts that failed at a given phase.",
		},
		[]string{"cluster_role", "phase"},
	)

	// FenceFailures counts failures of a fencing step by path (sql / proxysql).
	FenceFailures = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "fence_failures_total",
			Help:      "Number of fence attempts that failed, by path.",
		},
		[]string{"cluster_role", "path"},
	)

	// ManualInterventionRequired is bumped each time the controller marks the
	// CR Degraded and expects a human to unstick it.
	ManualInterventionRequired = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "mysql_keeper",
			Name:      "manual_intervention_required_total",
			Help:      "Number of times the controller gave up on a switchover and expects manual intervention.",
		},
		[]string{"cluster_role", "phase"},
	)

	// ReplicationChannelIORunning is 1 when the IO thread is ON and has no
	// last-error, 0 otherwise. Populated by the Sprint 3 metrics pump.
	ReplicationChannelIORunning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "replication_channel_io_running",
			Help:      "1 if the IO thread of the replication channel is running, 0 otherwise.",
		},
		[]string{"cluster_role", "scope", "channel"},
	)

	// ReplicationChannelSQLRunning is 1 when the applier is running, 0 otherwise.
	ReplicationChannelSQLRunning = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "replication_channel_sql_running",
			Help:      "1 if the SQL applier of the replication channel is running, 0 otherwise.",
		},
		[]string{"cluster_role", "scope", "channel"},
	)

	// BinlogExpireLogsSeconds is the configured retention window on each node.
	BinlogExpireLogsSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "mysql_keeper",
			Name:      "binlog_expire_logs_seconds",
			Help:      "Current binlog_expire_logs_seconds on the cluster.",
		},
		[]string{"cluster_role", "scope"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ClusterHealthy,
		ClusterWritable,
		ConsecutiveFailures,
		SwitchoverTotal,
		SwitchoverDurationSeconds,
		ProxySQLHealthyInstances,
		BothClustersReadOnly,
		BothClustersWritable,
		CooldownBlocked,
		PreFlightFailures,
		FenceFailures,
		ManualInterventionRequired,
		ReplicationChannelIORunning,
		ReplicationChannelSQLRunning,
		BinlogExpireLogsSeconds,
	)
}
