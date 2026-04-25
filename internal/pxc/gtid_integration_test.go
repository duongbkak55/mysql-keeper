//go:build integration

package pxc_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	_ "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// startMySQLWithGTID launches a MySQL 8.0 container with GTID mode enabled
// and returns the DSN for root access. Container is terminated via t.Cleanup.
func startMySQLWithGTID(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image: "mysql:8.0.39",
		Env: map[string]string{
			"MYSQL_ROOT_PASSWORD": "testpass",
		},
		ExposedPorts: []string{"3306/tcp"},
		Cmd: []string{
			"--gtid-mode=ON",
			"--enforce-gtid-consistency=ON",
			"--log-bin=mysql-bin",
			"--log-replica-updates=ON",
			"--binlog-format=ROW",
			"--server-id=1",
			"--skip-name-resolve",
		},
		// MySQL 8.0 logs "ready for connections" twice (once during init, once
		// when fully ready) but may still be finishing auth-plugin setup after
		// the second log line. ForAll ensures the port is also accepting TCP
		// connections before we declare the container ready.
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

	// Ping-retry: the port can be listening while MySQL is still completing
	// auth-plugin initialisation — wait up to 30 s for the first clean Ping.
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

// TestMySQLIntegration_WaitForGTID_ImmediateSuccess verifies that WaitForGTID
// returns nil when the target GTID set is already in @@GLOBAL.gtid_executed.
// This is the normal "replica fully caught up" path.
func TestMySQLIntegration_WaitForGTID_ImmediateSuccess(t *testing.T) {
	dsn := startMySQLWithGTID(t)
	ctx := context.Background()

	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer raw.Close()

	if _, err := raw.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS keeper"); err != nil {
		t.Fatalf("seed DDL to generate GTID: %v", err)
	}

	var executed string
	if err := raw.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_executed").Scan(&executed); err != nil {
		t.Fatalf("read gtid_executed: %v", err)
	}
	if executed == "" {
		t.Fatal("gtid_executed is empty — GTID mode did not activate")
	}

	mgr := pxc.NewRemoteManager(dsn, 10*time.Second)
	if err := mgr.WaitForGTID(ctx, executed, 5*time.Second); err != nil {
		t.Fatalf("WaitForGTID with already-executed GTID set returned error: %v", err)
	}
}

// TestMySQLIntegration_WaitForGTID_Timeout verifies that WaitForGTID returns
// an error when the target GTID will never be committed on this standalone node.
func TestMySQLIntegration_WaitForGTID_Timeout(t *testing.T) {
	dsn := startMySQLWithGTID(t)
	ctx := context.Background()

	// A syntactically valid GTID that no standalone node will ever commit.
	ghost := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee:1-9999"

	mgr := pxc.NewRemoteManager(dsn, 10*time.Second)
	err := mgr.WaitForGTID(ctx, ghost, 1*time.Second)
	if err == nil {
		t.Fatal("expected error for unexecutable GTID, got nil")
	}
	if !strings.Contains(err.Error(), "did not reach target GTID within") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestMySQLIntegration_Manager_ReadWriteLifecycle exercises the full fence
// cycle on a plain MySQL 8.0 node (no PXC/wsrep):
//
//	EnsureKeeperSchema → IsWritable=true → SetReadOnly → IsWritable=false →
//	SetReadWrite → VerifyWrite → IsWritable=true
//
// EnsureKeeperSchema must run before SetReadOnly because SetReadWrite calls
// VerifyWrite internally, which requires keeper.probe to already exist.
func TestMySQLIntegration_Manager_ReadWriteLifecycle(t *testing.T) {
	dsn := startMySQLWithGTID(t)
	ctx := context.Background()
	mgr := pxc.NewRemoteManager(dsn, 10*time.Second)

	// Create keeper.probe while the node is writable so that the internal
	// VerifyWrite call inside SetReadWrite has a table to write to.
	if err := mgr.EnsureKeeperSchema(ctx); err != nil {
		t.Fatalf("EnsureKeeperSchema (initial): %v", err)
	}

	writable, err := mgr.IsWritable(ctx)
	if err != nil {
		t.Fatalf("IsWritable (initial): %v", err)
	}
	if !writable {
		t.Fatal("freshly started MySQL should be writable")
	}

	if err := mgr.SetReadOnly(ctx); err != nil {
		t.Fatalf("SetReadOnly: %v", err)
	}
	writable, err = mgr.IsWritable(ctx)
	if err != nil {
		t.Fatalf("IsWritable (after SetReadOnly): %v", err)
	}
	if writable {
		t.Fatal("MySQL should be read-only after SetReadOnly")
	}

	// SetReadWrite turns off super_read_only+read_only then calls VerifyWrite
	// internally — keeper.probe must exist at this point (created above).
	if err := mgr.SetReadWrite(ctx); err != nil {
		t.Fatalf("SetReadWrite: %v", err)
	}
	if err := mgr.VerifyWrite(ctx); err != nil {
		t.Fatalf("VerifyWrite: %v", err)
	}

	writable, err = mgr.IsWritable(ctx)
	if err != nil {
		t.Fatalf("IsWritable (after SetReadWrite): %v", err)
	}
	if !writable {
		t.Fatal("MySQL should be writable after SetReadWrite")
	}
}
