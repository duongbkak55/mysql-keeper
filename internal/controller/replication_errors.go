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
	"strings"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/health"
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

// observeReplicationErrors runs after the cluster health check on every
// reconcile cycle (cadence == spec.healthCheck.interval, default 15s).
// It detects SQL applier errors on the local replica channel, optionally
// invokes the auto-skip path (P3), evaluates rate-limit and quarantine
// thresholds, and returns an outcome the caller persists alongside the
// health-status patch.
//
// localHealth is the just-collected health snapshot for the local cluster.
// When it reports WritableUnknown (i.e. the cluster did not respond to the
// health probe at all), this function returns early without issuing any
// performance_schema queries — those queries would only time out and
// produce noisy debug logs while MySQL is unreachable. Detection resumes
// automatically on the next reconcile once the cluster comes back.
//
// All side effects (events, metrics) are emitted from this function. The
// caller is responsible for the status patch (via updateReplicationErrorStatus).
func (r *ClusterSwitchPolicyReconciler) observeReplicationErrors(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	localHealth health.ClusterHealth,
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

	// Reachability gate — when the health check could not even read
	// read_only on the local cluster, MySQL is effectively unreachable.
	// Don't bother running performance_schema queries that will only time
	// out; the next reconcile will retry once the probe comes back.
	if localHealth.Writable == health.WritableUnknown {
		logger.V(1).Info("replication_error_detection_skipped_unreachable",
			"event", "replication_error_detection_skipped",
			"cluster_role", role,
			"channel", channel,
			"reason", "local_unreachable",
		)
		return out
	}

	// Pre-seed the replication_error gauge for whitelisted errnos so the
	// series appears in /metrics from the first reconcile cycle, regardless
	// of whether any error has fired. Without this, Prometheus shows "no
	// data" until an incident, which makes alerting rules harder to validate.
	// The detection loop below sets the gauge to 1 for any errno actually
	// observed; the diff-reset path zeroes errnos that previously fired.
	preseedErrnos := cfg.AutoSkip.ErrorCodeWhitelist
	if len(preseedErrnos) == 0 {
		preseedErrnos = []int32{1062, 1032}
	}
	for _, errno := range preseedErrnos {
		errnoStr := strconv.FormatInt(int64(errno), 10)
		key := replicationErrorLabelKey(string(policy.UID), role, channel, errnoStr)
		if _, alreadyActive := r.activeReplicationErrorLabels.Load(key); !alreadyActive {
			metrics.ReplicationError.WithLabelValues(role, channel, errnoStr).Set(0)
		}
	}

	// GTID gap alarm — alarm-only path, never triggers a skip. Threshold 0
	// disables it. We deliberately keep this distinct from the existing
	// healthCheck.GTIDLagAlertThresholdTransactions so the two surfaces can
	// be tuned independently during migration.
	if gtidMeasured && cfg.GTIDGapAlertThreshold > 0 && gtidMissing > cfg.GTIDGapAlertThreshold {
		out.GTIDGapTriggered = true
		logger.Info("replication_gtid_gap_high",
			"event", "replication_gtid_gap_high",
			"cluster_role", role,
			"channel", channel,
			"missing", gtidMissing,
			"threshold", cfg.GTIDGapAlertThreshold,
		)
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
	uid := string(policy.UID)
	// currentLabels tracks every (uid,role,channel,errno) key observed THIS
	// cycle so we can diff against activeReplicationErrorLabels and zero any
	// gauge series that were "1" last cycle but are absent now.
	currentLabels := map[string]struct{}{}
	// newErrorEntries collects only the errors that are seeing their first
	// observation in the current incident — i.e. their (errno) label was
	// NOT in activeReplicationErrorLabels before this cycle. Used to debounce
	// k8s event emission so `kubectl describe` is not flooded with the same
	// Warning every reconcile while a long-running error persists. Logs
	// (which go to ELK) and metrics (gauges) keep firing every cycle so the
	// incident's duration stays observable.
	var newErrorEntries []mysqlv1alpha1.ReplicationErrorEntry

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
		errnoStr := strconv.FormatInt(int64(w.Errno), 10)
		metrics.ReplicationError.WithLabelValues(role, channel, errnoStr).Set(1)
		key := replicationErrorLabelKey(uid, role, channel, errnoStr)
		currentLabels[key] = struct{}{}
		// LoadOrStore returns wasPresent=true when the key was already in the
		// map from a prior cycle. We treat the inverse as "new error
		// transition" worth alarming about.
		if _, wasActive := r.activeReplicationErrorLabels.LoadOrStore(key, struct{}{}); !wasActive {
			newErrorEntries = append(newErrorEntries, entry)
		}
		logger.Info("replication_sql_error",
			"event", "replication_sql_error",
			"cluster_role", role,
			"channel", w.Channel,
			"worker_id", w.WorkerID,
			"errno", w.Errno,
			"gtid", w.FailedGTID,
			"message", truncateMessage(w.Message, 256),
		)
	}
	if len(out.AllErrors) > 0 {
		first := out.AllErrors[0]
		out.ActiveError = &first
		// Debounce: only emit a k8s Warning when at least one of the
		// observed errnos was NOT firing in the previous reconcile. Long-
		// running incidents would otherwise flood `kubectl describe csp`
		// with the same Warning every healthcheck.interval. The metric
		// (gauge=1 every cycle) and the structured log already give ELK /
		// Prometheus full coverage of the incident's duration.
		if len(newErrorEntries) > 0 && r.Recorder != nil {
			ne := newErrorEntries[0]
			r.Recorder.Event(policy, corev1.EventTypeWarning, "ReplicationSQLError",
				fmt.Sprintf("channel=%q worker=%d errno=%d gtid=%q: %s",
					ne.Channel, ne.WorkerID, ne.Errno, ne.GTID, ne.Message))
		}
	}

	// Diff-based metric reset: any key for this policy that was active in a
	// prior cycle but is absent from currentLabels must be zeroed. This
	// prevents stale "errno=X → 1" series from lingering after the error
	// rotates to a different errno or clears entirely.
	uidPrefix := uid + "\x00"
	r.activeReplicationErrorLabels.Range(func(k, _ any) bool {
		key := k.(string)
		if !strings.HasPrefix(key, uidPrefix) {
			return true // belongs to a different policy; skip
		}
		if _, present := currentLabels[key]; present {
			return true // still active this cycle; leave at 1
		}
		// Parse role/channel/errno back out of the key so we can call
		// WithLabelValues with exactly the values that were registered.
		_, parsedRole, parsedChannel, parsedErrno, ok := parseReplicationErrorLabelKey(key)
		if ok {
			metrics.ReplicationError.WithLabelValues(parsedRole, parsedChannel, parsedErrno).Set(0)
		}
		r.activeReplicationErrorLabels.Delete(key)
		return true
	})

	// Capture quarantine state before applyAutoSkip so we can detect the
	// operator-driven clear transition for event emission below.
	wasQuarantined := isCurrentlyQuarantined(policy)

	// P3 hook: invoke auto-skip path. No-op when the feature is disabled
	// or when the inspector cannot satisfy the skipper interface.
	r.applyAutoSkip(ctx, policy, comps, cfg, &out)

	// Quarantine transition logs — one line per state change so ELK can
	// alert on the transition rather than parsing condition history.
	switch {
	case !wasQuarantined && out.QuarantineActive:
		logger.Info("replica_quarantine_entered",
			"event", "replica_quarantine_entered",
			"cluster_role", role,
			"channel", channel,
			"reason", out.QuarantineReason,
		)
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeWarning, "ReplicaQuarantined",
				fmt.Sprintf("local replica quarantined: %s — clear via annotation %s before promote",
					out.QuarantineReason, mysqlv1alpha1.AnnotationClearQuarantine))
		}
	case wasQuarantined && !out.QuarantineActive:
		val := policy.Annotations[mysqlv1alpha1.AnnotationClearQuarantine]
		logger.Info("replica_quarantine_cleared",
			"event", "replica_quarantine_cleared",
			"cluster_role", role,
			"channel", channel,
			"annotation_value", val,
		)
		if r.Recorder != nil {
			r.Recorder.Event(policy, corev1.EventTypeNormal, "ReplicaQuarantineCleared",
				fmt.Sprintf("operator cleared replica quarantine via annotation %s=%q",
					mysqlv1alpha1.AnnotationClearQuarantine, val))
		}
	}
	if out.RefusedClearAnnotation != "" {
		logger.Info("replica_quarantine_clear_refused",
			"event", "replica_quarantine_clear_refused",
			"cluster_role", role,
			"channel", channel,
			"annotation_value", out.RefusedClearAnnotation,
			"reason", out.RefusedClearReason,
		)
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
	// Prune entries older than HistoryRetention before this cycle's appends
	// so the time-based bound holds even when no new skip lands. Cap at 50
	// entries afterwards so a flood within retention still cannot grow the
	// list unbounded.
	cfg := effectiveReplicationErrorHandling(policy)
	cur.SkippedTransactions = pruneSkipsByRetention(cur.SkippedTransactions,
		cfg.AutoSkip.HistoryRetention.Duration, now)
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
	if out.AutoSkip.HistoryRetention.Duration <= 0 {
		// 7 days mirrors the kubebuilder default and the recommended
		// minimum binlog retention; keeps audit history aligned with what
		// MySQL itself can replay.
		out.AutoSkip.HistoryRetention.Duration = 7 * 24 * time.Hour
	}
	return out
}

// pruneSkipsByRetention drops entries older than `retention` from history.
// Returns history unchanged when retention <= 0 (caller opted out of
// time-based pruning). Stable order preserved.
//
// The filter is order-independent: it walks every entry and keeps those
// whose SkippedAt is at or after the cutoff. This is correct even when
// clock skew or future code paths insert a fresh entry before a stale one.
// history[:0:0] forces a fresh backing array so the returned slice never
// aliases the input (M2 fix).
func pruneSkipsByRetention(
	history []mysqlv1alpha1.SkippedTransaction,
	retention time.Duration,
	now time.Time,
) []mysqlv1alpha1.SkippedTransaction {
	if retention <= 0 {
		return history
	}
	cutoff := now.Add(-retention)
	kept := history[:0:0] // fresh backing array, cap=0; never aliases history
	for _, e := range history {
		if !e.SkippedAt.Time.Before(cutoff) {
			kept = append(kept, e)
		}
	}
	return kept
}

// appendSkipsCapped appends entries to history, oldest-first, and returns the
// most-recent `cap` entries. Stable order preserved across reconciles.
func appendSkipsCapped(history []mysqlv1alpha1.SkippedTransaction,
	entries []mysqlv1alpha1.SkippedTransaction, capN int) []mysqlv1alpha1.SkippedTransaction {
	combined := append(append([]mysqlv1alpha1.SkippedTransaction(nil), history...), entries...)
	if len(combined) <= capN {
		return combined
	}
	return combined[len(combined)-capN:]
}

// replicationErrorLabelKey encodes the four label dimensions into a single
// string key used by activeReplicationErrorLabels. Format:
//
//	"<policyUID>\x00<role>\x00<channel>\x00<errno>"
//
// NUL (\x00) is used as the separator because it is valid in Go map keys but
// can never appear in a MySQL identifier (channel name) or UUID, making the
// key unambiguous even when a channel name contains ":" (which is valid in
// MySQL but would corrupt a colon-separated key).
func replicationErrorLabelKey(uid, role, channel, errnoStr string) string {
	return uid + "\x00" + role + "\x00" + channel + "\x00" + errnoStr
}

// parseReplicationErrorLabelKey is the inverse of replicationErrorLabelKey.
// It returns (uid, role, channel, errnoStr, ok). ok is false only when the
// key does not have the expected number of NUL-delimited segments.
//
// NUL (\x00) cannot appear in MySQL identifiers or UUIDs, so a simple
// SplitN with n=4 always produces exactly the four encoded fields regardless
// of whether the channel name contains ":" or other punctuation.
func parseReplicationErrorLabelKey(key string) (uid, role, channel, errnoStr string, ok bool) {
	parts := strings.SplitN(key, "\x00", 4)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

