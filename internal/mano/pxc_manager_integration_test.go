//go:build integration

package mano_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/mano"
	_ "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startMySQLForMANO launches a plain MySQL 8.0 container and returns the DSN.
// GTID mode is not required here — we only need a live MySQL to test the write
// probe behaviour of PXCManager.
func startMySQLForMANO(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "mysql:8.0.39",
		Env:          map[string]string{"MYSQL_ROOT_PASSWORD": "testpass"},
		ExposedPorts: []string{"3306/tcp"},
		WaitingFor: wait.ForAll(
			wait.ForLog("ready for connections").WithOccurrence(2),
			wait.ForListeningPort("3306/tcp"),
		).WithStartupTimeout(120 * time.Second),
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start mysql container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	mappedPort, err := ctr.MappedPort(ctx, "3306/tcp")
	if err != nil {
		t.Fatalf("get mysql port: %v", err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("get mysql host: %v", err)
	}
	dsn := fmt.Sprintf("root:testpass@tcp(%s:%s)/?parseTime=true&loc=Local", host, mappedPort.Port())

	// Ping-retry: MySQL may log "ready" before auth-plugin init is complete.
	probe, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open probe db: %v", err)
	}
	defer probe.Close()
	probe.SetMaxOpenConns(1)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := probe.PingContext(ctx); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := probe.PingContext(ctx); err != nil {
		t.Fatalf("mysql not ready after 30s: %v", err)
	}
	return dsn
}

func setMySQLReadOnly(t *testing.T, dsn string, on bool) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	val := 0
	if on {
		val = 1
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("SET GLOBAL super_read_only=%d", val)); err != nil {
		t.Fatalf("set super_read_only=%d: %v", val, err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf("SET GLOBAL read_only=%d", val)); err != nil {
		t.Fatalf("set read_only=%d: %v", val, err)
	}
}

// TestPXCManagerIntegration_SetReadWrite_WriteProbeCatchesOperatorLag is the
// core missing integration: MANO reports COMPLETED but MySQL is still
// read-only (simulating a PXC Operator reconcile lag or a stuck operator pod).
// The write probe that PXCManager runs after MANO COMPLETED must surface this
// as an error — not silently succeed.
//
// This failure mode was observed in production and is the exact reason
// PXCManager.SetReadWrite calls VerifyWrite after polling COMPLETED.
// The stub-based unit test (TestPXCManagerSetReadWrite_WriteProbeFailsAfterMANO)
// covers the plumbing; this test confirms the behaviour on a real MySQL instance.
func TestPXCManagerIntegration_SetReadWrite_WriteProbeCatchesOperatorLag(t *testing.T) {
	dsn := startMySQLForMANO(t)
	ctx := context.Background()

	// Simulate the state just before promotion: MySQL is the current replica,
	// so it is read-only. MANO will say COMPLETED but the PXC Operator has not
	// yet reconciled the change onto MySQL.
	setMySQLReadOnly(t, dsn, true)

	mock := newMockMANO(t) // always returns operationState=COMPLETED
	mgr := mano.NewPXCManager(
		mano.NewClient(mock.srv.URL, "mock-token", false),
		"cnf-dc", "vdu-pxc",
		10*time.Millisecond, 5*time.Second,
		dsn, 10*time.Second,
	)

	err := mgr.SetReadWrite(ctx)
	if err == nil {
		t.Fatal("expected error: MANO completed but MySQL is still read-only (operator lag not detected)")
	}
	// The failure must originate from the write probe, not from the MANO HTTP call.
	if strings.Contains(err.Error(), "lcmOpOcc") || strings.Contains(err.Error(), "did not complete") {
		t.Errorf("error should be from the write probe, not the MANO call; got: %v", err)
	}
}

// TestPXCManagerIntegration_SetReadWrite_HappyPath verifies the normal
// promotion path: MANO COMPLETED + MySQL is writable (operator already applied
// the change). SetReadWrite must succeed and the write probe must confirm it.
func TestPXCManagerIntegration_SetReadWrite_HappyPath(t *testing.T) {
	dsn := startMySQLForMANO(t)
	ctx := context.Background()

	mock := newMockMANO(t)
	mgr := mano.NewPXCManager(
		mano.NewClient(mock.srv.URL, "mock-token", false),
		"cnf-dc", "vdu-pxc",
		10*time.Millisecond, 5*time.Second,
		dsn, 10*time.Second,
	)

	if err := mgr.SetReadWrite(ctx); err != nil {
		t.Fatalf("SetReadWrite: %v", err)
	}
	if mock.lastIsSource != "true" {
		t.Errorf("MANO received isSource=%q, want true", mock.lastIsSource)
	}
}

// TestPXCManagerIntegration_SetReadOnly_OnlyCallsMANO verifies that
// SetReadOnly fires the MANO API call and does NOT issue SQL fencing directly
// to MySQL. Enforcing read_only is the PXC Operator's responsibility;
// mysql-keeper only tells MANO to trigger the reconcile.
func TestPXCManagerIntegration_SetReadOnly_OnlyCallsMANO(t *testing.T) {
	dsn := startMySQLForMANO(t)
	ctx := context.Background()

	mock := newMockMANO(t)
	mgr := mano.NewPXCManager(
		mano.NewClient(mock.srv.URL, "mock-token", false),
		"cnf-dc", "vdu-pxc",
		10*time.Millisecond, 5*time.Second,
		dsn, 10*time.Second,
	)

	if err := mgr.SetReadOnly(ctx); err != nil {
		t.Fatalf("SetReadOnly: %v", err)
	}
	if mock.lastIsSource != "false" {
		t.Errorf("MANO received isSource=%q, want false", mock.lastIsSource)
	}

	// MySQL must remain writable — SetReadOnly must NOT apply SQL read_only=ON
	// directly. That is the PXC Operator's job after it sees the CRD change.
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var ro int
	if err := db.QueryRowContext(ctx, "SELECT @@read_only").Scan(&ro); err != nil {
		t.Fatalf("read read_only: %v", err)
	}
	if ro != 0 {
		t.Errorf("SetReadOnly must not fence MySQL directly; read_only=%d, want 0", ro)
	}
}
