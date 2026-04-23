//go:build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
)

// namespace is where every CSP in the suite is created. A single namespace
// keeps the RBAC story simple and matches the default installed by
// config/default.
const namespace = "mysql-keeper-system"

// newClient returns a controller-runtime client using whatever kubeconfig the
// environment advertises. The scheme knows only the types we need to touch,
// plus the core types that come in by default.
func newClient(t *testing.T) client.Client {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		if home, err := os.UserHomeDir(); err == nil {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig from %s: %v", kubeconfig, err)
	}

	// SetupSignalHandler registers signal channels internally via a sync.Once
	// that panics on second call. e2e tests create a client per test, so we
	// must NOT call it here. A test-scoped context is built via
	// context.WithTimeout in the individual tests instead.
	scheme := runtime.NewScheme()
	if err := mysqlv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add CSP scheme: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return c
}

// fixtureCSP returns the minimal CSP the e2e controller can reconcile. Both
// local and remote point at the mysql-stub service because the controller's
// job here is bookkeeping, not real switchover.
func fixtureCSP(name string) *mysqlv1alpha1.ClusterSwitchPolicy {
	return &mysqlv1alpha1.ClusterSwitchPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ClusterRole:            mysqlv1alpha1.ClusterRoleDC,
			PXCNamespace:           "mysql-keeper-e2e",
			PXCName:                "mysql-stub",
			ReplicationChannelName: "dc-to-dr",
			LocalMySQL: mysqlv1alpha1.MySQLEndpointConfig{
				Host: "mysql-stub.mysql-keeper-e2e.svc.cluster.local",
				Port: 3306,
				CredentialsSecretRef: mysqlv1alpha1.SecretRef{
					Name:      "mysql-stub-creds",
					Namespace: "mysql-keeper-e2e",
				},
			},
			RemoteMySQL: mysqlv1alpha1.MySQLEndpointConfig{
				Host: "mysql-stub.mysql-keeper-e2e.svc.cluster.local",
				Port: 3306,
				CredentialsSecretRef: mysqlv1alpha1.SecretRef{
					Name:      "mysql-stub-creds",
					Namespace: "mysql-keeper-e2e",
				},
			},
			// ProxySQL is a required CRD field. The e2e tests don't exercise
			// ProxySQL routing, so we hand in a single placeholder endpoint
			// pointing at nowhere-reachable — the controller's ProxySQL
			// health check will report it unreachable but the reconciler
			// does not crash on that.
			ProxySQL: []mysqlv1alpha1.ProxySQLEndpoint{
				{
					Host:      "proxysql-stub.mysql-keeper-e2e.svc.cluster.local",
					AdminPort: 6032,
					CredentialsSecretRef: mysqlv1alpha1.SecretRef{
						Name:      "mysql-stub-creds",
						Namespace: "mysql-keeper-e2e",
					},
				},
			},
			HealthCheck: mysqlv1alpha1.HealthCheckConfig{
				Interval:           metav1.Duration{Duration: 5 * time.Second},
				FailureThreshold:   3,
				MySQLCheckTimeout:  metav1.Duration{Duration: 2 * time.Second},
				ProxySQLMinHealthy: 0, // no ProxySQL in the stub setup
			},
			Switchover: mysqlv1alpha1.SwitchoverConfig{
				Timeout:            metav1.Duration{Duration: time.Minute},
				DrainTimeout:       metav1.Duration{Duration: 2 * time.Second},
				FenceTimeout:       metav1.Duration{Duration: 5 * time.Second},
				CooldownPeriod:     metav1.Duration{Duration: 30 * time.Second},
				ResumeStuckTimeout: metav1.Duration{Duration: 30 * time.Second},
				ReadWriteHostgroup: 10,
				ReadOnlyHostgroup:  20,
				BlackholeHostgroup: 9999,
				RemoteWriterHost:   "mysql-stub.mysql-keeper-e2e.svc.cluster.local",
				RemoteWriterPort:   3306,
				LocalWriterHost:    "mysql-stub.mysql-keeper-e2e.svc.cluster.local",
			},
			AutoFailover: false, // e2e tests avoid auto-flip; they drive status directly
		},
	}
}

// mustCreate creates obj and fails the test if the create call errors.
func mustCreate(t *testing.T, ctx context.Context, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
	}
}

// mustGet fetches the CSP by name into out; fails the test on error.
func mustGet(t *testing.T, ctx context.Context, c client.Client, name string, out *mysqlv1alpha1.ClusterSwitchPolicy) {
	t.Helper()
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, out); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
}

// waitForFinalizer polls the CSP until the controller has attached the
// keeper finalizer. Used by every test that wants to exercise the delete
// path.
func waitForFinalizer(t *testing.T, ctx context.Context, c client.Client, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		csp := &mysqlv1alpha1.ClusterSwitchPolicy{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, csp); err == nil {
			if containsString(csp.Finalizers, "mysql.keeper.io/finalizer") {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("finalizer was not attached on %s within %s", name, timeout)
}

// removeFinalizerAndDelete is the test-teardown counterpart to the finalizer
// we deliberately leave blocking deletion during TestE2E_FinalizerBlocksDelete.
// Run in t.Cleanup so even a failing test does not leak objects.
func removeFinalizerAndDelete(t *testing.T, c client.Client, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	csp := &mysqlv1alpha1.ClusterSwitchPolicy{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, csp); err != nil {
		return
	}
	if len(csp.Finalizers) > 0 {
		patch := client.MergeFrom(csp.DeepCopy())
		csp.Finalizers = nil
		_ = c.Patch(ctx, csp, patch)
	}
	_ = c.Delete(ctx, csp)
}

// containsString is a tiny helper to avoid importing slices just for one check.
func containsString(ss []string, needle string) bool {
	for _, s := range ss {
		if s == needle {
			return true
		}
	}
	return false
}

// shortUUID returns eight hex characters — enough entropy for test names
// that need to be unique across runs but short enough to be readable.
func shortUUID(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(buf)
}

// kubectl shells out rather than bringing in a full apps/v1 client. Any
// failure is fatal to the test.
func kubectl(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = testWriter{t}
	cmd.Stderr = testWriter{t}
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl %v: %v", args, err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// metricsURL returns the URL the test uses to scrape the controller's
// metrics endpoint. We rely on the NodePort 30080 exposed by the kind
// cluster config — see kind-config.yaml.
func metricsURL(t *testing.T) string {
	t.Helper()
	if override := os.Getenv("E2E_METRICS_URL"); override != "" {
		return override
	}
	return "http://127.0.0.1:30080/metrics"
}

// decodeYAML is a small convenience wrapper around yaml.NewYAMLOrJSONDecoder
// so tests can embed manifest text if needed; currently unused but kept here
// to avoid reshaping the file when future tests do need it.
var _ = yaml.NewYAMLOrJSONDecoder
