// Replication-error reconciler.
//
// Phase 2 (this file): detection + alarm. We query worker-level error rows
// from performance_schema, surface them as k8s events, Prometheus metrics,
// and CR Status conditions, and persist a stable summary on
// status.replicationErrors.
//
// Phase 3 augments this file by wiring the auto-skip path through
// applyAutoSkip — see replication_skip.go.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// workerErrorDetector is the subset of *pxc.Manager methods used by the
// replication-error reconciler. Defined locally so non-direct inspectors
// (e.g. mano.PXCManager) can be skipped via type assertion without breaking
// the switchover.ReplicationInspector interface.
type workerErrorDetector interface {
	DetectWorkerErrors(ctx context.Context, channel string) ([]pxc.WorkerError, error)
}

// replicationErrorOutcome summarises the per-reconcile findings of the
// replication-error reconciler. The caller in Reconcile() uses this to update
// CR status, emit events, and decide whether the C12 quarantine guard fires.
type replicationErrorOutcome struct {
	// ActiveError is the first SQL apply error currently observed, if any.
	// Subsequent worker errors are exposed via metrics; only the first
	// populates status.replicationErrors.lastError so operators have a
	// stable anchor in `kubectl describe`.
	ActiveError *mysqlv1alpha1.ReplicationErrorEntry

	// AllErrors holds every distinct error currently observed (1 entry per
	// worker). Used only inside this package; not persisted directly on CR.
	AllErrors []mysqlv1alpha1.ReplicationErrorEntry

	// GTIDGapTriggered is true when missing GTID transactions exceeded the
	// configured replicationErrorHandling.gtidGapAlertThreshold.
	GTIDGapTriggered bool

	// GTIDMissing is the gauge value passed in from observeGTIDLag for
	// inclusion in the condition message.
	GTIDMissing int64

	// Skipped lists transactions skipped (or would-be-skipped in dry-run)
	// during this reconcile cycle. Filled by P3.
	Skipped []mysqlv1alpha1.SkippedTransaction

	// SkipBlocked maps reason -> count for skip-block bumps this cycle.
	SkipBlocked map[string]int

	// QuarantineActive is the post-reconcile quarantine state.
	QuarantineActive bool
	QuarantineReason string

	// RefusedClearAnnotation is the annotation value that the operator
	// supplied but that the controller refused to act on (e.g. because an
	// active error or a still-bursty skip count blocked the clear). When
	// non-empty, updateReplicationErrorStatus persists it into
	// status.replicationErrors.lastClearAnnotationValue so subsequent
	// reconciles do not re-fire ClearQuarantineRefused for the same value.
	RefusedClearAnnotation string

	// RefusedClearReason is the human-readable reason that accompanied the
	// refusal. Surfaced via the ClearQuarantineRefused event.
	RefusedClearReason string
}

// observeReplicationErrors runs after observeGTIDLag and before any
// switchover branch. It detects SQL applier errors on the local replica
// channel, optionally invokes the auto-skip path (P3), evaluates rate-limit
// and quarantine thresholds, and returns an outcome the caller persists
// alongside the health-status patch.
//
// All side effects (events, metrics) are emitted from this function. The
// caller is responsible for the status patch (via updateReplicationErrorStatus).
func (r *ClusterSwitchPolicyReconciler) observeReplicationErrors(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	gtidMissing int64,
	gtidMeasured bool,
) replicationErrorOutcome {
	cfg := effectiveReplicationErrorHandling(policy)
	out := replicationErrorOutcome{
		SkipBlocked: map[string]int{},
		GTIDMissing: gtidMissing,
	}
	if comps == nil || comps.localInspector == nil {
		return out
	}
	channel := policy.Spec.ReplicationChannelName
	if channel == "" {
		return out
	}
	role := policy.Spec.ClusterRole
	logger := ctrl.LoggerFrom(ctx).WithValues("component", "replication-errors")

	// GTID gap alarm — alarm-only path, never triggers a skip. Threshold 0
	// disables it. We deliberately keep this distinct from the existing
	// healthCheck.GTIDLagAlertThresholdTransactions so the two surfaces can
	// be tuned independently during migration.
	if gtidMeasured && cfg.GTIDGapAlertThreshold > 0 && gtidMissing > cfg.GTIDGapAlertThreshold {
		out.GTIDGapTriggered = true
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeWarning, "GTIDGapHigh",
				fmt.Sprintf("GTID gap %d exceeds replicationErrorHandling threshold %d on channel %q",
					gtidMissing, cfg.GTIDGapAlertThreshold, channel))
		}
	}

	// Worker-level SQL apply errors. If the inspector is not a direct MySQL
	// connection (e.g. MANO-backed), skip silently — the queries require
	// performance_schema access.
	detector, ok := comps.localInspector.(workerErrorDetector)
	if !ok {
		return out
	}
	workerErrs, err := detector.DetectWorkerErrors(ctx, channel)
	if err != nil {
		logger.V(1).Info("DetectWorkerErrors failed", "err", err)
		return out
	}
	now := time.Now()
	for _, w := range workerErrs {
		ts := w.Timestamp
		if ts.IsZero() {
			ts = now
		}
		entry := mysqlv1alpha1.ReplicationErrorEntry{
			Channel:    w.Channel,
			WorkerID:   w.WorkerID,
			GTID:       w.FailedGTID,
			Errno:      w.Errno,
			Message:    w.Message,
			ObservedAt: metav1.NewTime(ts),
		}
		out.AllErrors = append(out.AllErrors, entry)
		metrics.ReplicationError.WithLabelValues(role, channel,
			strconv.FormatInt(int64(w.Errno), 10)).Set(1)
	}
	if len(out.AllErrors) > 0 {
		first := out.AllErrors[0]
		out.ActiveError = &first
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeWarning, "ReplicationSQLError",
				fmt.Sprintf("channel=%q worker=%d errno=%d gtid=%q: %s",
					first.Channel, first.WorkerID, first.Errno, first.GTID, first.Message))
		}
	} else {
		// Best-effort metric reset for the most recent errno seen on the CR.
		// We do not enumerate historical errnos — operators should alert on
		// `replication_error == 1` rather than its absence.
		if last := policy.Status.ReplicationErrors; last != nil && last.LastError != nil {
			metrics.ReplicationError.WithLabelValues(role, channel,
				strconv.FormatInt(int64(last.LastError.Errno), 10)).Set(0)
		}
	}

	// Capture quarantine state before applyAutoSkip so we can detect the
	// operator-driven clear transition for event emission below.
	wasQuarantined := isCurrentlyQuarantined(policy)

	// P3 hook: invoke auto-skip path. No-op when the feature is disabled
	// or when the inspector cannot satisfy the skipper interface.
	r.applyAutoSkip(ctx, policy, comps, cfg, &out)

	// Operator-driven clear-quarantine: emit a Normal event so the
	// transition is visible in `kubectl describe` without parsing logs.
	if wasQuarantined && !out.QuarantineActive && r.Recorder != nil {
		val := policy.Annotations[mysqlv1alpha1.AnnotationClearQuarantine]
		r.Recorder.Event(policy, corev1.EventTypeNormal, "ReplicaQuarantineCleared",
			fmt.Sprintf("operator cleared replica quarantine via annotation %s=%q",
				mysqlv1alpha1.AnnotationClearQuarantine, val))
	}

	return out
}

// updateReplicationErrorStatus mutates policy.Status.ReplicationErrors and
// the ReplicationHealthy / ReplicaQuarantined conditions based on outcome.
// Caller is responsible for the actual status patch.
func updateReplicationErrorStatus(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	out replicationErrorOutcome,
	now time.Time,
) {
	cur := policy.Status.ReplicationErrors
	if cur == nil {
		cur = &mysqlv1alpha1.ReplicationErrorStatus{}
	}
	cur.LastError = out.ActiveError
	if len(out.Skipped) > 0 {
		cur.SkippedTransactions = appendSkipsCapped(cur.SkippedTransactions, out.Skipped, 50)
	}
	if out.QuarantineActive {
		if cur.QuarantinedSince == nil {
			t := metav1.NewTime(now)
			cur.QuarantinedSince = &t
		}
		cur.QuarantineReason = out.QuarantineReason
	} else if cur.QuarantinedSince != nil {
		cur.QuarantinedSince = nil
		cur.QuarantineReason = ""
		// Persist the annotation value that triggered the clear so the
		// next reconcile does not re-fire on the same operator action.
		if val := policy.Annotations[mysqlv1alpha1.AnnotationClearQuarantine]; val != "" {
			cur.LastClearAnnotationValue = val
		}
	}
	// Refused-clear path: still quarantined, but operator supplied a value
	// that did not satisfy preconditions. Persist the value so the warning
	// is one-shot (one event per distinct annotation value).
	if out.RefusedClearAnnotation != "" {
		cur.LastClearAnnotationValue = out.RefusedClearAnnotation
	}
	policy.Status.ReplicationErrors = cur

	// Replication healthy = no active error and GTID gap below threshold.
	healthy := out.ActiveError == nil && !out.GTIDGapTriggered
	healthyCond := metav1.Condition{
		Type:               mysqlv1alpha1.ConditionReplicationHealthy,
		LastTransitionTime: metav1.NewTime(now),
	}
	if healthy {
		healthyCond.Status = metav1.ConditionTrue
		healthyCond.Reason = "OK"
		healthyCond.Message = "no SQL apply error and GTID gap within threshold"
	} else {
		healthyCond.Status = metav1.ConditionFalse
		switch {
		case out.ActiveError != nil:
			healthyCond.Reason = "SQLApplierError"
			healthyCond.Message = fmt.Sprintf("errno=%d on channel %q: %s",
				out.ActiveError.Errno, out.ActiveError.Channel, out.ActiveError.Message)
		case out.GTIDGapTriggered:
			healthyCond.Reason = "GTIDGapExceeded"
			healthyCond.Message = fmt.Sprintf("missing GTID transactions exceeded threshold (current=%d)",
				out.GTIDMissing)
		}
	}
	meta.SetStatusCondition(&policy.Status.Conditions, healthyCond)

	quarantineCond := metav1.Condition{
		Type:               mysqlv1alpha1.ConditionReplicaQuarantined,
		LastTransitionTime: metav1.NewTime(now),
	}
	if out.QuarantineActive {
		quarantineCond.Status = metav1.ConditionTrue
		quarantineCond.Reason = "SkipThresholdExceeded"
		quarantineCond.Message = out.QuarantineReason
	} else {
		quarantineCond.Status = metav1.ConditionFalse
		quarantineCond.Reason = "Clear"
		quarantineCond.Message = "skip count below quarantine threshold"
	}
	meta.SetStatusCondition(&policy.Status.Conditions, quarantineCond)

	role := policy.Spec.ClusterRole
	if out.QuarantineActive {
		metrics.ReplicaQuarantined.WithLabelValues(role).Set(1)
	} else {
		metrics.ReplicaQuarantined.WithLabelValues(role).Set(0)
	}
}

// effectiveReplicationErrorHandling returns the user-configured policy with
// production-safe defaults filled in for omitted fields. Callers must not
// mutate the returned config.
//
// Upgrade safety (R5): when the user has not declared
// spec.replicationErrorHandling at all (src == nil), AutoSkip.Enabled defaults
// to false here so existing CRs from before this feature shipped do not
// silently start skipping transactions on upgrade. New CRs that include the
// field opt into the kubebuilder default of Enabled=true.
func effectiveReplicationErrorHandling(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
) mysqlv1alpha1.ReplicationErrorHandlingConfig {
	src := policy.Spec.ReplicationErrorHandling
	out := mysqlv1alpha1.ReplicationErrorHandlingConfig{}
	explicit := src != nil
	if explicit {
		out = *src
	}
	if !explicit {
		out.AutoSkip.Enabled = false
	}
	if out.AutoSkip.Enabled && len(out.AutoSkip.ErrorCodeWhitelist) == 0 {
		out.AutoSkip.ErrorCodeWhitelist = []int32{1062, 1032}
	}
	if out.AutoSkip.MaxSkipsPerWindow <= 0 {
		out.AutoSkip.MaxSkipsPerWindow = 3
	}
	if out.AutoSkip.Window.Duration <= 0 {
		out.AutoSkip.Window.Duration = 10 * time.Minute
	}
	if out.AutoSkip.MaxSkipBeforeQuarantine <= 0 {
		out.AutoSkip.MaxSkipBeforeQuarantine = 5
	}
	if out.AutoSkip.QuarantineWindow.Duration <= 0 {
		out.AutoSkip.QuarantineWindow.Duration = time.Hour
	}
	return out
}

// appendSkipsCapped appends entries to history, oldest-first, and returns the
// most-recent `cap` entries. Stable order preserved across reconciles.
func appendSkipsCapped(history []mysqlv1alpha1.SkippedTransaction,
	entries []mysqlv1alpha1.SkippedTransaction, capN int) []mysqlv1alpha1.SkippedTransaction {
	combined := append(history, entries...)
	if len(combined) <= capN {
		return combined
	}
	return combined[len(combined)-capN:]
}

