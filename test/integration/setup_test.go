//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Pass the GTID + replication settings as command-line arguments rather
	// than mounting a config file. testcontainers-go's WithConfigFile places
	// the file at /etc/mysql/conf.d/my.cnf, which the Percona image does not
	// pick up before the server initialises GTID_MODE. Command-line flags
	// override everything and make the container behaviour deterministic.
	dcCmd := []string{
		"--gtid-mode=ON",
		"--enforce-gtid-consistency=ON",
		"--log-bin=/var/lib/mysql/binlog",
		"--log-replica-updates=ON",
		"--binlog-format=ROW",
		"--binlog-expire-logs-seconds=604800",
		"--server-id=1",
	}
	drCmd := []string{
		"--gtid-mode=ON",
		"--enforce-gtid-consistency=ON",
		"--log-bin=/var/lib/mysql/binlog",
		"--log-replica-updates=ON",
		"--binlog-format=ROW",
		"--binlog-expire-logs-seconds=604800",
		"--server-id=2",
		// Do NOT set --read-only=ON at startup; the entrypoint needs write
		// access to create the root user. We flip read_only on after setup.
	}

	dcC, err := mysql.RunContainer(ctx,
		testcontainers.WithImage("percona/percona-server:8.0"),
		mysql.WithDatabase("keeper"),
		mysql.WithUsername("root"),
		mysql.WithPassword(rootPW),
		withCmd(dcCmd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready for connections").
				WithStartupTimeout(120*time.Second).
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
		withCmd(drCmd),
		testcontainers.WithWaitStrategy(
			wait.ForLog("ready for connections").
				WithStartupTimeout(120*time.Second).
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

	// Only after the user account exists and replication is configured do we
	// flip DR to read_only. Setting read_only via SQL (rather than the
	// --read-only startup flag) means the entrypoint was free to create the
	// root user at boot without hitting super_read_only guardrails.
	if err := enforceReadOnly(ctx, drDSN); err != nil {
		log.Fatalf("enforce read_only on DR: %v", err)
	}

	code := m.Run()
	os.Exit(code)
}

// withCmd appends extra CMD arguments to the container request. The mysql
// module does not expose a direct helper for this; CustomizeRequest mutates
// the raw ContainerRequest before the container starts.
func withCmd(args []string) testcontainers.ContainerCustomizer {
	return testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Cmd: args,
		},
	})
}

// enforceReadOnly flips DR into read_only=ON + super_read_only=ON. We do this
// in SQL, not in the startup flags, because the MySQL entrypoint needs to
// create the root user during first boot and super_read_only blocks that.
func enforceReadOnly(ctx context.Context, dsn string) error {
	db, err := openWithRetries(ctx, dsn, 30*time.Second)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, s := range []string{
		`SET GLOBAL super_read_only=ON`,
		`SET GLOBAL read_only=ON`,
	} {
		if err := execWithRetries(ctx, db, s, 5); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	return nil
}

// setupAsyncReplication wires DC → DR with a GTID-based async channel. It
// creates the replication user on DC, grants the expected privileges, and
// issues CHANGE REPLICATION SOURCE on DR pointing at DC's container IP.
//
// Any failure here is fatal for the suite — a replication-based test cannot
// run without replication.
func setupAsyncReplication(ctx context.Context, dcDSN, drDSN, dcInternalIP string) error {
	dc, err := openWithRetries(ctx, dcDSN, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open DC: %w", err)
	}
	defer dc.Close()

	// Replication grants — idempotent so re-running the suite against a
	// cached container works. Run each on a fresh dedicated connection to
	// avoid stale-pool EOFs; the container has enough slack during startup
	// to kill pooled connections that were opened during warm-up.
	for _, s := range []string{
		fmt.Sprintf(`CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED BY '%s'`, replUser, replPass),
		fmt.Sprintf(`GRANT REPLICATION SLAVE ON *.* TO '%s'@'%%'`, replUser),
		`FLUSH PRIVILEGES`,
		`CREATE DATABASE IF NOT EXISTS smoketest`,
		`CREATE TABLE IF NOT EXISTS smoketest.pings (
			id BIGINT AUTO_INCREMENT PRIMARY KEY,
			cluster_id VARCHAR(8) NOT NULL,
			ts DATETIME(6) NOT NULL DEFAULT NOW(6)
		)`,
	} {
		if err := execWithRetries(ctx, dc, s, 5); err != nil {
			return fmt.Errorf("DC stmt %q: %w", s, err)
		}
	}

	dr, err := openWithRetries(ctx, drDSN, 30*time.Second)
	if err != nil {
		return fmt.Errorf("open DR: %w", err)
	}
	defer dr.Close()

	// Best-effort idempotency — skip errors when the channel has never been
	// configured on this container. We pass context.Background() here so the
	// setup-wide timeout does not cancel the subsequent CHANGE REPLICATION
	// SOURCE if these take a few hundred ms.
	_, _ = dr.ExecContext(ctx, fmt.Sprintf(`STOP REPLICA FOR CHANNEL '%s'`, channelName))
	_, _ = dr.ExecContext(ctx, fmt.Sprintf(`RESET REPLICA ALL FOR CHANNEL '%s'`, channelName))

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
	if err := execWithRetries(ctx, dr, change, 5); err != nil {
		return fmt.Errorf("CHANGE REPLICATION SOURCE: %w", err)
	}
	if err := execWithRetries(ctx, dr,
		fmt.Sprintf(`START REPLICA FOR CHANNEL '%s'`, channelName), 5); err != nil {
		return fmt.Errorf("START REPLICA: %w", err)
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

// openWithRetries ping-loops until MySQL accepts connections. The
// "ready for connections" log the Percona image prints can fire before the
// auth subsystem is fully primed; pooled connections opened in that window
// are prone to EOF on the first real query.
func openWithRetries(ctx context.Context, dsn string, budget time.Duration) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Keep the pool small and short-lived during setup so we do not reuse a
	// connection that was negotiated before the server was really ready.
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Second)
	db.SetConnMaxIdleTime(5 * time.Second)

	deadline := time.Now().Add(budget)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			return db, nil
		}
		if time.Now().After(deadline) {
			db.Close()
			return nil, fmt.Errorf("ping within %s: %w", budget, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// execWithRetries retries transient "bad connection" / EOF errors that happen
// when the server recycles a pooled connection during startup. Production
// code runs long after warm-up and does not need this.
func execWithRetries(ctx context.Context, db *sql.DB, stmt string, attempts int) error {
	var lastErr error
	for i := 0; i < attempts; i++ {
		_, err := db.ExecContext(ctx, stmt)
		if err == nil {
			return nil
		}
		lastErr = err
		msg := err.Error()
		if !isTransientConnErr(msg) {
			return err
		}
		time.Sleep(time.Duration(200+(i*300)) * time.Millisecond)
	}
	return lastErr
}

func isTransientConnErr(msg string) bool {
	// go-sql-driver/mysql surfaces both "driver: bad connection" and
	// "invalid connection" when the server closes mid-handshake.
	return containsAny(msg,
		"bad connection",
		"invalid connection",
		"unexpected EOF",
		"connection reset by peer",
	)
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if len(n) > 0 && len(s) >= len(n) {
			if indexOf(s, n) >= 0 {
				return true
			}
		}
	}
	return false
}

// indexOf avoids importing strings just for the one call on the hot path.
func indexOf(s, substr string) int {
	n := len(substr)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= len(s); i++ {
		if s[i:i+n] == substr {
			return i
		}
	}
	return -1
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
