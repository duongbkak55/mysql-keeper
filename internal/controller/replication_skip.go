// Replication-error auto-skip orchestration.
//
// Invoked from observeReplicationErrors after worker errors have been
// detected. Decides per-error whether to actually call SkipNextTransaction:
//
//   - feature must be enabled
//   - the local inspector must support replicationSkipper
//   - errno must be in the configured whitelist
//   - the failed GTID must be known
//   - rate limit must have headroom inside Window
//   - the replica must not already be quarantined
//   - dry-run short-circuits the SQL call but still emits events + metrics
//
// The function also re-evaluates quarantine state from history+new skips so
// updateReplicationErrorStatus can flip the ReplicaQuarantined condition.
//
// Race safety: controller-runtime leader election ensures only one Reconcile
// loop is active per cluster at a time. Cross-cluster races are not possible
// because at most one cluster acts as replica (the one observing errors)
// during normal operation.
package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/metrics"
)

// replicationSkipper is the subset of *pxc.Manager used for empty-transaction
// injection. Defined locally to keep the switchover-side ReplicationInspector
// interface free of mutator methods.
type replicationSkipper interface {
	SkipNextTransaction(ctx context.Context, channel, gtid string) error
}

// eventRecorderShim is the subset of record.EventRecorder needed by the
// replication-error reconciler. Defined as a local interface so unit tests
// can pass nil or a fake recorder without importing client-go.
type eventRecorderShim interface {
	Event(object runtime.Object, eventType, reason, message string)
}

// applyAutoSkip is the production entry point. It type-asserts the local
// inspector to replicationSkipper and forwards to applyAutoSkipWith, which
// tests can call directly with a fake skipper.
func (r *ClusterSwitchPolicyReconciler) applyAutoSkip(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	comps *componentSet,
	cfg mysqlv1alpha1.ReplicationErrorHandlingConfig,
	out *replicationErrorOutcome,
) {
	var skipper replicationSkipper
	if comps != nil && comps.localInspector != nil {
		skipper, _ = comps.localInspector.(replicationSkipper)
	}
	r.applyAutoSkipWith(ctx, policy, skipper, cfg, out, time.Now())
}

// applyAutoSkipWith is the testable core: takes a skipper directly so unit
// tests do not need to construct a full componentSet. Mutates `out` in place.
// Always evaluates quarantine state at the end so a recovered replica can fall
// out of quarantine on the next reconcile (note: quarantine itself is sticky,
// see evaluateQuarantine for the rule).
//
// All early-exit gates (disabled, missing skipper, no errors, quarantined)
// live in this function so the contract is consistent across both entry
// points: the wrapper above and direct test calls.
func (r *ClusterSwitchPolicyReconciler) applyAutoSkipWith(
	ctx context.Context,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	skipper replicationSkipper,
	cfg mysqlv1alpha1.ReplicationErrorHandlingConfig,
	out *replicationErrorOutcome,
	now time.Time,
) {
	role := policy.Spec.ClusterRole

	defer evaluateQuarantine(r.Recorder, policy, cfg, out, now)

	if !cfg.AutoSkip.Enabled {
		if len(out.AllErrors) > 0 {
			out.SkipBlocked["disabled"] += len(out.AllErrors)
			for range out.AllErrors {
				metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "disabled").Inc()
			}
		}
		return
	}
	if len(out.AllErrors) == 0 {
		return
	}
	if skipper == nil {
		out.SkipBlocked["unsupported_inspector"] += len(out.AllErrors)
		for range out.AllErrors {
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "unsupported_inspector").Inc()
		}
		return
	}

	// If the replica is currently quarantined, refuse skip until operator
	// clears it. Quarantine is a hard guard — operators must investigate
	// before any further data divergence is permitted.
	if isCurrentlyQuarantined(policy) {
		out.SkipBlocked["quarantined"] += len(out.AllErrors)
		for range out.AllErrors {
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "quarantined").Inc()
		}
		return
	}

	history := historicalSkips(policy)
	whitelist := whitelistSet(cfg.AutoSkip.ErrorCodeWhitelist)

	for _, errEntry := range out.AllErrors {
		errno := errEntry.Errno
		channel := errEntry.Channel
		gtid := errEntry.GTID

		if _, ok := whitelist[errno]; !ok {
			out.SkipBlocked["not_whitelisted"]++
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "not_whitelisted").Inc()
			continue
		}
		if gtid == "" {
			out.SkipBlocked["missing_gtid"]++
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "missing_gtid").Inc()
			if r.Recorder != nil {
				r.Recorder.Event(policy, corev1.EventTypeWarning, "ReplicationSkipNoGTID",
					fmt.Sprintf("cannot skip errno=%d on channel %q: failed GTID not exposed by performance_schema",
						errno, channel))
			}
			continue
		}

		// Rate-limit accounting includes the in-flight Skipped slice so we
		// stop after MaxSkipsPerWindow even when many errors fire at once.
		if countSkipsInWindow(history, out.Skipped, cfg.AutoSkip.Window.Duration, now) >=
			int(cfg.AutoSkip.MaxSkipsPerWindow) {
			out.SkipBlocked["rate_limited"]++
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "rate_limited").Inc()
			if r.Recorder != nil {
				r.Recorder.Event(policy, corev1.EventTypeWarning, "SkipRateLimited",
					fmt.Sprintf("skip rate limit %d/%s reached for channel %q",
						cfg.AutoSkip.MaxSkipsPerWindow,
						cfg.AutoSkip.Window.Duration.String(), channel))
			}
			continue
		}

		entry := mysqlv1alpha1.SkippedTransaction{
			GTID:      gtid,
			Errno:     errno,
			Message:   errEntry.Message,
			SkippedAt: metav1.NewTime(now),
			DryRun:    cfg.AutoSkip.DryRun,
		}

		if cfg.AutoSkip.DryRun {
			out.SkipBlocked["dry_run"]++
			metrics.ReplicationSkipBlockedTotal.WithLabelValues(role, "dry_run").Inc()
			metrics.ReplicationSkippedTotal.WithLabelValues(role,
				strconv.FormatInt(int64(errno), 10)).Inc()
			if r.Recorder != nil {
				r.Recorder.Event(policy, corev1.EventTypeNormal, "WouldSkipTransaction",
					fmt.Sprintf("dry-run: would skip gtid=%q errno=%d on channel %q",
						gtid, errno, channel))
			}
			out.Skipped = append(out.Skipped, entry)
			continue
		}

		if err := skipper.SkipNextTransaction(ctx, channel, gtid); err != nil {
			log.FromContext(ctx).Error(err, "SkipNextTransaction failed",
				"channel", channel, "gtid", gtid, "errno", errno)
			if r.Recorder != nil {
				r.Recorder.Event(policy, corev1.EventTypeWarning, "ReplicationSkipFailed",
					fmt.Sprintf("failed to skip gtid=%q errno=%d on channel %q: %v",
						gtid, errno, channel, err))
			}
			continue
		}
		metrics.ReplicationSkippedTotal.WithLabelValues(role,
			strconv.FormatInt(int64(errno), 10)).Inc()
		if r.Recorder != nil {
			// MySQL error messages on 1062/1032 may include row-level data.
			// Truncate to keep the event payload bounded and avoid leaking
			// large rows into kube-apiserver event storage.
			r.Recorder.Event(policy, corev1.EventTypeNormal, "ReplicationTransactionSkipped",
				fmt.Sprintf("skipped gtid=%q errno=%d on channel %q: %s",
					gtid, errno, channel, truncateMessage(errEntry.Message, 200)))
		}
		out.Skipped = append(out.Skipped, entry)
	}
}

// historicalSkips returns the recorded skip history from the CR status, or an
// empty slice when the field is unset.
func historicalSkips(policy *mysqlv1alpha1.ClusterSwitchPolicy) []mysqlv1alpha1.SkippedTransaction {
	if policy.Status.ReplicationErrors == nil {
		return nil
	}
	return policy.Status.ReplicationErrors.SkippedTransactions
}

// whitelistSet builds an O(1) lookup table from the configured slice.
func whitelistSet(list []int32) map[int32]struct{} {
	m := make(map[int32]struct{}, len(list))
	for _, e := range list {
		m[e] = struct{}{}
	}
	return m
}

// isCurrentlyQuarantined returns true when the CR was already quarantined by
// a previous reconcile and no operator action has cleared it.
func isCurrentlyQuarantined(policy *mysqlv1alpha1.ClusterSwitchPolicy) bool {
	r := policy.Status.ReplicationErrors
	if r == nil || r.QuarantinedSince == nil {
		return false
	}
	return true
}

// countSkipsInWindow counts the entries in history+pending that fall within
// the given window ending at `now`. Used for both rate-limit (Window) and
// quarantine (QuarantineWindow) accounting.
func countSkipsInWindow(
	history []mysqlv1alpha1.SkippedTransaction,
	pending []mysqlv1alpha1.SkippedTransaction,
	window time.Duration,
	now time.Time,
) int {
	if window <= 0 {
		return 0
	}
	cutoff := now.Add(-window)
	count := 0
	for _, e := range history {
		if !e.SkippedAt.Time.Before(cutoff) {
			count++
		}
	}
	for _, e := range pending {
		if !e.SkippedAt.Time.Before(cutoff) {
			count++
		}
	}
	return count
}

// evaluateQuarantine sets out.QuarantineActive based on combined skip count
// in QuarantineWindow vs MaxSkipBeforeQuarantine. Once entered, quarantine
// is sticky until the user clears it via the
// mysql.keeper.io/clear-quarantine annotation; the count drifting back below
// threshold by itself does NOT auto-release. Auto-release would defeat the
// purpose — the operator must look at the data divergence first.
//
// When the operator changes the clear-quarantine annotation value AND no
// active error is observed this cycle AND the skip count in QuarantineWindow
// is at or below threshold, the quarantine is cleared.
//
// If the annotation is observed but safety preconditions are not yet met,
// a ClearQuarantineRefused warning event is emitted and out.RefusedClearReason
// is set so updateReplicationErrorStatus can persist the (refused) annotation
// value — preventing event spam on subsequent reconciles for the same value.
func evaluateQuarantine(
	recorder eventRecorderShim,
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	cfg mysqlv1alpha1.ReplicationErrorHandlingConfig,
	out *replicationErrorOutcome,
	now time.Time,
) {
	wasQuarantined := isCurrentlyQuarantined(policy)
	if wasQuarantined && operatorRequestedClear(policy) {
		if safeToClearQuarantine(policy, cfg, out, now) {
			out.QuarantineActive = false
			return
		}
		// Operator asked to clear, but preconditions are not met. Mark the
		// refusal so the wrapper persists the annotation value and emit a
		// one-shot warning event for visibility.
		out.RefusedClearAnnotation = policy.Annotations[mysqlv1alpha1.AnnotationClearQuarantine]
		out.RefusedClearReason = whyClearRefused(policy, cfg, out, now)
		if recorder != nil {
			recorder.Event(policy, "Warning", "ClearQuarantineRefused",
				"clear-quarantine annotation refused: "+out.RefusedClearReason)
		}
	}
	if wasQuarantined {
		out.QuarantineActive = true
		if r := policy.Status.ReplicationErrors; r != nil && r.QuarantineReason != "" {
			out.QuarantineReason = r.QuarantineReason
		} else {
			out.QuarantineReason = "previously quarantined; awaiting operator clear"
		}
		return
	}
	count := countSkipsInWindow(historicalSkips(policy), out.Skipped,
		cfg.AutoSkip.QuarantineWindow.Duration, now)
	if int32(count) > cfg.AutoSkip.MaxSkipBeforeQuarantine {
		out.QuarantineActive = true
		out.QuarantineReason = fmt.Sprintf(
			"skip count %d exceeded threshold %d in window %s",
			count, cfg.AutoSkip.MaxSkipBeforeQuarantine,
			cfg.AutoSkip.QuarantineWindow.Duration.String())
	}
}

// operatorRequestedClear returns true when the
// mysql.keeper.io/clear-quarantine annotation has been added or changed since
// the controller last processed it. An empty annotation value is ignored
// (operators must set a non-empty value such as a timestamp for uniqueness).
func operatorRequestedClear(policy *mysqlv1alpha1.ClusterSwitchPolicy) bool {
	val := policy.Annotations[mysqlv1alpha1.AnnotationClearQuarantine]
	if val == "" {
		return false
	}
	last := ""
	if r := policy.Status.ReplicationErrors; r != nil {
		last = r.LastClearAnnotationValue
	}
	return val != last
}

// safeToClearQuarantine verifies the post-clear state is clean before letting
// the operator release the replica. Refuses while the replica still reports
// an active SQL apply error or while the QuarantineWindow count is still
// above the configured threshold (the burst that caused entry must have
// subsided).
func safeToClearQuarantine(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	cfg mysqlv1alpha1.ReplicationErrorHandlingConfig,
	out *replicationErrorOutcome,
	now time.Time,
) bool {
	if out.ActiveError != nil {
		return false
	}
	count := countSkipsInWindow(historicalSkips(policy), out.Skipped,
		cfg.AutoSkip.QuarantineWindow.Duration, now)
	return int32(count) <= cfg.AutoSkip.MaxSkipBeforeQuarantine
}

// whyClearRefused returns a human-readable explanation of why
// safeToClearQuarantine returned false. Used to populate the
// ClearQuarantineRefused event message and the RefusedClearReason outcome.
func whyClearRefused(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	cfg mysqlv1alpha1.ReplicationErrorHandlingConfig,
	out *replicationErrorOutcome,
	now time.Time,
) string {
	if out.ActiveError != nil {
		return fmt.Sprintf("active SQL applier error errno=%d still firing on channel %q",
			out.ActiveError.Errno, out.ActiveError.Channel)
	}
	count := countSkipsInWindow(historicalSkips(policy), out.Skipped,
		cfg.AutoSkip.QuarantineWindow.Duration, now)
	return fmt.Sprintf(
		"skip count %d still > threshold %d in window %s — wait for the burst to age out",
		count, cfg.AutoSkip.MaxSkipBeforeQuarantine,
		cfg.AutoSkip.QuarantineWindow.Duration.String())
}

// replicaQuarantined returns true when the CR currently believes the local
// replica is quarantined. Used by the engine config plumb for C12.
func replicaQuarantined(policy *mysqlv1alpha1.ClusterSwitchPolicy) bool {
	return isCurrentlyQuarantined(policy)
}

// replicaQuarantineReason returns the quarantine reason or empty when not
// quarantined. Used by the engine config plumb for C12.
func replicaQuarantineReason(policy *mysqlv1alpha1.ClusterSwitchPolicy) string {
	r := policy.Status.ReplicationErrors
	if r == nil {
		return ""
	}
	return r.QuarantineReason
}
