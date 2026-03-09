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
)

func init() {
	metrics.Registry.MustRegister(
		ClusterHealthy,
		ClusterWritable,
		ConsecutiveFailures,
		SwitchoverTotal,
		SwitchoverDurationSeconds,
		ProxySQLHealthyInstances,
	)
}
