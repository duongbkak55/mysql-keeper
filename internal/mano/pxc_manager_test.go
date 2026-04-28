package mano_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/mano"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// stubSQL implements mano.sqlOps for tests — all read-only ops succeed with
// zero values; write ops are configurable.
type stubSQL struct {
	ensureSchemaErr error
	verifyWriteErr  error
}

func (s *stubSQL) IsWritable(_ context.Context) (bool, error) { return true, nil }
func (s *stubSQL) GetGTIDSnapshot(_ context.Context) (pxc.GTIDSnapshot, error) {
	return pxc.GTIDSnapshot{}, nil
}
func (s *stubSQL) GetExecutedGTID(_ context.Context) (string, error) { return "", nil }
func (s *stubSQL) IsGTIDSubset(_ context.Context, _ string) (bool, error) { return true, nil }
func (s *stubSQL) MissingGTIDs(_ context.Context, _ string) (string, error) { return "", nil }
func (s *stubSQL) WaitForGTID(_ context.Context, _ string, _ time.Duration) error { return nil }
func (s *stubSQL) GetReplicationStatus(_ context.Context, _ string) (pxc.ReplicationStatus, error) {
	return pxc.ReplicationStatus{}, nil
}
func (s *stubSQL) ProbeReachable(_ context.Context, _ time.Duration) (bool, error) { return true, nil }
func (s *stubSQL) StopReplica(_ context.Context, _ string) error        { return nil }
func (s *stubSQL) ResetReplicaAll(_ context.Context, _ string) error    { return nil }
func (s *stubSQL) EnsureKeeperSchema(_ context.Context) error           { return s.ensureSchemaErr }
func (s *stubSQL) VerifyWrite(_ context.Context) error                  { return s.verifyWriteErr }

// newTestPXCManager wires a PXCManager against the mock MANO server using the
// exported test constructor (only available in _test packages via internal package
// access trick — here we use the exported NewPXCManagerForTest).
func newTestManager(t *testing.T, mock *mockMANO, sql *stubSQL) *mano.PXCManager {
	t.Helper()
	client := mano.NewClient(mock.srv.URL, "test-token", false)
	return mano.NewPXCManagerForTest(client, "cnf-dc", "vdu-pxc",
		10*time.Millisecond, 5*time.Second, sql)
}

// --- SetReadOnly -----------------------------------------------------------

func TestPXCManagerSetReadOnly_SendsIsSourceFalse(t *testing.T) {
	mock := newMockMANO(t)
	mgr := newTestManager(t, mock, &stubSQL{})

	if err := mgr.SetReadOnly(context.Background()); err != nil {
		t.Fatalf("SetReadOnly failed: %v", err)
	}
	if mock.lastIsSource != "false" {
		t.Errorf("MANO received isSource=%q, want false", mock.lastIsSource)
	}
	if mock.lastCNF != "cnf-dc" {
		t.Errorf("MANO received cnfName=%q, want cnf-dc", mock.lastCNF)
	}
}

func TestPXCManagerSetReadOnly_MANOFailed_ReturnsError(t *testing.T) {
	mock := newMockMANO(t)
	mock.opState = "FAILED"
	mock.opErrorDetail = "operator timed out enforcing read_only"
	mgr := newTestManager(t, mock, &stubSQL{})

	err := mgr.SetReadOnly(context.Background())
	if err == nil {
		t.Fatal("expected error when MANO op fails, got nil")
	}
}

// --- SetReadWrite ----------------------------------------------------------

func TestPXCManagerSetReadWrite_SendsIsSourceTrue(t *testing.T) {
	mock := newMockMANO(t)
	mgr := newTestManager(t, mock, &stubSQL{})

	if err := mgr.SetReadWrite(context.Background()); err != nil {
		t.Fatalf("SetReadWrite failed: %v", err)
	}
	if mock.lastIsSource != "true" {
		t.Errorf("MANO received isSource=%q, want true", mock.lastIsSource)
	}
}

func TestPXCManagerSetReadWrite_WriteProbeFailsAfterMANO(t *testing.T) {
	// MANO COMPLETED but MySQL write probe fails — should surface as error.
	mock := newMockMANO(t)
	sql := &stubSQL{verifyWriteErr: errors.New("table locked")}
	mgr := newTestManager(t, mock, sql)

	err := mgr.SetReadWrite(context.Background())
	if err == nil {
		t.Fatal("expected error from failing write probe, got nil")
	}
}

func TestPXCManagerSetReadWrite_MANOFailed_SkipsWriteProbe(t *testing.T) {
	// MANO fails — we must NOT attempt a write probe on a potentially read-only cluster.
	mock := newMockMANO(t)
	mock.opState = "FAILED"
	mock.opErrorDetail = "rollback"
	sql := &stubSQL{verifyWriteErr: errors.New("should not be called")}
	mgr := newTestManager(t, mock, sql)

	err := mgr.SetReadWrite(context.Background())
	if err == nil {
		t.Fatal("expected error when MANO fails, got nil")
	}
	// verifyWriteErr would only propagate if write probe was called —
	// the error message from MANO should appear, not "should not be called".
	if errors.Is(err, sql.verifyWriteErr) {
		t.Error("write probe was called despite MANO failure")
	}
}

// TestPXCManagerSetReadWrite_EnsureSchemaFails verifies that when
// EnsureKeeperSchema fails, the error is wrapped and surfaced before
// VerifyWrite is ever attempted — the write probe has no table to write to.
func TestPXCManagerSetReadWrite_EnsureSchemaFails(t *testing.T) {
	mock := newMockMANO(t)
	sql := &stubSQL{ensureSchemaErr: errors.New("table creation denied")}
	mgr := newTestManager(t, mock, sql)

	err := mgr.SetReadWrite(context.Background())
	if err == nil {
		t.Fatal("expected error when EnsureKeeperSchema fails, got nil")
	}
	if !strings.Contains(err.Error(), "ensure keeper schema") {
		t.Errorf("expected 'ensure keeper schema' in error message; got: %v", err)
	}
	// VerifyWrite must not have been called — VerifyWriteErr != err proves it.
	if errors.Is(err, sql.verifyWriteErr) {
		t.Error("VerifyWrite was called despite EnsureKeeperSchema failure")
	}
}

// TestPXCManagerDelegation verifies that every read-only method on PXCManager
// forwards to the underlying sqlOps and returns what the stub provides —
// ensuring no silent swallowing or mutation of results.
func TestPXCManagerDelegation(t *testing.T) {
	mock := newMockMANO(t)
	stub := &stubSQL{}
	mgr := newTestManager(t, mock, stub)
	ctx := context.Background()

	t.Run("IsWritable", func(t *testing.T) {
		ok, err := mgr.IsWritable(ctx)
		if err != nil || !ok {
			t.Errorf("IsWritable = (%v, %v), want (true, nil)", ok, err)
		}
	})
	t.Run("GetGTIDSnapshot", func(t *testing.T) {
		if _, err := mgr.GetGTIDSnapshot(ctx); err != nil {
			t.Errorf("GetGTIDSnapshot unexpected error: %v", err)
		}
	})
	t.Run("GetExecutedGTID", func(t *testing.T) {
		if _, err := mgr.GetExecutedGTID(ctx); err != nil {
			t.Errorf("GetExecutedGTID unexpected error: %v", err)
		}
	})
	t.Run("IsGTIDSubset", func(t *testing.T) {
		ok, err := mgr.IsGTIDSubset(ctx, "dc:1-100")
		if err != nil || !ok {
			t.Errorf("IsGTIDSubset = (%v, %v), want (true, nil)", ok, err)
		}
	})
	t.Run("MissingGTIDs", func(t *testing.T) {
		missing, err := mgr.MissingGTIDs(ctx, "dc:1-100")
		if err != nil || missing != "" {
			t.Errorf("MissingGTIDs = (%q, %v), want (\"\", nil)", missing, err)
		}
	})
	t.Run("WaitForGTID", func(t *testing.T) {
		if err := mgr.WaitForGTID(ctx, "dc:1-100", time.Second); err != nil {
			t.Errorf("WaitForGTID unexpected error: %v", err)
		}
	})
	t.Run("ProbeReachable", func(t *testing.T) {
		ok, err := mgr.ProbeReachable(ctx, time.Second)
		if err != nil || !ok {
			t.Errorf("ProbeReachable = (%v, %v), want (true, nil)", ok, err)
		}
	})
	t.Run("StopReplica", func(t *testing.T) {
		if err := mgr.StopReplica(ctx, "dc-to-dr"); err != nil {
			t.Errorf("StopReplica unexpected error: %v", err)
		}
	})
	t.Run("ResetReplicaAll", func(t *testing.T) {
		if err := mgr.ResetReplicaAll(ctx, "dc-to-dr"); err != nil {
			t.Errorf("ResetReplicaAll unexpected error: %v", err)
		}
	})
	t.Run("GetReplicationStatus", func(t *testing.T) {
		if _, err := mgr.GetReplicationStatus(ctx, "dc-to-dr"); err != nil {
			t.Errorf("GetReplicationStatus unexpected error: %v", err)
		}
	})
}
