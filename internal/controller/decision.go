// Pure-logic decision helpers extracted from Reconcile so they can be unit
// tested without bringing up a fake k8s client + secret fixture.
//
// Keep this file free of I/O — no API calls, no SQL. The decisions must be
// pure functions of (policy snapshot, local health, remote health, clock).
package controller

import (
	"fmt"
	"time"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/health"
)

// SwitchoverDecision is the result of evaluating whether to flip right now.
type SwitchoverDecision struct {
	// Should is true when the caller may proceed to the execute path.
	Should bool
	// Reason is populated on every branch — either the trigger that would
	// fire or the reason we refused. Always suitable for logging.
	Reason string
	// Blocker names the specific guard that refused the flip. Empty on the
	// "Should=true" path. Used to bump the metric label in decideSwitchover.
	Blocker string
}

// EvaluateSwitchover contains the deterministic part of the auto-failover
// decision, independent of controller state. It does NOT:
//   - read the peer lease (requires SQL)
//   - read the PreFlight result (runs inside the engine)
//
// These additional gates are applied in the reconciler on top of whatever
// this function returns.
func EvaluateSwitchover(
	policy *mysqlv1alpha1.ClusterSwitchPolicy,
	localH, remoteH health.ClusterHealth,
	now time.Time,
) SwitchoverDecision {
	// 1. Cooldown applies to both manual and automatic.
	if policy.Status.LastSwitchoverTime != nil {
		cooldown := policy.Spec.Switchover.CooldownPeriod.Duration
		if cooldown <= 0 {
			cooldown = 10 * time.Minute
		}
		since := now.Sub(policy.Status.LastSwitchoverTime.Time)
		if since < cooldown {
			return SwitchoverDecision{
				Should:  false,
				Reason:  fmt.Sprintf("cooldown active (sinceLast=%s < cooldown=%s)", since, cooldown),
				Blocker: "cooldown",
			}
		}
	}

	// 2. Manual trigger bypasses further auto-gates (but not cooldown).
	if policy.Spec.ManualSwitchoverTarget == "promote-remote" {
		return SwitchoverDecision{
			Should: true,
			Reason: "manual switchover requested via spec.manualSwitchoverTarget",
		}
	}

	// 3. Auto-failover path.
	if !policy.Spec.AutoFailover {
		return SwitchoverDecision{
			Should: false,
			Reason: "autoFailover disabled and no manual trigger set",
		}
	}
	if policy.Status.ConsecutiveLocalFailures < policy.Spec.HealthCheck.FailureThreshold {
		return SwitchoverDecision{
			Should: false,
			Reason: fmt.Sprintf("ConsecutiveLocalFailures=%d < threshold=%d",
				policy.Status.ConsecutiveLocalFailures,
				policy.Spec.HealthCheck.FailureThreshold),
		}
	}

	// 3a. Both-ReadOnly guard: cluster-wide incident, not a failover scenario.
	if localH.Writable == health.WritableNo && remoteH.Writable == health.WritableNo {
		return SwitchoverDecision{
			Should:  false,
			Reason:  "both clusters ReadOnly — cluster-wide incident, manual triage required",
			Blocker: "both_readonly",
		}
	}

	// 3b. Remote must be reachable AND read-only (the only state that is
	// safe to promote from).
	if remoteH.Writable != health.WritableNo {
		return SwitchoverDecision{
			Should: false,
			Reason: fmt.Sprintf("remote not safe to promote: writable=%s",
				remoteH.Writable.String()),
			Blocker: "remote_unsafe",
		}
	}

	// 3c. Remote must itself be healthy (replication running, etc.). A
	// promote-to-a-broken-replica is worse than doing nothing.
	if !remoteH.Healthy {
		return SwitchoverDecision{
			Should: false,
			Reason: fmt.Sprintf("remote unhealthy: %s", remoteH.Message),
			Blocker: "remote_unhealthy",
		}
	}

	return SwitchoverDecision{
		Should: true,
		Reason: fmt.Sprintf("automatic failover: local unhealthy for %d consecutive checks",
			policy.Status.ConsecutiveLocalFailures),
	}
}
