// Package controller implements the ClusterSwitchPolicy reconciler.
package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
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
// +kubebuilder:rbac:groups=pxc.percona.com,resources=perconaxtradbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
type ClusterSwitchPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile is the main reconciliation loop entry point.
// It is triggered by:
//   - Changes to ClusterSwitchPolicy resources.
//   - Periodic requeue (health check interval).
func (r *ClusterSwitchPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the ClusterSwitchPolicy resource.
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Initialize status phase if not set.
	if policy.Status.Phase == "" {
		patch := client.MergeFrom(policy.DeepCopy())
		policy.Status.Phase = mysqlv1alpha1.PhaseInitializing
		if err := r.Status().Patch(ctx, policy, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Build checkers and managers.
	localChecker, remoteChecker, localPXCMgr, remotePXCMgr, localProxySQLMgr, err :=
		r.buildComponents(ctx, policy)
	if err != nil {
		logger.Error(err, "Failed to build health check components")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Run health checks.
	localHealth := localChecker.Check(ctx)
	remoteHealth := remoteChecker.Check(ctx)

	// Update metrics.
	r.updateMetrics(policy, localHealth, remoteHealth)

	// Update failure counters.
	patch := client.MergeFrom(policy.DeepCopy())
	r.updateHealthStatus(policy, localHealth, remoteHealth)

	shouldSwitchover := false
	switchoverReason := ""

	// Check for manual switchover trigger.
	if policy.Spec.ManualSwitchoverTarget == "promote-remote" {
		shouldSwitchover = true
		switchoverReason = "manual switchover requested via spec.manualSwitchoverTarget"
		logger.Info("Manual switchover triggered")
	}

	// Check for automatic failover condition.
	if !shouldSwitchover && policy.Spec.AutoFailover {
		if policy.Status.ConsecutiveLocalFailures >= policy.Spec.HealthCheck.FailureThreshold {
			switch remoteHealth.Writable {
			case health.WritableNo:
				// Remote is reachable and read-only — safe to promote.
				shouldSwitchover = true
				switchoverReason = fmt.Sprintf(
					"automatic failover: local cluster unhealthy for %d consecutive checks",
					policy.Status.ConsecutiveLocalFailures,
				)
				logger.Info("Automatic failover condition met", "reason", switchoverReason)

			case health.WritableYes:
				// Remote is already writable — split-brain or already failed over by DR controller.
				logger.Info("Local cluster unhealthy but remote is already writable — no action (split-brain guard)")

			case health.WritableUnknown:
				// Remote is unreachable — we cannot promote it and cannot safely assume its state.
				// Do NOT failover: promoting an unreachable cluster would cause split-brain if it recovers.
				logger.Info("Local cluster unhealthy but remote is UNREACHABLE — cannot failover safely",
					"consecutiveLocalFailures", policy.Status.ConsecutiveLocalFailures,
					"remoteMessage", remoteHealth.Message,
				)
			}
		}
	}

	if shouldSwitchover && policy.Status.Phase != mysqlv1alpha1.PhaseSwitchingOver {
		// Execute switchover.
		policy.Status.Phase = mysqlv1alpha1.PhaseSwitchingOver
		if err := r.Status().Patch(ctx, policy, patch); err != nil {
			return ctrl.Result{}, err
		}

		result := r.executeSwitchover(ctx, policy, localPXCMgr, remotePXCMgr, localProxySQLMgr, switchoverReason)

		patch2 := client.MergeFrom(policy.DeepCopy())
		if result.Success {
			now := metav1.Now()
			policy.Status.LastSwitchoverTime = &now
			policy.Status.LastSwitchoverReason = switchoverReason
			policy.Status.Phase = mysqlv1alpha1.PhaseMonitoring
			policy.Status.ConsecutiveLocalFailures = 0

			// Flip the active cluster.
			if policy.Status.ActiveCluster == mysqlv1alpha1.ClusterRoleDC {
				policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDR
			} else {
				policy.Status.ActiveCluster = mysqlv1alpha1.ClusterRoleDC
			}

			// Clear manual target.
			if policy.Spec.ManualSwitchoverTarget != "" {
				specPatch := client.MergeFrom(policy.DeepCopy())
				policy.Spec.ManualSwitchoverTarget = ""
				if err := r.Patch(ctx, policy, specPatch); err != nil {
					logger.Error(err, "Failed to clear manualSwitchoverTarget")
				}
			}

			metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "success").Inc()
		} else {
			policy.Status.Phase = mysqlv1alpha1.PhaseDegraded
			logger.Error(result.Error, "Switchover failed",
				"phase", result.FailedPhase.String(),
				"rolledBack", result.RolledBack,
			)

			if result.RolledBack {
				metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "rollback").Inc()
			} else {
				metrics.SwitchoverTotal.WithLabelValues(policy.Spec.ClusterRole, "failure").Inc()
			}
		}

		if err := r.Status().Patch(ctx, policy, patch2); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		if policy.Status.Phase != mysqlv1alpha1.PhaseSwitchingOver {
			patch3 := client.MergeFrom(policy.DeepCopy())
			policy.Status.Phase = mysqlv1alpha1.PhaseMonitoring
			if err := r.Status().Patch(ctx, policy, patch3); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Requeue after the configured health check interval.
	return ctrl.Result{RequeueAfter: policy.Spec.HealthCheck.Interval.Duration}, nil
}

// buildComponents constructs all the health checkers and managers from the policy spec.
func (r *ClusterSwitchPolicyReconciler) buildComponents(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) (
	localChecker *health.Checker,
	remoteChecker *health.Checker,
	localPXCMgr switchover.PXCManager,
	remotePXCMgr switchover.PXCManager,
	localProxySQLMgr *proxysql.Manager,
	err error,
) {
	timeout := policy.Spec.HealthCheck.MySQLCheckTimeout.Duration

	// Read local MySQL credentials.
	localUser, localPass, err := r.readSecret(ctx, policy.Spec.LocalMySQL.CredentialsSecretRef)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("local MySQL credentials: %w", err)
	}
	localDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		localUser, localPass,
		policy.Spec.LocalMySQL.Host, policy.Spec.LocalMySQL.Port,
	)

	// Read remote MySQL credentials.
	remoteUser, remotePass, err := r.readSecret(ctx, policy.Spec.RemoteMySQL.CredentialsSecretRef)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("remote MySQL credentials: %w", err)
	}
	remoteDSN := fmt.Sprintf("%s:%s@tcp(%s:%d)/",
		remoteUser, remotePass,
		policy.Spec.RemoteMySQL.Host, policy.Spec.RemoteMySQL.Port,
	)

	// Build PXC health checkers.
	localPXCChecker := health.NewLocalPXCChecker(
		r.Client,
		policy.Spec.PXCNamespace,
		policy.Spec.PXCName,
		localDSN,
		timeout,
	)
	remotePXCChecker := health.NewRemotePXCChecker(remoteDSN, timeout)

	// Build ProxySQL health checker.
	proxySQLEndpoints := make([]health.ProxySQLEndpointInfo, 0, len(policy.Spec.ProxySQL))
	proxySQLMgrEndpoints := make([]proxysql.Endpoint, 0, len(policy.Spec.ProxySQL))

	for _, ep := range policy.Spec.ProxySQL {
		u, p, err := r.readSecret(ctx, ep.CredentialsSecretRef)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("ProxySQL creds for %s: %w", ep.Host, err)
		}
		proxySQLEndpoints = append(proxySQLEndpoints, health.ProxySQLEndpointInfo{
			Host: ep.Host, Port: ep.AdminPort, Username: u, Password: p,
		})
		proxySQLMgrEndpoints = append(proxySQLMgrEndpoints, proxysql.Endpoint{
			Host: ep.Host, Port: ep.AdminPort, Username: u, Password: p,
		})
	}

	proxySQLChecker := health.NewProxySQLChecker(proxySQLEndpoints, timeout)

	localChecker = health.NewLocalChecker(localPXCChecker, proxySQLChecker, policy.Spec.HealthCheck.ProxySQLMinHealthy)
	remoteChecker = health.NewRemoteChecker(remotePXCChecker)

	// If MANO is configured, all state changes (both DC and DR) go through the MANO API.
	// mano.PXCManager uses MANO for SetReadOnly/SetReadWrite and direct MySQL for IsWritable.
	if policy.Spec.MANO != nil {
		manoClient, err := r.buildMANOClient(ctx, policy.Spec.MANO)
		if err != nil {
			return nil, nil, nil, nil, nil, fmt.Errorf("build MANO client: %w", err)
		}
		pollInterval := policy.Spec.MANO.PollInterval.Duration
		if pollInterval == 0 {
			pollInterval = 5 * time.Second
		}
		pollTimeout := policy.Spec.MANO.PollTimeout.Duration
		if pollTimeout == 0 {
			pollTimeout = 5 * time.Minute
		}
		localPXCMgr = mano.NewPXCManager(manoClient,
			policy.Spec.MANO.LocalCNFName, policy.Spec.MANO.LocalVDUName,
			pollInterval, pollTimeout,
			localDSN, timeout,
		)
		remotePXCMgr = mano.NewPXCManager(manoClient,
			policy.Spec.MANO.RemoteCNFName, policy.Spec.MANO.RemoteVDUName,
			pollInterval, pollTimeout,
			remoteDSN, timeout,
		)
	} else {
		// No MANO: local uses direct k8s API, remote uses optional remote k8s API or SQL-only.
		localPXCMgr = pxc.NewManager(localDSN, timeout, r.Client,
			policy.Spec.PXCNamespace, policy.Spec.PXCName, policy.Spec.ReplicationChannelName)

		if policy.Spec.RemoteKubeAPI != nil {
			remoteKubeClient, err := r.buildRemoteKubeClient(ctx, policy.Spec.RemoteKubeAPI)
			if err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("build remote k8s client: %w", err)
			}
			remotePXCMgr = pxc.NewRemoteManagerWithKubeAPI(remoteDSN, timeout,
				remoteKubeClient,
				policy.Spec.RemoteKubeAPI.PXCNamespace,
				policy.Spec.RemoteKubeAPI.PXCName,
				policy.Spec.ReplicationChannelName,
			)
		} else {
			// SQL only. Remote controller self-corrects its CRD on its next reconcile.
			remotePXCMgr = pxc.NewRemoteManager(remoteDSN, timeout)
		}
	}

	localProxySQLMgr = proxysql.NewManager(proxySQLMgrEndpoints, timeout)

	return localChecker, remoteChecker, localPXCMgr, remotePXCMgr, localProxySQLMgr, nil
}

// executeSwitchover builds and runs the switchover engine.
func (r *ClusterSwitchPolicyReconciler) executeSwitchover(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localPXC switchover.PXCManager,
	remotePXC switchover.PXCManager,
	localProxySQL *proxysql.Manager,
	reason string,
) switchover.Result {
	sw := policy.Spec.Switchover
	engine := switchover.NewEngine(switchover.Config{
		LocalPXC:      localPXC,
		RemotePXC:     remotePXC,
		LocalProxySQL: localProxySQL,
		Routing: proxysql.RoutingConfig{
			OldWriterHost:      sw.LocalWriterHost,
			OldWriterPort:      3306,
			NewWriterHost:      sw.RemoteWriterHost,
			NewWriterPort:      sw.RemoteWriterPort,
			ReadWriteHostgroup: sw.ReadWriteHostgroup,
			ReadOnlyHostgroup:  sw.ReadOnlyHostgroup,
		},
		DrainTimeout: sw.DrainTimeout.Duration,
		FenceTimeout: sw.FenceTimeout.Duration,
		Reason:       reason,
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

// updateHealthStatus updates the status counters and health snapshots on the policy.
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

	if localH.Healthy {
		policy.Status.ConsecutiveLocalFailures = 0
	} else {
		policy.Status.ConsecutiveLocalFailures++
	}

	if remoteH.Healthy {
		policy.Status.ConsecutiveRemoteFailures = 0
	} else {
		policy.Status.ConsecutiveRemoteFailures++
	}

	// Infer activeCluster on first run from writable state.
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
// It reads the CA cert and bearer token from local Secrets, then builds a rest.Config.
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
		// No CA provided — skip TLS verification (acceptable for internal networks).
		restCfg.TLSClientConfig = rest.TLSClientConfig{Insecure: true}
	}

	// Build a scheme that includes the PXC CRD types for patching.
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
