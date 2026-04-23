//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Package-level DSNs that every integration test uses. Set by TestMain.
var (
	dcDSN string
	drDSN string

	// Channel name used across tests. The PreFlight helpers and the pxc
	// manager both default to reading a single named channel, so the same
	// name must be programmed into CHANGE REPLICATION SOURCE.
	channelName = "dc-to-dr"
)

// replUser / replPass are created on DC during setup and used by DR to
// subscribe. Hardcoded because the whole stack is ephemeral.
const (
	replUser = "keeper_repl"
	replPass = "keeper_repl_pw"
	rootPW   = "testpass"
)

// TestMain brings up two Percona 8.0 containers, wires them into an async
// replication channel, and caches their DSNs for the rest of the suite. A
// single panic ends the whole run — integration tests that cannot talk to
// MySQL have no useful answer to produce.
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			log.Fatalf("resolve %s: %v", p, err)
		}
		return a
	}

	dcC, err := mysql.RunContainer(ctx,
		testcontainers.WithImage("percona/percona-server:8.0"),
		mysql.WithDatabase("keeper"),
		mysql.WithUsername("root"),
		mysql.WithPassword(rootPW),
		mysql.WithConfigFile(abs("testdata/my-dc.cnf")),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready for connections").
				WithStartupTimeout(90*time.Second).
				WithOccurrence(2),
		),
	)
	if err != nil {
		log.Fatalf("start DC container: %v", err)
	}
	defer terminate(ctx, dcC, "dc")

	drC, err := mysql.RunContainer(ctx,
		testcontainers.WithImage("percona/percona-server:8.0"),
		mysql.WithDatabase("keeper"),
		mysql.WithUsername("root"),
		mysql.WithPassword(rootPW),
		mysql.WithConfigFile(abs("testdata/my-dr.cnf")),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready for connections").
				WithStartupTimeout(90*time.Second).
				WithOccurrence(2),
		),
	)
	if err != nil {
		log.Fatalf("start DR container: %v", err)
	}
	defer terminate(ctx, drC, "dr")

	dcHost, _ := dcC.Host(ctx)
	dcPort, _ := dcC.MappedPort(ctx, "3306/tcp")
	drHost, _ := drC.Host(ctx)
	drPort, _ := drC.MappedPort(ctx, "3306/tcp")

	// ContainerIP is used when configuring DR's CHANGE REPLICATION SOURCE so
	// DR can reach DC on the docker network. The mapped port is only valid
	// from the host, not from inside another container.
	dcContainerIP, err := dcC.ContainerIP(ctx)
	if err != nil {
		log.Fatalf("DC ContainerIP: %v", err)
	}

	dcDSN = fmt.Sprintf("root:%s@tcp(%s:%s)/", rootPW, dcHost, dcPort.Port())
	drDSN = fmt.Sprintf("root:%s@tcp(%s:%s)/", rootPW, drHost, drPort.Port())

	if err := setupAsyncReplication(ctx, dcDSN, drDSN, dcContainerIP); err != nil {
		log.Fatalf("setup replication: %v", err)
	}

	code := m.Run()
	os.Exit(code)
}

// setupAsyncReplication wires DC → DR with a GTID-based async channel. It
// creates the replication user on DC, grants the expected privileges, and
// issues CHANGE REPLICATION SOURCE on DR pointing at DC's container IP.
//
// Any failure here is fatal for the suite — a replication-based test cannot
// run without replication.
func setupAsyncReplication(ctx context.Context, dcDSN, drDSN, dcInternalIP string) error {
	dc, err := sql.Open("mysql", dcDSN)
	if err != nil {
		return fmt.Errorf("open DC: %w", err)
	}
	defer dc.Close()
	if err := dc.PingContext(ctx); err != nil {
		return fmt.Errorf("ping DC: %w", err)
	}

	// Replication grants — keep them idempotent so re-running the suite
	// against a cached container works.
	stmts := []string{
		fmt.Sprintf(`CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'`, replUser, replPass),
		fmt.Sprintf(`GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'`, replUser),
		`FLUSH PRIVILEGES`,
		`CREATE DATABASE IF NOT EXISTS smoketest`,
		`CREATE TABLE IF NOT EXISTS smoketest.pings (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			cluster_id VARCHAR(8) NOT NULL,
			ts DATETIME(6) NOT NULL DEFAULT NOW(6)
		)`,
	}
	for _, s := range stmts {
		if _, err := dc.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("DC stmt %q: %w", s, err)
		}
	}

	dr, err := sql.Open("mysql", drDSN)
	if err != nil {
		return fmt.Errorf("open DR: %w", err)
	}
	defer dr.Close()

	// Point DR at DC on the docker network. We use port 3306 (container
	// port), not the host-mapped one.
	change := fmt.Sprintf(`
		CHANGE REPLICATION SOURCE TO
			SOURCE_HOST='%s',
			SOURCE_PORT=3306,
			SOURCE_USER='%s',
			SOURCE_PASSWORD='%s',
			SOURCE_AUTO_POSITION=1,
			SOURCE_HEARTBEAT_PERIOD=2,
			GET_SOURCE_PUBLIC_KEY=1
		FOR CHANNEL '%s'`, dcInternalIP, replUser, replPass, channelName)
	drStmts := []string{
		fmt.Sprintf(`STOP REPLICA FOR CHANNEL '%s'`, channelName), // idempotent
		fmt.Sprintf(`RESET REPLICA ALL FOR CHANNEL '%s'`, channelName),
		change,
		fmt.Sprintf(`START REPLICA FOR CHANNEL '%s'`, channelName),
	}
	for _, s := range drStmts {
		if _, err := dr.ExecContext(ctx, s); err != nil {
			// STOP/RESET on an unconfigured channel returns an error — that
			// is expected on the first run. Only the START must succeed.
			if s == change || s == fmt.Sprintf(`START REPLICA FOR CHANNEL '%s'`, channelName) {
				return fmt.Errorf("DR stmt %q: %w", s, err)
			}
		}
	}

	// Sanity-wait: replication must become Running within 30s.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var ioState, sqlState sql.NullString
		err := dr.QueryRowContext(ctx, `
			SELECT service_state
			FROM performance_schema.replication_connection_status
			WHERE channel_name = ?`, channelName).Scan(&ioState)
		if err == nil && ioState.String == "ON" {
			err = dr.QueryRowContext(ctx, `
				SELECT service_state
				FROM performance_schema.replication_applier_status
				WHERE channel_name = ?`, channelName).Scan(&sqlState)
			if err == nil && sqlState.String == "ON" {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("replication did not become Running within 30s")
}

func terminate(ctx context.Context, c testcontainers.Container, label string) {
	if c == nil {
		return
	}
	if err := c.Terminate(ctx); err != nil {
		log.Printf("terminate %s: %v", label, err)
	}
}

// writeOnDC inserts N sentinel rows on DC. Returns the @@global.gtid_executed
// snapshot after the writes, so callers can assert DR's catch-up against an
// exact target.
func writeOnDC(ctx context.Context, t *testing.T, n int) string {
	t.Helper()
	dc, err := sql.Open("mysql", dcDSN)
	if err != nil {
		t.Fatalf("open DC: %v", err)
	}
	defer dc.Close()
	for i := 0; i < n; i++ {
		if _, err := dc.ExecContext(ctx,
			`INSERT INTO smoketest.pings (cluster_id) VALUES ('dc')`); err != nil {
			t.Fatalf("insert on DC: %v", err)
		}
	}
	var gtid string
	if err := dc.QueryRowContext(ctx, `SELECT @@GLOBAL.gtid_executed`).Scan(&gtid); err != nil {
		t.Fatalf("read DC gtid: %v", err)
	}
	return gtid
}
