package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	mysqlv1alpha1 "github.com/duongnguyen/mysql-keeper/api/v1alpha1"
	"github.com/duongnguyen/mysql-keeper/internal/health"
)

// TestDecide_BothReadOnlyBlocksAuto maps S2/4.5 — the scenario that triggered
// the original production incident. When both clusters are RO, auto-failover
// must NOT fire.
func TestDecide_BothReadOnlyBlocksAuto(t *testing.T) {
	p := newPolicy()
	p.Status.ConsecutiveLocalFailures = 5 // way past threshold

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableNo, Healthy: false,
			Message: "local quorum lost"},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: false,
			Message: "remote quorum lost"},
		time.Now(),
	)
	if d.Should {
		t.Fatalf("expected BothReadOnly to block; got: %s", d.Reason)
	}
	if d.Blocker != "both_readonly" {
		t.Errorf("expected Blocker=both_readonly, got %q", d.Blocker)
	}
}

// TestDecide_CooldownBlocks maps S9/4.8 — the ping-pong guard.
func TestDecide_CooldownBlocks(t *testing.T) {
	p := newPolicy()
	p.Spec.Switchover.CooldownPeriod = metav1.Duration{Duration: 10 * time.Minute}
	p.Spec.ManualSwitchoverTarget = "promote-remote" // manual still honours cooldown
	recent := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	p.Status.LastSwitchoverTime = &recent

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableYes, Healthy: true},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: true},
		time.Now(),
	)
	if d.Should {
		t.Fatalf("expected cooldown to block; got: %s", d.Reason)
	}
	if d.Blocker != "cooldown" {
		t.Errorf("expected Blocker=cooldown, got %q", d.Blocker)
	}
}

// TestDecide_CooldownExpiredAllowsManual — once cooldown passes, the manual
// trigger is honoured again.
func TestDecide_CooldownExpiredAllowsManual(t *testing.T) {
	p := newPolicy()
	p.Spec.Switchover.CooldownPeriod = metav1.Duration{Duration: 10 * time.Minute}
	p.Spec.ManualSwitchoverTarget = "promote-remote"
	past := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	p.Status.LastSwitchoverTime = &past

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableYes, Healthy: true},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: true},
		time.Now(),
	)
	if !d.Should {
		t.Fatalf("expected manual switchover to proceed; got: %s", d.Reason)
	}
}

// TestDecide_AutoFailoverRequiresRemoteHealthy blocks the "promote a broken
// replica" path — remote is reachable + RO but replication is dead.
func TestDecide_AutoFailoverRequiresRemoteHealthy(t *testing.T) {
	p := newPolicy()
	p.Status.ConsecutiveLocalFailures = 5

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableUnknown, Healthy: false,
			Message: "local unreachable"},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: false,
			Message: "remote replication broken"},
		time.Now(),
	)
	if d.Should {
		t.Fatalf("expected auto-failover to be blocked; got: %s", d.Reason)
	}
	if d.Blocker != "remote_unhealthy" {
		t.Errorf("expected Blocker=remote_unhealthy, got %q", d.Blocker)
	}
}

// TestDecide_HappyPathAutoFailover — local unhealthy, remote RO + healthy,
// threshold exceeded, no cooldown. The controller should flip.
func TestDecide_HappyPathAutoFailover(t *testing.T) {
	p := newPolicy()
	p.Status.ConsecutiveLocalFailures = 5

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableUnknown, Healthy: false,
			Message: "local mysql ping timeout"},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: true},
		time.Now(),
	)
	if !d.Should {
		t.Fatalf("expected auto-failover to fire; got: %s (blocker=%s)", d.Reason, d.Blocker)
	}
}

// TestDecide_UnreachableOnlyAlsoTriggers covers the regression where
// killing local MySQL entirely (not "PXC up but reporting bad state")
// never crossed the failover threshold because ConsecutiveLocalFailures
// stayed at 0 while ConsecutiveLocalUnreachable grew unbounded.
func TestDecide_UnreachableOnlyAlsoTriggers(t *testing.T) {
	p := newPolicy()
	p.Status.ConsecutiveLocalFailures = 0      // local never reported "unhealthy"
	p.Status.ConsecutiveLocalUnreachable = 5   // only "can't reach" accumulated

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableUnknown, Healthy: false,
			Message: "dial tcp: connection refused"},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: true},
		time.Now(),
	)
	if !d.Should {
		t.Fatalf("expected failover on sustained unreachable; got: %s", d.Reason)
	}
	if !containsString(d.Reason, "unreachable") {
		t.Errorf("expected reason to mention 'unreachable'; got: %s", d.Reason)
	}
}

// containsString is a tiny helper — package already imports strings? Check
// import list first; if not we inline a loop.
func containsString(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestDecide_AutoFailoverDisabled — respects the kill switch.
func TestDecide_AutoFailoverDisabled(t *testing.T) {
	p := newPolicy()
	p.Spec.AutoFailover = false
	p.Status.ConsecutiveLocalFailures = 5

	d := EvaluateSwitchover(p,
		health.ClusterHealth{Writable: health.WritableUnknown, Healthy: false},
		health.ClusterHealth{Writable: health.WritableNo, Healthy: true},
		time.Now(),
	)
	if d.Should {
		t.Fatalf("expected autoFailover=false to block")
	}
}

// --- helpers --------------------------------------------------------------

func newPolicy() *mysqlv1alpha1.ClusterSwitchPolicy {
	return &mysqlv1alpha1.ClusterSwitchPolicy{
		Spec: mysqlv1alpha1.ClusterSwitchPolicySpec{
			ClusterRole:  mysqlv1alpha1.ClusterRoleDC,
			AutoFailover: true,
			HealthCheck: mysqlv1alpha1.HealthCheckConfig{
				FailureThreshold: 3,
			},
		},
	}
}
