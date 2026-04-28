package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=csp
// +kubebuilder:printcolumn:name="Role",type=string,JSONPath=`.spec.clusterRole`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeCluster`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterSwitchPolicy configures and tracks DC-DR MySQL automatic switchover.
// Deploy one instance per Kubernetes cluster (DC and DR each get their own CR).
type ClusterSwitchPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSwitchPolicySpec   `json:"spec"`
	Status ClusterSwitchPolicyStatus `json:"status,omitempty"`
}

// ClusterSwitchPolicySpec defines the desired configuration for the switchover controller.
type ClusterSwitchPolicySpec struct {
	// ClusterRole defines whether this controller instance manages the "dc" or "dr" cluster.
	// +kubebuilder:validation:Enum=dc;dr
	ClusterRole string `json:"clusterRole"`

	// PXCNamespace is the namespace where the local PerconaXtraDBCluster resource lives.
	PXCNamespace string `json:"pxcNamespace"`

	// PXCName is the name of the local PerconaXtraDBCluster resource.
	PXCName string `json:"pxcName"`

	// ReplicationChannelName is the name of the channel that terminates ON
	// THIS cluster when this cluster is acting as a replica. It is the
	// channel the controller will STOP + RESET when this cluster becomes
	// the new source during a flip, and the channel the controller will
	// (re)activate when this cluster is demoted back to replica.
	//
	// For symmetric setups (same channel name both directions) this field
	// alone is enough; leave PeerReplicationChannelName unset and the
	// controller will use this value for both sides.
	//
	// Must match the channel name configured in the PerconaXtraDBCluster
	// CRD on both sites.
	ReplicationChannelName string `json:"replicationChannelName"`

	// PeerReplicationChannelName is the name of the channel that
	// terminates ON THE PEER cluster when the peer is acting as a replica.
	// Set this when DC→DR and DR→DC use different channel names
	// (e.g. "dc-to-dr" inbound on DR vs "dr-to-dc" inbound on DC).
	//
	// Empty means "same as ReplicationChannelName" — the symmetric case.
	//
	// Which channel the controller uses at each step:
	//   - PreFlight C3 (is remote replica healthy?) → PeerReplicationChannelName
	//   - Promote (STOP+RESET on new source, which was the remote replica)
	//     → PeerReplicationChannelName
	//   - ReverseReplica (STOP on former source, which is now the local
	//     cluster preparing to replicate back) → ReplicationChannelName
	// +optional
	PeerReplicationChannelName string `json:"peerReplicationChannelName,omitempty"`

	// LocalMySQL lists direct MySQL endpoints for the local PXC cluster nodes.
	// Used for health checks and read_only toggle on the local cluster.
	LocalMySQL MySQLEndpointConfig `json:"localMySQL"`

	// RemoteMySQL is the MySQL endpoint of the remote cluster (cross-cluster reachable).
	// Used for health monitoring and promoting/fencing the remote cluster.
	RemoteMySQL MySQLEndpointConfig `json:"remoteMySQL"`

	// ProxySQL lists the local ProxySQL admin endpoints (static list).
	// Use for StatefulSet-based ProxySQL with stable pod DNS names.
	// Mutually exclusive with ProxySQLSelector; if both are set ProxySQLSelector takes precedence.
	// +optional
	ProxySQL []ProxySQLEndpoint `json:"proxySQL,omitempty"`

	// ProxySQLSelector discovers ProxySQL admin endpoints dynamically at each
	// reconcile by listing Pods matching the given label selector. Use this when
	// ProxySQL is deployed as a Deployment (pod IPs change on heal/restart).
	// Mutually exclusive with ProxySQL.
	// +optional
	ProxySQLSelector *ProxySQLSelectorConfig `json:"proxySQLSelector,omitempty"`

	// HealthCheck defines thresholds and intervals for health monitoring.
	HealthCheck HealthCheckConfig `json:"healthCheck"`

	// Switchover defines parameters for the switchover operation.
	Switchover SwitchoverConfig `json:"switchover"`

	// PreFlight carries knobs that govern the preflight checklist executed
	// before a switchover is allowed to proceed. Defaults are production-safe.
	// +optional
	PreFlight PreFlightConfig `json:"preFlight,omitempty"`

	// AutoFailover enables automatic promotion when the local cluster fails.
	// When true, this controller will promote the remote cluster if local fails.
	// +kubebuilder:default=true
	AutoFailover bool `json:"autoFailover"`

	// AllowDataLossFailover is an explicit opt-in for promoting a replica when
	// the local cluster is destroyed and the preflight GTID subset check
	// therefore cannot run. The operator must understand that some recent
	// writes on the former source may be lost.
	// +kubebuilder:default=false
	// +optional
	AllowDataLossFailover bool `json:"allowDataLossFailover,omitempty"`

	// MANO configures the MANO (Management and Orchestration) API used to toggle
	// isSource on both the local and remote PerconaXtraDBCluster CNFs.
	// When provided, isSource changes are applied via the MANO LCM API instead of
	// direct k8s API calls. MANO then reconciles the CRD and enforces MySQL state.
	// Optional: if omitted, falls back to direct k8s API (RemoteKubeAPI) or SQL only.
	// +optional
	MANO *MANOConfig `json:"mano,omitempty"`

	// RemoteKubeAPI configures access to the remote cluster's Kubernetes API server.
	// When provided, the local controller also patches the remote PerconaXtraDBCluster CRD
	// (isSource field) directly after SQL state changes, preventing the remote PXC operator
	// from fighting our state change on its next reconcile cycle.
	// Optional: if omitted, only SQL-based state changes are made to the remote cluster.
	// +optional
	RemoteKubeAPI *RemoteKubeAPIConfig `json:"remoteKubeAPI,omitempty"`

	// ManualSwitchoverTarget triggers a planned switchover when set to "promote-remote".
	// The controller clears this field after the switchover completes.
	// +kubebuilder:validation:Enum="";promote-remote
	// +optional
	ManualSwitchoverTarget string `json:"manualSwitchoverTarget,omitempty"`
}

// MANOConfig configures the MANO LCM API for toggling isSource on PXC CNFs.
type MANOConfig struct {
	// Host is the MANO API base URL (e.g. "https://mano.example.com").
	Host string `json:"host"`

	// TokenSecretRef refers to a Secret with a "token" key containing a static Bearer token.
	// Mutually exclusive with CredentialsSecretRef. If both are set, CredentialsSecretRef takes precedence.
	// +optional
	TokenSecretRef *SecretRef `json:"tokenSecretRef,omitempty"`

	// CredentialsSecretRef refers to a Secret with "username" and "password" keys for MANO login.
	// The client calls POST /users/auth to obtain and auto-refresh Bearer tokens.
	// Mutually exclusive with TokenSecretRef.
	// +optional
	CredentialsSecretRef *SecretRef `json:"credentialsSecretRef,omitempty"`

	// LocalCNFName is the CNF name of the local PerconaXtraDBCluster in MANO.
	LocalCNFName string `json:"localCnfName"`

	// LocalVDUName is the VDU name of the local PerconaXtraDBCluster in MANO.
	LocalVDUName string `json:"localVduName"`

	// RemoteCNFName is the CNF name of the remote PerconaXtraDBCluster in MANO.
	RemoteCNFName string `json:"remoteCnfName"`

	// RemoteVDUName is the VDU name of the remote PerconaXtraDBCluster in MANO.
	RemoteVDUName string `json:"remoteVduName"`

	// PollInterval is how often to poll lcm-op-occ for the operation result.
	// +kubebuilder:default="5s"
	// +optional
	PollInterval metav1.Duration `json:"pollInterval,omitempty"`

	// PollTimeout is the maximum time to wait for a MANO LCM operation to complete.
	// +kubebuilder:default="5m"
	// +optional
	PollTimeout metav1.Duration `json:"pollTimeout,omitempty"`
}

// RemoteKubeAPIConfig holds credentials to reach the remote cluster's k8s API server.
// Used to patch the remote PerconaXtraDBCluster CRD (isSource) during switchover.
type RemoteKubeAPIConfig struct {
	// Host is the remote k8s API server URL (e.g. "https://10.10.0.1:6443").
	Host string `json:"host"`

	// CASecretRef refers to a Secret with a "ca.crt" key containing the TLS CA
	// certificate for the remote k8s API server. Omit to skip TLS verification.
	// +optional
	CASecretRef *SecretRef `json:"caSecretRef,omitempty"`

	// TokenSecretRef refers to a Secret with a "token" key containing a
	// ServiceAccount bearer token that has patch/update permissions on
	// PerconaXtraDBCluster resources in the remote cluster.
	TokenSecretRef SecretRef `json:"tokenSecretRef"`

	// PXCNamespace is the namespace of the remote PerconaXtraDBCluster resource.
	PXCNamespace string `json:"pxcNamespace"`

	// PXCName is the name of the remote PerconaXtraDBCluster resource.
	PXCName string `json:"pxcName"`
}

// MySQLEndpointConfig holds connection info for a MySQL cluster endpoint.
type MySQLEndpointConfig struct {
	// Host is the hostname or IP of the MySQL endpoint (VIP, LoadBalancer, or HAProxy).
	Host string `json:"host"`

	// Port is the MySQL port. Defaults to 3306.
	// +kubebuilder:default=3306
	Port int32 `json:"port"`

	// CredentialsSecretRef refers to a Secret with "username" and "password" keys.
	// The user must have SUPER (or SYSTEM_VARIABLES_ADMIN) privilege.
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
}

// ProxySQLSelectorConfig discovers ProxySQL pod endpoints dynamically by
// listing Pods that match a label selector. The controller queries the k8s API
// at each reconcile cycle so endpoints are always current regardless of pod
// restarts or IP changes caused by Deployment healing.
type ProxySQLSelectorConfig struct {
	// Namespace where the ProxySQL pods run.
	Namespace string `json:"namespace"`

	// MatchLabels is a map of key=value pairs that ProxySQL pods must carry.
	MatchLabels map[string]string `json:"matchLabels"`

	// AdminPort is the ProxySQL admin interface port. Default 6032.
	// +kubebuilder:default=6032
	// +optional
	AdminPort int32 `json:"adminPort,omitempty"`

	// CredentialsSecretRef refers to a Secret with "username" and "password" for ProxySQL admin.
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
}

// ProxySQLEndpoint defines a single ProxySQL admin interface.
//
// The controller talks to ProxySQL on its admin port (default 6032) to:
//   - health-check the instance (SELECT 1)
//   - rewrite hostgroup routing during a switchover (UPDATE mysql_servers
//     + LOAD MYSQL SERVERS TO RUNTIME + SAVE MYSQL SERVERS TO DISK)
//   - apply the blackhole fence when the SQL fence cannot complete
//     (OFFLINE_HARD + max_connections=0 on the target writer)
//
// The admin port is separate from the client port that applications use
// (default 6033) — do not confuse the two in Service definitions.
type ProxySQLEndpoint struct {
	// Host is the DNS name or IP of the ProxySQL instance.
	Host string `json:"host"`

	// AdminPort is the ProxySQL admin interface port. Default 6032.
	// +kubebuilder:default=6032
	// +optional
	AdminPort int32 `json:"adminPort,omitempty"`

	// CredentialsSecretRef refers to a Secret with "username" and "password" for ProxySQL admin.
	CredentialsSecretRef SecretRef `json:"credentialsSecretRef"`
}

// SecretRef identifies a Kubernetes Secret.
type SecretRef struct {
	// Name of the Secret.
	Name string `json:"name"`

	// Namespace of the Secret. Defaults to the controller's namespace if omitted.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// HealthCheckConfig defines health monitoring thresholds.
type HealthCheckConfig struct {
	// Interval between consecutive health check cycles.
	// +kubebuilder:default="15s"
	Interval metav1.Duration `json:"interval"`

	// FailureThreshold is the number of consecutive failed checks before
	// a cluster is declared unhealthy and failover is considered.
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold"`

	// MySQLCheckTimeout is the per-query timeout for direct MySQL health checks.
	// +kubebuilder:default="5s"
	MySQLCheckTimeout metav1.Duration `json:"mysqlCheckTimeout"`

	// ProxySQLMinHealthy is the minimum number of local ProxySQL instances
	// that must be reachable to consider ProxySQL healthy.
	// +kubebuilder:default=2
	ProxySQLMinHealthy int32 `json:"proxySQLMinHealthy"`

	// GTIDLagAlertThresholdTransactions, when positive, causes the controller
	// to emit a Warning Kubernetes event on the CR whenever the number of
	// unapplied GTID transactions on the replica exceeds this value.
	// 0 (default) disables the event. Use this for an early warning before
	// replication lag grows to the point where preflight C5/C6 would block
	// a switchover.
	// +kubebuilder:default=0
	// +optional
	GTIDLagAlertThresholdTransactions int64 `json:"gtidLagAlertThresholdTransactions,omitempty"`

	// RemoteUnreachableThreshold, when positive, enables an additional
	// auto-promotion path: if the remote cluster has been TCP-unreachable for
	// this many consecutive health-check cycles the controller promotes the
	// local cluster to active.
	//
	// This path is INDEPENDENT of FailureThreshold (which triggers on LOCAL
	// failure). Use it to let the passive DR site take over when the active DC
	// disappears entirely — e.g. DC k8s + MySQL both down.
	//
	// IMPORTANT: AllowDataLossFailover must also be true. Because the remote
	// cannot be queried, GTID subset verification (preflight C5/C6) is
	// impossible. Set this threshold only after accepting that risk.
	//
	// Example: interval=15s, threshold=10 → promote after ~2.5 min of silence.
	// 0 (default) disables this path entirely.
	// +kubebuilder:default=0
	// +optional
	RemoteUnreachableThreshold int32 `json:"remoteUnreachableThreshold,omitempty"`
}

// SwitchoverConfig defines switchover behavior and ProxySQL hostgroup IDs.
type SwitchoverConfig struct {
	// Timeout is the maximum duration for a complete switchover before rollback.
	// +kubebuilder:default="5m"
	Timeout metav1.Duration `json:"timeout"`

	// DrainTimeout is how long to wait for existing connections to drain on the
	// local (being demoted) cluster before proceeding with fencing.
	// +kubebuilder:default="30s"
	DrainTimeout metav1.Duration `json:"drainTimeout"`

	// FenceTimeout is the timeout for the fencing (set read_only=ON) operation.
	// +kubebuilder:default="10s"
	FenceTimeout metav1.Duration `json:"fenceTimeout"`

	// CooldownPeriod is the minimum interval between two successive switchovers.
	// Protects against ping-pong when a transient incident causes both sides to
	// flap. Default 10 minutes.
	// +kubebuilder:default="10m"
	// +optional
	CooldownPeriod metav1.Duration `json:"cooldownPeriod,omitempty"`

	// ResumeStuckTimeout is how long a "SwitchingOver" phase may remain in the
	// middle of a checkpoint before the controller abandons it and transitions
	// to Degraded. Default 10 minutes.
	// +kubebuilder:default="10m"
	// +optional
	ResumeStuckTimeout metav1.Duration `json:"resumeStuckTimeout,omitempty"`

	// BlackholeHostgroup is the ProxySQL hostgroup used by the alternate fence
	// path when the SQL fence fails and the local cluster is unreachable.
	// Default 9999 (an ID no query rules route to).
	// +kubebuilder:default=9999
	// +optional
	BlackholeHostgroup int32 `json:"blackholeHostgroup,omitempty"`

	// ReadWriteHostgroup is the ProxySQL hostgroup ID for write traffic.
	// +kubebuilder:default=10
	ReadWriteHostgroup int32 `json:"readWriteHostgroup"`

	// ReadOnlyHostgroup is the ProxySQL hostgroup ID for read traffic.
	// +kubebuilder:default=20
	ReadOnlyHostgroup int32 `json:"readOnlyHostgroup"`

	// RemoteWriterHost is the MySQL host (of the remote cluster) that ProxySQL should
	// route write traffic to after failover. This should be the remote cluster's
	// VIP or LoadBalancer address reachable from this cluster's ProxySQL instances.
	RemoteWriterHost string `json:"remoteWriterHost"`

	// RemoteWriterPort is the MySQL port for the remote writer.
	// +kubebuilder:default=3306
	RemoteWriterPort int32 `json:"remoteWriterPort"`

	// LocalWriterHost is the MySQL host of this cluster's PXC (used to identify
	// the current writer entry in ProxySQL to be demoted).
	LocalWriterHost string `json:"localWriterHost"`
}

// ClusterSwitchPolicyStatus reflects the observed state of the controller.
type ClusterSwitchPolicyStatus struct {
	// ActiveCluster indicates which cluster currently holds write role: "dc" or "dr".
	// +kubebuilder:validation:Enum=dc;dr
	// +optional
	ActiveCluster string `json:"activeCluster,omitempty"`

	// Phase is the current high-level operation state.
	// +kubebuilder:validation:Enum=Initializing;Monitoring;SwitchingOver;RollingBack;Degraded;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// LocalHealth holds the last observed health of the local cluster.
	LocalHealth ClusterHealthStatus `json:"localHealth,omitempty"`

	// RemoteHealth holds the last observed health of the remote cluster.
	RemoteHealth ClusterHealthStatus `json:"remoteHealth,omitempty"`

	// ConsecutiveLocalFailures tracks consecutive health check failures for the local cluster.
	ConsecutiveLocalFailures int32 `json:"consecutiveLocalFailures,omitempty"`

	// ConsecutiveRemoteFailures tracks consecutive health check failures for the remote cluster.
	ConsecutiveRemoteFailures int32 `json:"consecutiveRemoteFailures,omitempty"`

	// ConsecutiveLocalUnreachable counts how many successive health check cycles
	// were unable to reach the local cluster at all (vs. reached but unhealthy).
	// Unreachability alone must not trigger a failover; the counter is kept
	// separately so operators can alert on this independently.
	// +optional
	ConsecutiveLocalUnreachable int32 `json:"consecutiveLocalUnreachable,omitempty"`

	// ConsecutiveRemoteUnreachable counts how many successive health check
	// cycles were unable to reach the remote cluster at all (TCP-level failure).
	// When this counter reaches spec.healthCheck.remoteUnreachableThreshold and
	// AllowDataLossFailover=true, the controller promotes the local cluster.
	// +optional
	ConsecutiveRemoteUnreachable int32 `json:"consecutiveRemoteUnreachable,omitempty"`

	// LastSwitchoverTime is when the last successful switchover completed.
	// +optional
	LastSwitchoverTime *metav1.Time `json:"lastSwitchoverTime,omitempty"`

	// LastSwitchoverReason is a human-readable description of the last switchover trigger.
	// +optional
	LastSwitchoverReason string `json:"lastSwitchoverReason,omitempty"`

	// SwitchoverProgress is set while Phase == SwitchingOver and describes how
	// far the engine has progressed. Used for checkpoint/resume after pod
	// restart and for operator-facing diagnostics.
	// +optional
	SwitchoverProgress *SwitchoverProgress `json:"switchoverProgress,omitempty"`

	// LastPreFlight holds the checklist result of the most recent switchover
	// attempt (successful or not), so operators can see which checks passed
	// and which blocked the flip without reading controller logs.
	// +optional
	LastPreFlight *PreFlightStatus `json:"lastPreFlight,omitempty"`

	// Conditions provides detailed status conditions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// GTIDLag is the most recent replication-lag measurement between the
	// source cluster and its replica. Updated every reconcile when a
	// replication channel is configured. Use this for quick visibility via
	// "kubectl describe" without reading Prometheus.
	// +optional
	GTIDLag *GTIDLagStatus `json:"gtidLag,omitempty"`

	// ObservedGeneration is the last spec generation the controller processed.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// SwitchoverProgress is a checkpoint written after every phase transition so
// the reconciler can resume after a pod restart instead of leaving the CR
// stuck in PhaseSwitchingOver forever.
type SwitchoverProgress struct {
	// AttemptID is a UUID per switchover attempt. Correlates log lines,
	// metrics, and CR events for the same attempt.
	AttemptID string `json:"attemptID"`

	// StartedAt is when the engine was invoked for this attempt.
	StartedAt metav1.Time `json:"startedAt"`

	// CurrentPhase is the step the engine is currently executing.
	// +kubebuilder:validation:Enum=PreFlight;Fence;Promote;Routing;ReverseReplica;Verify;Done
	CurrentPhase string `json:"currentPhase"`

	// CompletedPhases lists phases that were fully applied, in order.
	// +optional
	CompletedPhases []string `json:"completedPhases,omitempty"`

	// FailedPhase is set when the attempt aborted. Empty on success.
	// +optional
	FailedPhase string `json:"failedPhase,omitempty"`

	// Reason is the trigger that caused this attempt (manual, auto, resume).
	// +optional
	Reason string `json:"reason,omitempty"`

	// Error is the first error surfaced by the failed phase, for operator
	// visibility without grepping logs.
	// +optional
	Error string `json:"error,omitempty"`
}

// PreFlightStatus mirrors switchover.PreFlightResult on the CR.
type PreFlightStatus struct {
	// PassedAt is when this preflight snapshot was collected.
	PassedAt metav1.Time `json:"passedAt"`

	// Checks captures each C-level check result.
	// +optional
	Checks []PreFlightCheck `json:"checks,omitempty"`

	// LocalGTIDExecuted / RemoteGTIDExecuted snapshot the GTID positions that
	// drove the subset / catch-up decisions.
	// +optional
	LocalGTIDExecuted string `json:"localGTIDExecuted,omitempty"`
	// +optional
	RemoteGTIDExecuted string `json:"remoteGTIDExecuted,omitempty"`
}

// PreFlightCheck is a single row from the preflight checklist.
type PreFlightCheck struct {
	Name     string `json:"name"`
	Severity string `json:"severity"` // "hard" | "soft"
	Passed   bool   `json:"passed"`
	Message  string `json:"message,omitempty"`
}

// PreFlightConfig tunes the preflight checklist behavior.
type PreFlightConfig struct {
	// CatchupTimeout is how long the preflight may wait for the replica to
	// apply every GTID the source has. Default 30 seconds.
	// +kubebuilder:default="30s"
	// +optional
	CatchupTimeout metav1.Duration `json:"catchupTimeout,omitempty"`

	// MinBinlogRetentionSeconds is the minimum binlog_expire_logs_seconds
	// accepted on both sides. The C11 check surfaces a soft warning when this
	// threshold is not met. Default 604800 (7 days).
	// +kubebuilder:default=604800
	// +optional
	MinBinlogRetentionSeconds int64 `json:"minBinlogRetentionSeconds,omitempty"`
}

// GTIDLagStatus captures a point-in-time GTID replication-lag measurement
// between the source cluster and its replica.
type GTIDLagStatus struct {
	// MissingTransactions is the number of GTID transactions present on the
	// source that the replica has not yet applied (GTID_SUBTRACT count).
	MissingTransactions int64 `json:"missingTransactions"`

	// LagSeconds is the estimated lag derived from the ORIGINAL_COMMIT_TIMESTAMP
	// of the last applied transaction on the replica. -1 means no data was
	// available (channel idle, not yet configured, or no transactions replicated).
	LagSeconds int64 `json:"lagSeconds"`

	// MeasuredAt is when this measurement was collected.
	MeasuredAt metav1.Time `json:"measuredAt"`

	// LocalGTIDExecuted is the raw @@GLOBAL.gtid_executed string from the
	// local (source) cluster at the time of this measurement. Refreshed every
	// health-check interval so operators can compare it directly against a live
	// MySQL query without waiting for a switchover.
	// +optional
	LocalGTIDExecuted string `json:"localGTIDExecuted,omitempty"`

	// RemoteGTIDExecuted is the GTID set the remote (replica) cluster is still
	// missing from the source, expressed as GTID_SUBTRACT(local, remote).
	// An empty string means the replica is fully caught up.
	// +optional
	RemoteGTIDMissing string `json:"remoteGTIDMissing,omitempty"`
}

// ClusterHealthStatus holds the observed health of one cluster.
//
// Every field here is optional at the CRD level because status is populated
// progressively by the controller — on a freshly-created CR the status
// subresource starts empty, and partial patches land as the various health
// probes complete at different times. Marking the bool fields required
// would reject every transitional state (e.g., "we have a measurement of
// `healthy` but not yet of `isWritable`") with an admission error rather
// than allowing the next reconcile to fill it in.
type ClusterHealthStatus struct {
	// Healthy is the aggregated health determination.
	// +optional
	Healthy bool `json:"healthy,omitempty"`

	// PXCState is the state from the PerconaXtraDBCluster CRD (e.g. "ready", "error").
	// +optional
	PXCState string `json:"pxcState,omitempty"`

	// PXCReadyNodes is the number of PXC pod members currently ready.
	// +optional
	PXCReadyNodes int32 `json:"pxcReadyNodes,omitempty"`

	// WsrepClusterSize is the reported Galera cluster size.
	// +optional
	WsrepClusterSize int32 `json:"wsrepClusterSize,omitempty"`

	// WsrepClusterStatus is the reported Galera status string (e.g. "Primary").
	// +optional
	WsrepClusterStatus string `json:"wsrepClusterStatus,omitempty"`

	// IsWritable reports whether the cluster currently has read_only=OFF.
	// +optional
	IsWritable bool `json:"isWritable,omitempty"`

	// ProxySQLHealthy is the number of local ProxySQL instances that responded.
	// +optional
	ProxySQLHealthy int32 `json:"proxySQLHealthy,omitempty"`

	// LastChecked is when this health snapshot was collected.
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`

	// Message contains additional diagnostic information.
	// +optional
	Message string `json:"message,omitempty"`
}

// Phase constants.
const (
	PhaseInitializing  = "Initializing"
	PhaseMonitoring    = "Monitoring"
	PhaseSwitchingOver = "SwitchingOver"
	PhaseRollingBack   = "RollingBack"
	PhaseDegraded      = "Degraded"
	PhaseError         = "Error"
)

// Condition type constants.
const (
	ConditionLocalHealthy        = "LocalClusterHealthy"
	ConditionRemoteHealthy       = "RemoteClusterHealthy"
	ConditionSwitchoverInProgress = "SwitchoverInProgress"
	ConditionSplitBrainSafe      = "SplitBrainSafe"
)

// ClusterRole constants.
const (
	ClusterRoleDC = "dc"
	ClusterRoleDR = "dr"
)

// +kubebuilder:object:root=true

// ClusterSwitchPolicyList contains a list of ClusterSwitchPolicy.
type ClusterSwitchPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterSwitchPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterSwitchPolicy{}, &ClusterSwitchPolicyList{})
}
