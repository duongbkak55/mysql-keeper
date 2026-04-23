//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
)

// TestE2E_SwitchoverProgressCheckpointSurvivesRestart covers S6 / S7 / 4.6.
// We write a fake in-flight switchover checkpoint to the CR status, restart
// the controller deployment, and assert the checkpoint is still observable
// after the new pod starts reconciling.
//
// We do NOT assert that the reconciler resumes — the current 0.2 release
// deliberately abandons stuck attempts via ResumeStuckTimeout (tracked for
// 0.3). This test only asserts that the progress bookkeeping is durable,
// i.e. we won't silently forget an attempt across pod restarts.
func TestE2E_SwitchoverProgressCheckpointSurvivesRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	c := newClient(t)
	name := "e2e-checkpoint-" + shortUUID(t)

	csp := fixtureCSP(name)
	mustCreate(t, ctx, c, csp)
	t.Cleanup(func() { removeFinalizerAndDelete(t, c, name) })
	waitForFinalizer(t, ctx, c, name, 30*time.Second)

	current := &mysqlv1alpha1.ClusterSwitchPolicy{}
	mustGet(t, ctx, c, name, current)
	patch := client.MergeFrom(current.DeepCopy())
	attemptID := "e2e-checkpoint-attempt-" + shortUUID(t)
	current.Status.Phase = mysqlv1alpha1.PhaseSwitchingOver
	current.Status.SwitchoverProgress = &mysqlv1alpha1.SwitchoverProgress{
		AttemptID:       attemptID,
		StartedAt:       metav1.NewTime(time.Now()),
		CurrentPhase:    "Promote",
		CompletedPhases: []string{"PreFlight", "Fence"},
		Reason:          "e2e-checkpoint test",
	}
	if err := c.Status().Patch(ctx, current, patch); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}

	// Bounce the controller deployment. We do this by scaling to 0 and back
	// to 2 via kubectl to avoid depending on the apps/v1 client just for
	// one restart.
	kubectl(t, "scale", "-n", "mysql-keeper-system",
		"deploy/mysql-keeper-controller-manager", "--replicas=0")
	kubectl(t, "wait", "-n", "mysql-keeper-system",
		"--for=delete", "pods", "-l", "app.kubernetes.io/name=mysql-keeper", "--timeout=30s")
	kubectl(t, "scale", "-n", "mysql-keeper-system",
		"deploy/mysql-keeper-controller-manager", "--replicas=2")
	kubectl(t, "wait", "-n", "mysql-keeper-system",
		"--for=condition=Available", "deploy/mysql-keeper-controller-manager", "--timeout=60s")

	// Poll the CR: the SwitchoverProgress we seeded must still be there, and
	// the AttemptID must match. Once the reconciler notices the stale state
	// it may flip Phase to Degraded (controlled by ResumeStuckTimeout); we
	// accept either SwitchingOver or Degraded as long as the checkpoint is
	// not silently nil'd.
	deadline := time.Now().Add(60 * time.Second)
	for {
		observed := &mysqlv1alpha1.ClusterSwitchPolicy{}
		mustGet(t, ctx, c, name, observed)
		if observed.Status.SwitchoverProgress == nil {
			t.Fatalf("SwitchoverProgress was lost across the restart (S6 regression)")
		}
		if observed.Status.SwitchoverProgress.AttemptID == attemptID {
			t.Logf("checkpoint preserved: phase=%s currentStep=%s",
				observed.Status.Phase, observed.Status.SwitchoverProgress.CurrentPhase)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("AttemptID changed unexpectedly: got %s, want %s",
				observed.Status.SwitchoverProgress.AttemptID, attemptID)
		}
		time.Sleep(2 * time.Second)
	}
}
