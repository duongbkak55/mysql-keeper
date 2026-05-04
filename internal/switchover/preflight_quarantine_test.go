package switchover

import (
	"strings"
	"testing"
)

// TestC12_NotQuarantined_Passes confirms the default state passes.
func TestC12_NotQuarantined_Passes(t *testing.T) {
	p := PreFlight{}
	c := p.checkReplicaNotQuarantined()
	if !c.Passed {
		t.Errorf("expected pass when not quarantined, got %+v", c)
	}
	if c.Severity != SeverityHard {
		t.Errorf("C12 must be Hard severity, got %s", c.Severity)
	}
	if c.Name != "C12_ReplicaNotQuarantined" {
		t.Errorf("expected check name C12_ReplicaNotQuarantined, got %q", c.Name)
	}
}

// TestC12_Quarantined_FailsHard ensures C12 blocks promotion of a quarantined
// replica with a self-explanatory message that mentions the clear annotation.
func TestC12_Quarantined_FailsHard(t *testing.T) {
	p := PreFlight{
		LocalReplicaQuarantined: true,
		LocalQuarantineReason:   "skip count 6 exceeded threshold 5 in window 1h",
	}
	c := p.checkReplicaNotQuarantined()
	if c.Passed {
		t.Errorf("expected fail when quarantined, got pass")
	}
	if c.Severity != SeverityHard {
		t.Errorf("expected hard severity, got %s", c.Severity)
	}
	if !strings.Contains(c.Message, "skip count 6") {
		t.Errorf("expected reason in message, got %q", c.Message)
	}
	if !strings.Contains(c.Message, "mysql.keeper.io/clear-quarantine") {
		t.Errorf("expected annotation hint in message, got %q", c.Message)
	}
}
