package health

import "time"

// WritableState is a tri-state for a cluster's read/write status.
// A plain bool is ambiguous: false could mean "read-only (safe)" or "unreachable (unsafe)".
type WritableState int

const (
	// WritableUnknown: cluster is unreachable — writable state cannot be determined.
	// It is NOT safe to promote this cluster or treat it as demoted.
	WritableUnknown WritableState = iota

	// WritableNo: cluster is reachable and has read_only=ON.
	// Safe to use as a failover target (can be promoted).
	WritableNo

	// WritableYes: cluster is reachable and has read_only=OFF (currently primary).
	WritableYes
)

func (w WritableState) String() string {
	switch w {
	case WritableNo:
		return "ReadOnly"
	case WritableYes:
		return "ReadWrite"
	default:
		return "Unknown"
	}
}

// ClusterHealth holds the aggregated health result for one MySQL cluster.
type ClusterHealth struct {
	// Healthy is true when the cluster passes all configured health checks.
	Healthy bool

	// PXCState is the state from PerconaXtraDBCluster CRD (local cluster only).
	PXCState string

	// PXCReadyNodes is the number of PXC pods currently ready (local only).
	PXCReadyNodes int32

	// WsrepClusterSize is the Galera cluster size reported by wsrep status.
	WsrepClusterSize int32

	// WsrepClusterStatus is the wsrep_cluster_status value (e.g. "Primary").
	WsrepClusterStatus string

	// Writable is the tri-state writable status.
	// WritableUnknown = unreachable, WritableNo = read-only, WritableYes = writable.
	Writable WritableState

	// IsWritable is true only when Writable == WritableYes.
	// Kept for convenience — do NOT use alone to decide failover safety.
	// Always check Healthy or Writable != WritableUnknown first.
	IsWritable bool

	// ProxySQLHealthy is the count of responsive ProxySQL admin ports (local only).
	ProxySQLHealthy int32

	// CheckedAt is when this health snapshot was taken.
	CheckedAt time.Time

	// Message holds a human-readable diagnostic string, usually set on failure.
	Message string
}

// CheckResult is the outcome of a single health check probe.
type CheckResult struct {
	Name    string
	Healthy bool
	Message string
}
