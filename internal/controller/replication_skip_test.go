package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// fakeSkipper records every SkipNextTransaction call and optionally returns
// an error. Sufficient for verifying whether the auto-skip path actually
// invokes SQL.
type fakeSkipper struct {
	calls   []struct{ Channel, GTID string }
	failOn  string // GTID prefix that triggers a synthetic error
	failErr error
}

func (f *fakeSkipper) SkipNextTransaction(_ context.Context, channel, gtid string) error {
	f.calls = append(f.calls, struct{ Channel, GTID string }{channel, gtid})
	if f.failOn != "" && strings.HasPrefix(gtid, f.failOn) {
		return f.failErr
	}
	return nil
}

func newPolicyWithSkipConfig(cfg mysqlv1alpha1.AutoSkipConfig) *mysqlv1alpha1.ClusterSwitchPolicy {
	return &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ClusterRole: "dr",
			ReplicationErrorHandling: &mysqlv1alpha1.ReplicationErrorHandlingConfig{
				AutoSkip: cfg,
			},
		},
	}
}

func errEntry(errno int32, gtid string) mysqlv1alpha1.ReplicationErrorEntry {
	return mysqlv1alpha1.ReplicationErrorEntry{
		Channel: "dc-to-dr", Errno: errno, GTID: gtid,
		Message: "synthetic", ObservedAt: metav1.Now(),
	}
}

// TestApplyAutoSkip_Disabled covers the upgrade-safety case: even when an
// error is observed, the disabled feature must not invoke the skipper.
func TestApplyAutoSkip_Disabled(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{Enabled: false})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1062, "abc:1")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	// Disabled is handled by the wrapper applyAutoSkip; once we are inside
	// applyAutoSkipWith, an enabled flag of false from cfg is irrelevant —
	// the wrapper would have short-circuited. So here we ensure the call
	// does its own job: with cfg.Enabled=false but reaching the inner path,
	// it still runs whitelist/rate-limit logic. We verify via the wrapper
	// path instead.
	r.applyAutoSkip(context.Background(), policy, &componentSet{}, cfg, out)
	if len(skipper.calls) != 0 {
		t.Errorf("expected 0 skip calls when disabled, got %d", len(skipper.calls))
	}
	if out.SkipBlocked["disabled"] == 0 {
		t.Errorf("expected disabled blocker to fire, got %v", out.SkipBlocked)
	}
}

// TestApplyAutoSkip_NotWhitelisted covers errno filtering: a 1146 (table
// missing — schema drift) must never be skipped even when the feature is on.
func TestApplyAutoSkip_NotWhitelisted(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062, 1032},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1146, "abc:1")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 0 {
		t.Errorf("expected 0 skip calls for non-whitelisted errno, got %d", len(skipper.calls))
	}
	if out.SkipBlocked["not_whitelisted"] != 1 {
		t.Errorf("expected not_whitelisted blocker, got %v", out.SkipBlocked)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("Skipped should be empty, got %v", out.Skipped)
	}
}

// TestApplyAutoSkip_HappyPath covers the canonical case: whitelisted errno,
// GTID known, under rate limit → SkipNextTransaction invoked.
func TestApplyAutoSkip_HappyPath(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1062, "abc:42")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 1 {
		t.Fatalf("expected 1 skip call, got %d", len(skipper.calls))
	}
	if skipper.calls[0].GTID != "abc:42" || skipper.calls[0].Channel != "dc-to-dr" {
		t.Errorf("unexpected skip call: %+v", skipper.calls[0])
	}
	if len(out.Skipped) != 1 || out.Skipped[0].DryRun {
		t.Errorf("expected 1 non-dry-run skip recorded, got %v", out.Skipped)
	}
}

// TestApplyAutoSkip_DryRun verifies dry-run records the would-be skip but
// never calls SQL.
func TestApplyAutoSkip_DryRun(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		DryRun:                  true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1062, "abc:42")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 0 {
		t.Fatalf("expected 0 SQL skip calls in dry-run, got %d", len(skipper.calls))
	}
	if len(out.Skipped) != 1 || !out.Skipped[0].DryRun {
		t.Errorf("expected 1 dry-run skip recorded, got %v", out.Skipped)
	}
	if out.SkipBlocked["dry_run"] != 1 {
		t.Errorf("expected dry_run blocker, got %v", out.SkipBlocked)
	}
}

// TestApplyAutoSkip_RateLimit: 5 fresh errors but MaxSkipsPerWindow=2 →
// exactly 2 calls execute, the rest are blocked with reason rate_limited.
func TestApplyAutoSkip_RateLimit(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       2,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 99,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors: []mysqlv1alpha1.ReplicationErrorEntry{
			errEntry(1062, "abc:1"),
			errEntry(1062, "abc:2"),
			errEntry(1062, "abc:3"),
			errEntry(1062, "abc:4"),
			errEntry(1062, "abc:5"),
		},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 2 {
		t.Errorf("expected 2 actual skips, got %d", len(skipper.calls))
	}
	if got := out.SkipBlocked["rate_limited"]; got != 3 {
		t.Errorf("expected 3 rate_limited, got %d", got)
	}
}

// TestApplyAutoSkip_MissingGTID: whitelisted errno but no GTID known →
// blocked with reason missing_gtid (cannot synthesise empty txn safely).
func TestApplyAutoSkip_MissingGTID(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1062, "")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 0 {
		t.Errorf("expected 0 skips when GTID missing, got %d", len(skipper.calls))
	}
	if out.SkipBlocked["missing_gtid"] != 1 {
		t.Errorf("expected missing_gtid blocker, got %v", out.SkipBlocked)
	}
}

// TestApplyAutoSkip_AlreadyQuarantined: when CR status is quarantined, no
// new skip runs even on whitelisted errors.
func TestApplyAutoSkip_AlreadyQuarantined(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-30 * time.Minute))
	policy.Status.ReplicationErrors = &mysqlv1alpha1.ReplicationErrorStatus{
		QuarantinedSince: &t1,
		QuarantineReason: "previous burst",
	}
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors: []mysqlv1alpha1.ReplicationErrorEntry{
			errEntry(1062, "abc:1"),
			errEntry(1062, "abc:2"),
		},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, now)

	if len(skipper.calls) != 0 {
		t.Errorf("expected 0 skips while quarantined, got %d", len(skipper.calls))
	}
	if out.SkipBlocked["quarantined"] != 2 {
		t.Errorf("expected 2 quarantined blockers, got %v", out.SkipBlocked)
	}
	if !out.QuarantineActive {
		t.Errorf("expected QuarantineActive=true, got false")
	}
}

// TestApplyAutoSkip_SkipperError: SQL skip fails → entry NOT recorded, error
// surfaced via blocker count or event (no panic).
func TestApplyAutoSkip_SkipperError(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors:   []mysqlv1alpha1.ReplicationErrorEntry{errEntry(1062, "abc:42")},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}
	skipper := &fakeSkipper{failOn: "abc:42", failErr: errors.New("synthetic SQL fail")}

	r.applyAutoSkipWith(context.Background(), policy, skipper, cfg, out, time.Now())

	if len(skipper.calls) != 1 {
		t.Errorf("expected 1 attempted call, got %d", len(skipper.calls))
	}
	if len(out.Skipped) != 0 {
		t.Errorf("expected 0 successful skips on SQL failure, got %v", out.Skipped)
	}
}

// nonSkippingInspector satisfies switchover.ReplicationInspector but does NOT
// implement replicationSkipper (no SkipNextTransaction method). Used to
// exercise the unsupported_inspector blocker branch in applyAutoSkip.
type nonSkippingInspector struct{}

func (nonSkippingInspector) GetGTIDSnapshot(_ context.Context) (pxc.GTIDSnapshot, error) {
	return pxc.GTIDSnapshot{}, nil
}
func (nonSkippingInspector) GetExecutedGTID(_ context.Context) (string, error)  { return "", nil }
func (nonSkippingInspector) IsGTIDSubset(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (nonSkippingInspector) MissingGTIDs(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (nonSkippingInspector) WaitForGTID(_ context.Context, _ string, _ time.Duration) error {
	return nil
}
func (nonSkippingInspector) GetReplicationStatus(_ context.Context, _ string) (pxc.ReplicationStatus, error) {
	return pxc.ReplicationStatus{}, nil
}
func (nonSkippingInspector) ProbeReachable(_ context.Context, _ time.Duration) (bool, error) {
	return true, nil
}

// TestApplyAutoSkip_UnsupportedInspector covers the blocker path where
// localInspector is non-nil but does not implement replicationSkipper.
// applyAutoSkip must type-assert to nil and increment
// out.SkipBlocked["unsupported_inspector"] without touching out.Skipped.
func TestApplyAutoSkip_UnsupportedInspector(t *testing.T) {
	policy := newPolicyWithSkipConfig(mysqlv1alpha1.AutoSkipConfig{
		Enabled:                 true,
		ErrorCodeWhitelist:      []int32{1062},
		MaxSkipsPerWindow:       3,
		Window:                  metav1.Duration{Duration: 10 * time.Minute},
		MaxSkipBeforeQuarantine: 5,
		QuarantineWindow:        metav1.Duration{Duration: time.Hour},
	})
	cfg := effectiveReplicationErrorHandling(policy)
	out := &replicationErrorOutcome{
		AllErrors: []mysqlv1alpha1.ReplicationErrorEntry{
			errEntry(1062, "abc:1"),
			errEntry(1062, "abc:2"),
		},
		SkipBlocked: map[string]int{},
	}
	r := &ClusterSwitchPolicyReconciler{}

	// nonSkippingInspector satisfies switchover.ReplicationInspector but
	// has no SkipNextTransaction — the type assertion in applyAutoSkip
	// will yield skipper==nil, triggering the unsupported_inspector branch.
	comps := &componentSet{localInspector: nonSkippingInspector{}}
	r.applyAutoSkip(context.Background(), policy, comps, cfg, out)

	if got := out.SkipBlocked["unsupported_inspector"]; got == 0 {
		t.Errorf("expected unsupported_inspector blocker > 0, got %d (full map: %v)", got, out.SkipBlocked)
	}
	if len(out.Skipped) != 0 {
		t.Errorf("expected Skipped to be empty, got %v", out.Skipped)
	}
}

// TestEvaluateQuarantine_BelowThreshold leaves QuarantineActive=false when
// not enough skips have happened in the window.
func TestEvaluateQuarantine_BelowThreshold(t *testing.T) {
	now := time.Now()
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				SkippedTransactions: []mysqlv1alpha1.SkippedTransaction{
					{SkippedAt: metav1.NewTime(now.Add(-30 * time.Minute))},
				},
			},
		},
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if out.QuarantineActive {
		t.Errorf("expected QuarantineActive=false (count=1, threshold=3)")
	}
}

// TestEvaluateQuarantine_AboveThreshold flips QuarantineActive=true when the
// combined history+pending count strictly exceeds the threshold inside the
// QuarantineWindow.
func TestEvaluateQuarantine_AboveThreshold(t *testing.T) {
	now := time.Now()
	hist := []mysqlv1alpha1.SkippedTransaction{
		{SkippedAt: metav1.NewTime(now.Add(-50 * time.Minute))},
		{SkippedAt: metav1.NewTime(now.Add(-40 * time.Minute))},
		{SkippedAt: metav1.NewTime(now.Add(-30 * time.Minute))},
	}
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{SkippedTransactions: hist},
		},
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{
		Skipped: []mysqlv1alpha1.SkippedTransaction{
			{SkippedAt: metav1.NewTime(now)},
		},
	}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if !out.QuarantineActive {
		t.Fatalf("expected QuarantineActive=true (count=4 > threshold=3)")
	}
	if out.QuarantineReason == "" {
		t.Errorf("expected QuarantineReason populated")
	}
}

// TestOperatorRequestedClear: detects new annotation values vs the
// LastClearAnnotationValue persisted on the CR.
func TestOperatorRequestedClear(t *testing.T) {
	cases := []struct {
		name       string
		annotation string
		lastSeen   string
		want       bool
	}{
		{"empty annotation", "", "", false},
		{"empty annotation with last seen", "", "old", false},
		{"first ever value", "2026-05-04T15:00:00Z", "", true},
		{"changed value", "2026-05-04T16:00:00Z", "2026-05-04T15:00:00Z", true},
		{"same value (already processed)", "abc", "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy := &mysqlv1alpha1.ClusterSwitchPolicy{}
			if tc.annotation != "" {
				policy.Annotations = map[string]string{
					mysqlv1alpha1.AnnotationClearQuarantine: tc.annotation,
				}
			}
			if tc.lastSeen != "" {
				policy.Status.ReplicationErrors = &mysqlv1alpha1.ReplicationErrorStatus{
					LastClearAnnotationValue: tc.lastSeen,
				}
			}
			if got := operatorRequestedClear(policy); got != tc.want {
				t.Errorf("operatorRequestedClear=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestEvaluateQuarantine_AnnotationClears verifies the happy-path operator
// release: new annotation value, no active error, skip count below threshold.
func TestEvaluateQuarantine_AnnotationClears(t *testing.T) {
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-2 * time.Hour))
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				QuarantinedSince: &t1,
				QuarantineReason: "old",
			},
		},
	}
	policy.Annotations = map[string]string{
		mysqlv1alpha1.AnnotationClearQuarantine: "2026-05-04T15:00:00Z",
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if out.QuarantineActive {
		t.Errorf("expected QuarantineActive=false after operator clear")
	}
}

// fakeRecorder is a minimal eventRecorderShim test double that captures
// emitted events for assertion.
type fakeRecorder struct {
	events []struct{ Type, Reason, Message string }
}

func (f *fakeRecorder) Event(_ runtime.Object, eventType, reason, message string) {
	f.events = append(f.events, struct{ Type, Reason, Message string }{eventType, reason, message})
}

// TestEvaluateQuarantine_RefusedClear_EmitsEventOnce verifies that when an
// operator submits a clear annotation but the preconditions are not met,
// the controller emits a Warning ClearQuarantineRefused event AND records
// the refused annotation value so the same value does not re-fire on
// subsequent reconciles.
func TestEvaluateQuarantine_RefusedClear_EmitsEventOnce(t *testing.T) {
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-30 * time.Minute))
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				QuarantinedSince: &t1,
				QuarantineReason: "burst",
			},
		},
	}
	policy.Annotations = map[string]string{
		mysqlv1alpha1.AnnotationClearQuarantine: "2026-05-04T15:00:00Z",
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{
		ActiveError: &mysqlv1alpha1.ReplicationErrorEntry{
			Channel: "dc-to-dr", Errno: 1062, Message: "still firing",
		},
	}
	rec := &fakeRecorder{}
	evaluateQuarantine(rec, policy, cfg, out, now)

	if !out.QuarantineActive {
		t.Errorf("expected quarantine to remain active")
	}
	if out.RefusedClearAnnotation != "2026-05-04T15:00:00Z" {
		t.Errorf("expected RefusedClearAnnotation captured, got %q", out.RefusedClearAnnotation)
	}
	if out.RefusedClearReason == "" {
		t.Errorf("expected RefusedClearReason populated")
	}
	if len(rec.events) != 1 {
		t.Fatalf("expected 1 ClearQuarantineRefused event, got %d", len(rec.events))
	}
	ev := rec.events[0]
	if ev.Type != "Warning" || ev.Reason != "ClearQuarantineRefused" {
		t.Errorf("unexpected event %+v", ev)
	}
}

// TestEvaluateQuarantine_AnnotationRefusedActiveError: if a SQL apply error
// is currently observed, the operator clear is REFUSED — the underlying
// problem must be resolved first.
func TestEvaluateQuarantine_AnnotationRefusedActiveError(t *testing.T) {
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-2 * time.Hour))
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				QuarantinedSince: &t1,
				QuarantineReason: "old",
			},
		},
	}
	policy.Annotations = map[string]string{
		mysqlv1alpha1.AnnotationClearQuarantine: "2026-05-04T15:00:00Z",
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{
		ActiveError: &mysqlv1alpha1.ReplicationErrorEntry{
			Channel: "dc-to-dr", Errno: 1062, Message: "still firing",
		},
	}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if !out.QuarantineActive {
		t.Errorf("expected quarantine to remain active while error firing")
	}
}

// TestEvaluateQuarantine_AnnotationRefusedAboveThreshold: skip burst still
// in window > threshold → operator clear refused (entry condition still met).
func TestEvaluateQuarantine_AnnotationRefusedAboveThreshold(t *testing.T) {
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-30 * time.Minute))
	hist := []mysqlv1alpha1.SkippedTransaction{
		{SkippedAt: metav1.NewTime(now.Add(-50 * time.Minute))},
		{SkippedAt: metav1.NewTime(now.Add(-40 * time.Minute))},
		{SkippedAt: metav1.NewTime(now.Add(-30 * time.Minute))},
		{SkippedAt: metav1.NewTime(now.Add(-20 * time.Minute))},
	}
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				QuarantinedSince:    &t1,
				QuarantineReason:    "burst",
				SkippedTransactions: hist,
			},
		},
	}
	policy.Annotations = map[string]string{
		mysqlv1alpha1.AnnotationClearQuarantine: "2026-05-04T15:00:00Z",
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if !out.QuarantineActive {
		t.Errorf("expected quarantine to remain while burst still in window")
	}
}

// TestEvaluateQuarantine_StickyAfterEntry: once QuarantinedSince is set,
// quarantine stays active even when the sliding-window count drops below
// threshold (operator must clear).
func TestEvaluateQuarantine_StickyAfterEntry(t *testing.T) {
	now := time.Now()
	t1 := metav1.NewTime(now.Add(-2 * time.Hour))
	policy := &mysqlv1alpha1.ClusterSwitchPolicy{
		Status: mysqlv1alpha1.ClusterSwitchPolicyStatus{
			ReplicationErrors: &mysqlv1alpha1.ReplicationErrorStatus{
				QuarantinedSince: &t1,
				QuarantineReason: "old burst",
				// No skips inside QuarantineWindow.
			},
		},
	}
	cfg := mysqlv1alpha1.ReplicationErrorHandlingConfig{
		AutoSkip: mysqlv1alpha1.AutoSkipConfig{
			MaxSkipBeforeQuarantine: 3,
			QuarantineWindow:        metav1.Duration{Duration: time.Hour},
		},
	}
	out := &replicationErrorOutcome{}
	evaluateQuarantine(nil, policy, cfg, out, now)
	if !out.QuarantineActive {
		t.Errorf("expected sticky quarantine even with empty window count")
	}
	if out.QuarantineReason != "old burst" {
		t.Errorf("expected previous reason preserved, got %q", out.QuarantineReason)
	}
}
