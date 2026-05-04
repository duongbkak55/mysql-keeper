package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/prometheus/client_golang/prometheus/testutil"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// TestEffectiveReplicationErrorHandling_OmittedDefaults covers the upgrade
// safety case (R5): an existing CR with no replicationErrorHandling field
// must NOT silently start auto-skipping transactions.
func TestEffectiveReplicationErrorHandling_OmittedDefaults(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{}
	cfg := effectiveReplicationErrorHandling(policy)
	if cfg.AutoSkip.Enabled {
		t.Errorf("expected AutoSkip.Enabled=false when feature not declared (upgrade safety), got true")
	}
	if cfg.AutoSkip.MaxSkipsPerWindow <= 0 {
		t.Errorf("expected MaxSkipsPerWindow default > 0, got %d", cfg.AutoSkip.MaxSkipsPerWindow)
	}
	if cfg.AutoSkip.Window.Duration <= 0 {
		t.Errorf("expected Window default > 0, got %s", cfg.AutoSkip.Window.Duration)
	}
	if cfg.AutoSkip.MaxSkipBeforeQuarantine <= 0 {
		t.Errorf("expected MaxSkipBeforeQuarantine default > 0, got %d", cfg.AutoSkip.MaxSkipBeforeQuarantine)
	}
}

// TestEffectiveReplicationErrorHandling_ExplicitEnabled confirms whitelist
// defaults fill in when user explicitly enables the feature without listing
// codes — the conservative [1062, 1032] pair.
func TestEffectiveReplicationErrorHandling_ExplicitEnabled(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ReplicationErrorHandling: &mysqlv1alpha1.ReplicationErrorHandlingConfig{
				AutoSkip: mysqlv1alpha1.AutoSkipConfig{Enabled: true},
			},
		},
	}
	cfg := effectiveReplicationErrorHandling(policy)
	if !cfg.AutoSkip.Enabled {
		t.Fatalf("expected enabled")
	}
	if got := cfg.AutoSkip.ErrorCodeWhitelist; len(got) != 2 || got[0] != 1062 || got[1] != 1032 {
		t.Errorf("expected default whitelist [1062 1032], got %v", got)
	}
}

// TestEffectiveReplicationErrorHandling_PreservesUserOverrides ensures user
// values for whitelist/rate limit/quarantine are honoured.
func TestEffectiveReplicationErrorHandling_PreservesUserOverrides(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ReplicationErrorHandling: &mysqlv1alpha1.ReplicationErrorHandlingConfig{
				AutoSkip: mysqlv1alpha1.AutoSkipConfig{
					Enabled:                 true,
					ErrorCodeWhitelist:      []int32{1062},
					MaxSkipsPerWindow:       7,
					Window:                  metav1.Duration{Duration: 5 * time.Minute},
					MaxSkipBeforeQuarantine: 9,
					QuarantineWindow:        metav1.Duration{Duration: 30 * time.Minute},
				},
			},
		},
	}
	cfg := effectiveReplicationErrorHandling(policy)
	if got := cfg.AutoSkip.ErrorCodeWhitelist; len(got) != 1 || got[0] != 1062 {
		t.Errorf("whitelist: %v", got)
	}
	if cfg.AutoSkip.MaxSkipsPerWindow != 7 {
		t.Errorf("MaxSkipsPerWindow: %d", cfg.AutoSkip.MaxSkipsPerWindow)
	}
	if cfg.AutoSkip.Window.Duration != 5*time.Minute {
		t.Errorf("Window: %s", cfg.AutoSkip.Window.Duration)
	}
	if cfg.AutoSkip.MaxSkipBeforeQuarantine != 9 {
		t.Errorf("MaxSkipBeforeQuarantine: %d", cfg.AutoSkip.MaxSkipBeforeQuarantine)
	}
	if cfg.AutoSkip.QuarantineWindow.Duration != 30*time.Minute {
		t.Errorf("QuarantineWindow: %s", cfg.AutoSkip.QuarantineWindow.Duration)
	}
}

// TestAppendSkipsCapped_BelowCap_AppendsAll covers the simple case where
// adding entries does not exceed the cap.
func TestAppendSkipsCapped_BelowCap_AppendsAll(t *testing.T) {
	now := metav1.Now()
	hist := []mysqlv1alpha1.SkippedTransaction{{GTID: "g1", SkippedAt: now}}
	new := []mysqlv1alpha1.SkippedTransaction{
		{GTID: "g2", SkippedAt: now},
		{GTID: "g3", SkippedAt: now},
	}
	out := appendSkipsCapped(hist, new, 50)
	if len(out) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(out))
	}
	if out[0].GTID != "g1" || out[2].GTID != "g3" {
		t.Errorf("order broken: %+v", out)
	}
}

// TestAppendSkipsCapped_OverCap_DropsOldest verifies the cap drops oldest
// entries first, preserving most-recent ordering.
func TestAppendSkipsCapped_OverCap_DropsOldest(t *testing.T) {
	now := metav1.Now()
	hist := []mysqlv1alpha1.SkippedTransaction{
		{GTID: "g1", SkippedAt: now},
		{GTID: "g2", SkippedAt: now},
		{GTID: "g3", SkippedAt: now},
	}
	new := []mysqlv1alpha1.SkippedTransaction{{GTID: "g4", SkippedAt: now}}
	out := appendSkipsCapped(hist, new, 3)
	if len(out) != 3 {
		t.Fatalf("expected 3 entries (capped), got %d", len(out))
	}
	if out[0].GTID != "g2" || out[2].GTID != "g4" {
		t.Errorf("expected oldest dropped, got %v", out)
	}
}

// TestUpdateReplicationErrorStatus_HealthyToError covers the condition
// transition from healthy → SQLApplierError when an error is observed.
func TestUpdateReplicationErrorStatus_HealthyToError(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{ClusterRole: "dc"},
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Initial: healthy state.
	updateReplicationErrorStatus(policy, replicationErrorOutcome{}, now)
	if cond := meta.FindStatusCondition(policy.Status.Conditions,
		mysqlv1alpha1.ConditionReplicationHealthy); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected initial ReplicationHealthy=True, got %+v", cond)
	}

	// Observe an error.
	out := replicationErrorOutcome{
		ActiveError: &mysqlv1alpha1.ReplicationErrorEntry{
			Channel:    "dc-to-dr",
			Errno:      1062,
			Message:    "Duplicate entry",
			ObservedAt: metav1.NewTime(now),
		},
	}
	updateReplicationErrorStatus(policy, out, now.Add(time.Minute))

	cond := meta.FindStatusCondition(policy.Status.Conditions,
		mysqlv1alpha1.ConditionReplicationHealthy)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected ReplicationHealthy=False after error, got %+v", cond)
	}
	if cond.Reason != "SQLApplierError" {
		t.Errorf("expected reason SQLApplierError, got %q", cond.Reason)
	}
	if policy.Status.ReplicationErrors == nil ||
		policy.Status.ReplicationErrors.LastError == nil ||
		policy.Status.ReplicationErrors.LastError.Errno != 1062 {
		t.Errorf("expected LastError.Errno=1062 in status, got %+v",
			policy.Status.ReplicationErrors)
	}
}

// TestUpdateReplicationErrorStatus_GTIDGapTriggers covers the GTID-gap-only
// alarm path: no SQL error but missing transactions exceeded threshold.
func TestUpdateReplicationErrorStatus_GTIDGapTriggers(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{ClusterRole: "dr"},
	}
	now := time.Now()
	updateReplicationErrorStatus(policy, replicationErrorOutcome{
		GTIDGapTriggered: true,
		GTIDMissing:      99999,
	}, now)
	cond := meta.FindStatusCondition(policy.Status.Conditions,
		mysqlv1alpha1.ConditionReplicationHealthy)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected unhealthy from GTID gap, got %+v", cond)
	}
	if cond.Reason != "GTIDGapExceeded" {
		t.Errorf("expected reason GTIDGapExceeded, got %q", cond.Reason)
	}
}

// TestUpdateReplicationErrorStatus_AnnotationClearPersisted ensures that
// when the quarantine is released by an operator annotation, the new
// annotation value is persisted into LastClearAnnotationValue so the next
// reconcile does not re-fire the same release.
func TestUpdateReplicationErrorStatus_AnnotationClearPersisted(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{ClusterRole: "dc"},
	}
	policy.Annotations = map[string]string{
		mysqlv1alpha1.AnnotationClearQuarantine: "2026-05-04T15:00:00Z",
	}
	now := time.Date(2026, 5, 4, 15, 0, 0, 0, time.UTC)
	q := metav1.NewTime(now.Add(-2 * time.Hour))
	policy.Status.ReplicationErrors = &mysqlv1alpha1.ReplicationErrorStatus{
		QuarantinedSince: &q,
		QuarantineReason: "old",
	}
	// Outcome says quarantine cleared this cycle.
	updateReplicationErrorStatus(policy, replicationErrorOutcome{
		QuarantineActive: false,
	}, now)
	r := policy.Status.ReplicationErrors
	if r == nil {
		t.Fatal("ReplicationErrors must remain non-nil")
	}
	if r.QuarantinedSince != nil {
		t.Errorf("expected QuarantinedSince cleared, got %v", r.QuarantinedSince)
	}
	if r.LastClearAnnotationValue != "2026-05-04T15:00:00Z" {
		t.Errorf("expected LastClearAnnotationValue persisted, got %q",
			r.LastClearAnnotationValue)
	}
}

// fakeDetector is a workerErrorDetector test double that returns a fixed slice
// of WorkerErrors. Use nil errors slice to simulate "no errors this cycle".
type fakeDetector struct {
	errs []pxc.WorkerError
}

func (f *fakeDetector) DetectWorkerErrors(_ context.Context, _ string) ([]pxc.WorkerError, error) {
	return f.errs, nil
}

// fakeInspectorDetector satisfies both switchover.ReplicationInspector and
// workerErrorDetector so it can be placed in componentSet.localInspector.
// The ReplicationInspector methods are stubs — the stale-label reset test
// does not exercise any of those code paths.
type fakeInspectorDetector struct {
	fakeDetector
}

func (f *fakeInspectorDetector) GetGTIDSnapshot(_ context.Context) (pxc.GTIDSnapshot, error) {
	return pxc.GTIDSnapshot{}, nil
}
func (f *fakeInspectorDetector) GetExecutedGTID(_ context.Context) (string, error) { return "", nil }
func (f *fakeInspectorDetector) IsGTIDSubset(_ context.Context, _ string) (bool, error) {
	return true, nil
}
func (f *fakeInspectorDetector) MissingGTIDs(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (f *fakeInspectorDetector) WaitForGTID(_ context.Context, _ string, _ time.Duration) error {
	return nil
}
func (f *fakeInspectorDetector) GetReplicationStatus(_ context.Context, _ string) (pxc.ReplicationStatus, error) {
	return pxc.ReplicationStatus{}, nil
}
func (f *fakeInspectorDetector) ProbeReachable(_ context.Context, _ time.Duration) (bool, error) {
	return true, nil
}

// TestObserveReplicationErrors_StaleLabelsReset verifies that when a
// (role,channel,errno) combination was set to 1 in a prior reconcile cycle
// but is absent in the current cycle, the reconciler explicitly resets its
// gauge to 0 and removes the key from activeReplicationErrorLabels.
//
// Scenario:
//   - Pre-populate activeReplicationErrorLabels with two stale keys (errno
//     1062 and 1032) for a known policy UID.
//   - Call observeReplicationErrors with a fake detector that returns no
//     errors (empty cycle).
//   - Assert both gauge series are reset to 0 and the map is empty for
//     this policy's UID after the call.
func TestObserveReplicationErrors_StaleLabelsReset(t *testing.T) {
	const role = "dc"
	const channel = "dc-to-dr"
	uid := types.UID("aaaa-bbbb-cccc-dddd")

	// Seed the gauges to 1 so we can detect the reset.
	metrics.ReplicationError.WithLabelValues(role, channel, "1062").Set(1)
	metrics.ReplicationError.WithLabelValues(role, channel, "1032").Set(1)

	r := &ClusterSwitchPolicyReconciler{}
	// Pre-populate two stale label keys as if a previous cycle had set them.
	key1062 := replicationErrorLabelKey(string(uid), role, channel, "1062")
	key1032 := replicationErrorLabelKey(string(uid), role, channel, "1032")
	r.activeReplicationErrorLabels.Store(key1062, struct{}{})
	r.activeReplicationErrorLabels.Store(key1032, struct{}{})

	// Policy with matching UID, channel, role; no ReplicationErrorHandling so
	// GTID gap alarm is disabled. The detector returns no errors this cycle.
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ClusterRole:            role,
			ReplicationChannelName: channel,
		},
	}
	policy.UID = uid

	det := &fakeInspectorDetector{fakeDetector: fakeDetector{errs: nil}}
	comps := &componentSet{localInspector: det}

	r.observeReplicationErrors(context.Background(), policy, comps, 0, false)

	// Both stale gauges must be reset to 0.
	if got := testutil.ToFloat64(metrics.ReplicationError.WithLabelValues(role, channel, "1062")); got != 0 {
		t.Errorf("errno 1062 gauge: want 0, got %v", got)
	}
	if got := testutil.ToFloat64(metrics.ReplicationError.WithLabelValues(role, channel, "1032")); got != 0 {
		t.Errorf("errno 1032 gauge: want 0, got %v", got)
	}

	// Both keys must be removed from the tracking map.
	if _, ok := r.activeReplicationErrorLabels.Load(key1062); ok {
		t.Errorf("key1062 should have been deleted from activeReplicationErrorLabels")
	}
	if _, ok := r.activeReplicationErrorLabels.Load(key1032); ok {
		t.Errorf("key1032 should have been deleted from activeReplicationErrorLabels")
	}
}

// TestUpdateReplicationErrorStatus_QuarantineTransitions covers entering and
// leaving quarantine: QuarantinedSince must be stamped on entry and cleared
// on exit; the ReplicaQuarantined condition must follow.
func TestUpdateReplicationErrorStatus_QuarantineTransitions(t *testing.T) {
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{ClusterRole: "dc"},
	}
	enter := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	updateReplicationErrorStatus(policy, replicationErrorOutcome{
		QuarantineActive: true,
		QuarantineReason: "skip count 6 > 5",
	}, enter)

	cond := meta.FindStatusCondition(policy.Status.Conditions,
		mysqlv1alpha1.ConditionReplicaQuarantined)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected quarantined=True, got %+v", cond)
	}
	if policy.Status.ReplicationErrors == nil ||
		policy.Status.ReplicationErrors.QuarantinedSince == nil {
		t.Errorf("expected QuarantinedSince stamped, got %+v",
			policy.Status.ReplicationErrors)
	}

	// Re-enter quarantine on the next reconcile — QuarantinedSince must NOT
	// move to the new timestamp (sticky entry-time).
	original := policy.Status.ReplicationErrors.QuarantinedSince.Time
	later := enter.Add(5 * time.Minute)
	updateReplicationErrorStatus(policy, replicationErrorOutcome{
		QuarantineActive: true,
		QuarantineReason: "skip count 7 > 5",
	}, later)
	if !policy.Status.ReplicationErrors.QuarantinedSince.Time.Equal(original) {
		t.Errorf("QuarantinedSince was reset on re-entry: %s -> %s",
			original, policy.Status.ReplicationErrors.QuarantinedSince.Time)
	}

	// Clear quarantine.
	updateReplicationErrorStatus(policy, replicationErrorOutcome{}, later.Add(time.Minute))
	cond = meta.FindStatusCondition(policy.Status.Conditions,
		mysqlv1alpha1.ConditionReplicaQuarantined)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected quarantined=False after clear, got %+v", cond)
	}
	if policy.Status.ReplicationErrors.QuarantinedSince != nil {
		t.Errorf("expected QuarantinedSince cleared, got %v",
			policy.Status.ReplicationErrors.QuarantinedSince)
	}
}
