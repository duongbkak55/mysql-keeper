//go:build e2e

package e2e_test

import (
	"context"
	"testing"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
)

// TestE2E_FinalizerBlocksDelete covers S25 / 4.13. When the CR is in the
// SwitchingOver phase, Delete must not garbage-collect it; instead the
// finalizer keeps the object around so the reconciler has a chance to
// finish or abandon the in-flight operation.
func TestE2E_FinalizerBlocksDelete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c := newClient(t)
	name := "e2e-finalizer-" + shortUUID(t)

	csp := fixtureCSP(name)
	mustCreate(t, ctx, c, csp)
	t.Cleanup(func() { removeFinalizerAndDelete(t, c, name) })

	// Wait for the reconciler to attach the finalizer.
	waitForFinalizer(t, ctx, c, name, 30*time.Second)

	// Simulate an in-flight switchover via a status patch. In production the
	// engine would write this; for the e2e test we do it directly since we
	// are testing the finalizer guard, not the engine.
	updated := &mysqlv1alpha1.ClusterSwitchPolicy{}
	mustGet(t, ctx, c, name, updated)

	patch := client.MergeFrom(updated.DeepCopy())
	updated.Status.Phase = mysqlv1alpha1.PhaseSwitchingOver
	updated.Status.SwitchoverProgress = &mysqlv1alpha1.SwitchoverProgress{
		AttemptID:    "e2e-finalizer-attempt",
		StartedAt:    metav1.NewTime(time.Now()),
		CurrentPhase: "Fence",
	}
	if err := c.Status().Patch(ctx, updated, patch); err != nil {
		t.Fatalf("status patch: %v", err)
	}

	// Give the controller's informer a moment to observe the SwitchingOver
	// phase we just wrote. Without this, handleDeletion runs against a cache
	// view where Phase is still Monitoring and skips the "refuse to finalize
	// during a switchover" branch — causing the finalizer to be removed and
	// the CR to be GC'd.
	time.Sleep(3 * time.Second)

	// Issue Delete. k8s will set DeletionTimestamp but the finalizer is
	// expected to prevent actual GC until we remove it.
	if err := c.Delete(ctx, updated); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Wait a little and confirm the object still exists. 5s is comfortably
	// longer than any status patch round-trip but short enough to fail the
	// test quickly if the finalizer logic regressed.
	time.Sleep(5 * time.Second)
	check := &mysqlv1alpha1.ClusterSwitchPolicy{}
	err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, check)
	if kerrors.IsNotFound(err) {
		t.Fatalf("CR was garbage-collected despite finalizer (S25)")
	}
	if err != nil {
		t.Fatalf("unexpected Get error: %v", err)
	}
	if check.DeletionTimestamp.IsZero() {
		t.Errorf("expected DeletionTimestamp to be set; finalizer should keep the object around")
	}
	if !containsString(check.Finalizers, "mysql.keeper.io/finalizer") {
		t.Errorf("expected finalizer to still be present; got %v", check.Finalizers)
	}
}
