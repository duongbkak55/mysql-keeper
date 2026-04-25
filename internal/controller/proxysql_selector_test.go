package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
)

func newSelectorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	return s
}

func credSecret(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			"username": []byte("admin"),
			"password": []byte("secret"),
		},
	}
}

func runningReadyPod(name, ns, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: ip,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

const (
	selNS     = "percona-xtradb"
	credsNS   = "mysql-keeper-system"
	credsName = "proxysql-creds"
)

var matchLabels = map[string]string{"app": "proxysql", "site": "dc"}

func newSelectorReconciler(t *testing.T, objs ...runtime.Object) *ClusterSwitchPolicyReconciler {
	t.Helper()
	s := newSelectorScheme(t)
	b := fake.NewClientBuilder().WithScheme(s)
	for _, o := range objs {
		b = b.WithRuntimeObjects(o)
	}
	return &ClusterSwitchPolicyReconciler{Client: b.Build(), Scheme: s}
}

func defaultSelectorCfg() *mysqlv1alpha1.ProxySQLSelectorConfig {
	return &mysqlv1alpha1.ProxySQLSelectorConfig{
		Namespace:   selNS,
		MatchLabels: matchLabels,
		AdminPort:   6032,
		CredentialsSecretRef: mysqlv1alpha1.SecretRef{
			Name:      credsName,
			Namespace: credsNS,
		},
	}
}

// --- resolveProxySQLBySelector tests ---

func TestResolveBySelector_RunningReadyPodsReturned(t *testing.T) {
	pod1 := runningReadyPod("proxysql-abc", selNS, "10.0.0.1", matchLabels)
	pod2 := runningReadyPod("proxysql-def", selNS, "10.0.0.2", matchLabels)
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), pod1, pod2)

	mgrEps, healthEps, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 2 {
		t.Fatalf("expected 2 manager endpoints, got %d", len(mgrEps))
	}
	if len(healthEps) != 2 {
		t.Fatalf("expected 2 health endpoints, got %d", len(healthEps))
	}
	// Verify credentials and port are propagated.
	for _, ep := range mgrEps {
		if ep.Port != 6032 {
			t.Errorf("expected port 6032, got %d", ep.Port)
		}
		if ep.Username != "admin" {
			t.Errorf("expected username admin, got %s", ep.Username)
		}
	}
}

func TestResolveBySelector_PendingPodExcluded(t *testing.T) {
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "proxysql-pending", Namespace: selNS, Labels: matchLabels},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), pending)

	mgrEps, _, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 0 {
		t.Fatalf("expected 0 endpoints (pending pod filtered), got %d", len(mgrEps))
	}
}

func TestResolveBySelector_NotReadyPodExcluded(t *testing.T) {
	notReady := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "proxysql-notready", Namespace: selNS, Labels: matchLabels},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse},
			},
		},
	}
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), notReady)

	mgrEps, _, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 0 {
		t.Fatalf("expected 0 endpoints (not-Ready pod filtered), got %d", len(mgrEps))
	}
}

func TestResolveBySelector_NoIPPodExcluded(t *testing.T) {
	noIP := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "proxysql-noip", Namespace: selNS, Labels: matchLabels},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "", // no IP yet
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), noIP)

	mgrEps, _, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 0 {
		t.Fatalf("expected 0 endpoints (no-IP pod filtered), got %d", len(mgrEps))
	}
}

func TestResolveBySelector_WrongLabelPodExcluded(t *testing.T) {
	wrongLabel := runningReadyPod("other-app", selNS, "10.0.0.1", map[string]string{"app": "mysql"})
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), wrongLabel)

	mgrEps, _, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 0 {
		t.Fatalf("expected 0 endpoints (wrong-label pod filtered by k8s), got %d", len(mgrEps))
	}
}

func TestResolveBySelector_ZeroMatchesReturnsEmptyNoError(t *testing.T) {
	// Creds exist but no pods at all.
	r := newSelectorReconciler(t, credSecret(credsName, credsNS))

	mgrEps, healthEps, err := r.resolveProxySQLBySelector(context.Background(), defaultSelectorCfg())
	if err != nil {
		t.Fatalf("unexpected error when no pods match: %v", err)
	}
	if mgrEps != nil || healthEps != nil {
		t.Fatalf("expected nil slices for zero-match, got mgrEps=%v healthEps=%v", mgrEps, healthEps)
	}
}

func TestResolveBySelector_DefaultPortWhenZero(t *testing.T) {
	pod := runningReadyPod("proxysql-abc", selNS, "10.0.0.1", matchLabels)
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), pod)

	cfg := defaultSelectorCfg()
	cfg.AdminPort = 0 // should default to 6032

	mgrEps, _, err := r.resolveProxySQLBySelector(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(mgrEps))
	}
	if mgrEps[0].Port != 6032 {
		t.Errorf("expected default port 6032, got %d", mgrEps[0].Port)
	}
}

// --- resolveProxySQLEndpoints routing tests ---

func TestResolveEndpoints_BothNilReturnsError(t *testing.T) {
	r := newSelectorReconciler(t)
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{}
	// Both ProxySQL and ProxySQLSelector are zero-value (nil/empty).

	_, _, err := r.resolveProxySQLEndpoints(context.Background(), policy)
	if err == nil {
		t.Fatal("expected error when both proxySQL and proxySQLSelector are unset")
	}
}

func TestResolveEndpoints_RoutesToSelectorWhenSet(t *testing.T) {
	pod := runningReadyPod("proxysql-abc", selNS, "10.0.0.5", matchLabels)
	r := newSelectorReconciler(t, credSecret(credsName, credsNS), pod)

	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ProxySQLSelector: defaultSelectorCfg(),
		},
	}

	mgrEps, healthEps, err := r.resolveProxySQLEndpoints(context.Background(), policy)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgrEps) != 1 || mgrEps[0].Host != "10.0.0.5" {
		t.Errorf("expected single endpoint 10.0.0.5, got %v", mgrEps)
	}
	if len(healthEps) != 1 || healthEps[0].Host != "10.0.0.5" {
		t.Errorf("expected single health endpoint 10.0.0.5, got %v", healthEps)
	}
}
