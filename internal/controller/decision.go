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

	// 3a. Remote-sustained-unreachable path.
	// Separate from the local-failure path below: here the LOCAL cluster is
	// healthy (it is the passive DR replica) and the REMOTE (active DC) has
	// gone dark at the TCP level for enough consecutive cycles to justify
	// assuming it is dead, not just temporarily partitioned.
	//
	// Requires AllowDataLossFailover=true because the remote cannot be queried
	// for GTID verification (preflight C5/C6 would be impossible to run).
	remoteUnreachThreshold := policy.Spec.HealthCheck.RemoteUnreachableThreshold
	if remoteUnreachThreshold > 0 {
		if policy.Status.ConsecutiveRemoteUnreachable >= remoteUnreachThreshold {
			if !policy.Spec.AllowDataLossFailover {
				return SwitchoverDecision{
					Should: false,
					Reason: fmt.Sprintf(
						"remote unreachable for %d consecutive checks (threshold=%d) but AllowDataLossFailover=false — enable it to allow auto-promotion without GTID verification",
						policy.Status.ConsecutiveRemoteUnreachable, remoteUnreachThreshold,
					),
					Blocker: "remote_unreachable_dataloss_guard",
				}
			}
			return SwitchoverDecision{
				Should: true,
				Reason: fmt.Sprintf(
					"automatic failover: remote sustained-unreachable for %d consecutive checks (threshold=%d)",
					policy.Status.ConsecutiveRemoteUnreachable, remoteUnreachThreshold,
				),
			}
		}
	}

	// 3b. Local-failure path.
	// Failover triggers on EITHER counter hitting the threshold:
	//   - ConsecutiveLocalFailures: MySQL reachable but reporting bad state
	//     (wsrep non-Primary, quorum lost, etc.)
	//   - ConsecutiveLocalUnreachable: can't even reach MySQL (pod down,
	//     network cut, TCP refused)
	//
	// Tracking them separately lets operators alert on the specific failure
	// mode, but either condition sustained past the threshold is worth a
	// failover. The previous design only looked at Failures, so a hard-down
	// local cluster (MySQL crashed / container killed) never tripped the
	// threshold — reported by users as "I killed local, state Unknown, but
	// switchover never fires."
	threshold := policy.Spec.HealthCheck.FailureThreshold
	failures := policy.Status.ConsecutiveLocalFailures
	unreach := policy.Status.ConsecutiveLocalUnreachable
	if failures < threshold && unreach < threshold {
		return SwitchoverDecision{
			Should: false,
			Reason: fmt.Sprintf(
				"local ok: failures=%d unreachable=%d (threshold=%d on both)",
				failures, unreach, threshold,
			),
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

	trigger := "unhealthy"
	worst := failures
	if unreach >= threshold && failures < threshold {
		trigger = "unreachable"
		worst = unreach
	} else if unreach > failures {
		worst = unreach
	}
	return SwitchoverDecision{
		Should: true,
		Reason: fmt.Sprintf("automatic failover: local %s for %d consecutive checks (failures=%d unreachable=%d threshold=%d)",
			trigger, worst, failures, unreach, threshold,
		),
	}
}
