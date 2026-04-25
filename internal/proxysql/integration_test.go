//go:build integration

package proxysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// proxySQLAdminCNF is injected into the container at /etc/proxysql.cnf.
// Monitor intervals are set to 1 hour so ProxySQL never marks backends SHUNNED
// (they aren't real MySQL nodes) — tests only validate routing table changes.
const proxySQLAdminCNF = `
datadir="/var/lib/proxysql"

admin_variables =
{
    admin_credentials = "admin:admin;keeper:keeperpass"
    mysql_ifaces = "0.0.0.0:6032"
    refresh_interval = 2000
}

mysql_variables =
{
    threads = 2
    max_connections = 512
    server_version = "8.0.35"
    interfaces = "0.0.0.0:6033"
    monitor_connect_interval = 3600000
    monitor_ping_interval    = 3600000
    monitor_read_only_interval = 3600000
    monitor_read_only_timeout  = 3600000
    monitor_username = "monitor"
    monitor_password = "unused"
}

mysql_servers =
(
    { hostgroup = 10, hostname = "old-writer", port = 3306, max_connections = 200, weight = 1000, status = "ONLINE" }
)
`

// startProxySQL launches a ProxySQL 2.7.1 container with the test CNF injected
// and returns the mapped admin port. Container is terminated via t.Cleanup.
func startProxySQL(t *testing.T) int {
	t.Helper()
	ctx := context.Background()

	cnfPath := filepath.Join(t.TempDir(), "proxysql.cnf")
	if err := os.WriteFile(cnfPath, []byte(proxySQLAdminCNF), 0644); err != nil {
		t.Fatalf("write proxysql.cnf: %v", err)
	}

	req := testcontainers.ContainerRequest{
		Image:        "proxysql/proxysql:2.7.1",
		ExposedPorts: []string{"6032/tcp"},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      cnfPath,
				ContainerFilePath: "/etc/proxysql.cnf",
				FileMode:          0644,
			},
		},
		WaitingFor: wait.ForListeningPort("6032/tcp").WithStartupTimeout(90 * time.Second),
	}

	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start proxysql container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	mappedPort, err := ctr.MappedPort(ctx, "6032/tcp")
	if err != nil {
		t.Fatalf("get proxysql admin port: %v", err)
	}
	portInt, err := strconv.Atoi(mappedPort.Port())
	if err != nil {
		t.Fatalf("parse proxysql admin port: %v", err)
	}
	return portInt
}

func adminConn(t *testing.T, port int) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("keeper:keeperpass@tcp(127.0.0.1:%d)/?interpolateParams=true", port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open proxysql admin: %v", err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { db.Close() })
	return db
}

type serverRow struct {
	hostgroup int32
	hostname  string
	port      int32
	status    string
	maxConns  int32
}

func queryRuntime(t *testing.T, db *sql.DB, hg int32) []serverRow {
	t.Helper()
	return queryServersFrom(t, db, "runtime_mysql_servers", hg)
}

// queryStaging queries the mysql_servers staging table (pre-LOAD state).
func queryStaging(t *testing.T, db *sql.DB, hg int32) []serverRow {
	t.Helper()
	return queryServersFrom(t, db, "mysql_servers", hg)
}

func queryServersFrom(t *testing.T, db *sql.DB, table string, hg int32) []serverRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		"SELECT hostgroup_id, hostname, port, status, max_connections FROM "+table+" WHERE hostgroup_id = ?",
		hg)
	if err != nil {
		t.Fatalf("query %s hg=%d: %v", table, hg, err)
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		var r serverRow
		if err := rows.Scan(&r.hostgroup, &r.hostname, &r.port, &r.status, &r.maxConns); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, r)
	}
	return out
}

func queryAllServers(t *testing.T, db *sql.DB, table string) []serverRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		"SELECT hostgroup_id, hostname, port, status, max_connections FROM "+table)
	if err != nil {
		t.Logf("queryAllServers %s: %v", table, err)
		return nil
	}
	defer rows.Close()
	var out []serverRow
	for rows.Next() {
		var r serverRow
		if err := rows.Scan(&r.hostgroup, &r.hostname, &r.port, &r.status, &r.maxConns); err != nil {
			t.Logf("scan %s: %v", table, err)
			continue
		}
		out = append(out, r)
	}
	return out
}

// TestProxySQLIntegration_ApplyFailoverRouting verifies that after a successful
// ApplyFailoverRouting the ProxySQL runtime routes writes to the new writer and
// the old writer is absent from the write hostgroup.
func TestProxySQLIntegration_ApplyFailoverRouting(t *testing.T) {
	port := startProxySQL(t)
	db := adminConn(t, port)
	ctx := context.Background()

	cfg := RoutingConfig{
		OldWriterHost:      "old-writer",
		OldWriterPort:      3306,
		NewWriterHost:      "new-writer",
		NewWriterPort:      3306,
		ReadWriteHostgroup: 10,
		ReadOnlyHostgroup:  20,
	}
	ep := Endpoint{Host: "127.0.0.1", Port: int32(port), Username: "keeper", Password: "keeperpass"}
	m := NewManager([]Endpoint{ep}, 15*time.Second)

	if err := m.ApplyFailoverRouting(ctx, cfg); err != nil {
		t.Fatalf("ApplyFailoverRouting: %v", err)
	}

	writeHG := queryRuntime(t, db, 10)
	for _, r := range writeHG {
		if r.hostname == "old-writer" {
			t.Errorf("old-writer still present in HG 10 after failover; rows=%+v", writeHG)
		}
	}
	hasNew := false
	for _, r := range writeHG {
		if r.hostname == "new-writer" {
			hasNew = true
		}
	}
	if !hasNew {
		t.Errorf("new-writer not found in HG 10 after failover; rows=%+v", writeHG)
	}

	readHG := queryRuntime(t, db, 20)
	hasNewRO := false
	for _, r := range readHG {
		if r.hostname == "new-writer" {
			hasNewRO = true
		}
	}
	if !hasNewRO {
		t.Errorf("new-writer not found in HG 20 after failover; rows=%+v", readHG)
	}
}

// TestProxySQLIntegration_Blackhole verifies that Blackhole moves the target
// into HG 9999 with OFFLINE_HARD + max_connections=0 in the ProxySQL runtime.
func TestProxySQLIntegration_Blackhole(t *testing.T) {
	port := startProxySQL(t)
	db := adminConn(t, port)
	ctx := context.Background()

	ep := Endpoint{Host: "127.0.0.1", Port: int32(port), Username: "keeper", Password: "keeperpass"}
	m := NewManager([]Endpoint{ep}, 15*time.Second)

	if err := m.Blackhole(ctx, BlackholeConfig{TargetHost: "old-writer", TargetPort: 3306}); err != nil {
		t.Fatalf("Blackhole: %v", err)
	}

	// ProxySQL excludes OFFLINE_HARD servers from runtime_mysql_servers entirely —
	// they are absent from the active pool, which IS the blackhole fence.
	// Verify: staging (mysql_servers) has the fenced entry, runtime HG 10/20 does not.
	stagingHG := queryStaging(t, db, 9999)
	if len(stagingHG) == 0 {
		t.Fatal("old-writer not in mysql_servers HG 9999 — blackhole config not applied to staging")
	}
	for _, r := range stagingHG {
		if r.hostname != "old-writer" {
			continue
		}
		if r.maxConns != 0 {
			t.Errorf("staging: old-writer max_connections=%d, want 0", r.maxConns)
		}
		if !strings.EqualFold(r.status, "OFFLINE_HARD") {
			t.Errorf("staging: old-writer status=%q, want OFFLINE_HARD", r.status)
		}
		break
	}

	// OFFLINE_HARD + LOAD means old-writer is absent from every active hostgroup
	// in runtime — it cannot receive connections.
	for _, hg := range []int32{10, 20} {
		for _, r := range queryRuntime(t, db, hg) {
			if r.hostname == "old-writer" {
				t.Errorf("old-writer still routable via HG %d after Blackhole", hg)
			}
		}
	}
}

// TestProxySQLIntegration_PreparePhaseAbort verifies the 2-phase invariant on
// real ProxySQL: when one of two endpoints is unreachable the prepare phase
// fails, LOAD is never called on the healthy instance, and runtime_mysql_servers
// is NOT modified.
func TestProxySQLIntegration_PreparePhaseAbort(t *testing.T) {
	port := startProxySQL(t)
	db := adminConn(t, port)
	ctx := context.Background()

	cfg := RoutingConfig{
		OldWriterHost:      "old-writer",
		OldWriterPort:      3306,
		NewWriterHost:      "new-writer",
		NewWriterPort:      3306,
		ReadWriteHostgroup: 10,
		ReadOnlyHostgroup:  20,
	}

	epReal := Endpoint{Host: "127.0.0.1", Port: int32(port), Username: "keeper", Password: "keeperpass"}
	// Port 1 is always refused; causes prepare to fail on the second endpoint.
	epDead := Endpoint{Host: "127.0.0.1", Port: 1, Username: "keeper", Password: "keeperpass"}
	m := NewManager([]Endpoint{epReal, epDead}, 3*time.Second)

	if err := m.ApplyFailoverRouting(ctx, cfg); err == nil {
		t.Fatal("expected error when one endpoint is unreachable, got nil")
	}

	// The 2-phase invariant: prepare DML ran on epReal (staging table modified)
	// but LOAD was never called (runtime_mysql_servers unchanged). Both halves
	// must be true to prove staging-vs-runtime separation rather than just
	// "nothing happened at all".

	// Half 1: staging table MUST reflect the prepare DML on the healthy instance.
	stagingHG := queryStaging(t, db, 10)
	hasNewInStaging := false
	for _, r := range stagingHG {
		if r.hostname == "new-writer" {
			hasNewInStaging = true
		}
	}
	if !hasNewInStaging {
		t.Errorf("new-writer absent from mysql_servers HG 10 — prepare did not run on healthy instance; staging=%+v", stagingHG)
	}

	// Half 2: runtime MUST be unchanged — LOAD was never called because prepare
	// aborted before the commit phase.
	runtimeHG := queryRuntime(t, db, 10)
	hasOldInRuntime := false
	for _, r := range runtimeHG {
		if r.hostname == "old-writer" {
			hasOldInRuntime = true
		}
	}
	if !hasOldInRuntime {
		t.Errorf("old-writer missing from runtime HG 10 — LOAD was applied despite prepare failure; runtime=%+v", runtimeHG)
	}
	for _, r := range runtimeHG {
		if r.hostname == "new-writer" {
			t.Errorf("new-writer in runtime HG 10 — LOAD was applied despite prepare failure")
		}
	}
}
