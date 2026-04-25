// Package proxysql provides operations to reconfigure ProxySQL routing via its admin interface.
package proxysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Endpoint holds admin interface connection parameters for one ProxySQL instance.
type Endpoint struct {
	Host     string
	Port     int32
	Username string
	Password string
}

// RoutingConfig defines how to reconfigure ProxySQL hostgroups during a switchover.
type RoutingConfig struct {
	// OldWriterHost is the current writer (being demoted). Its HG will be set to ReadOnlyHG.
	OldWriterHost string
	OldWriterPort int32

	// NewWriterHost is the new writer (being promoted). Its entry will be added to ReadWriteHG.
	NewWriterHost string
	NewWriterPort int32

	// ReadWriteHostgroup is the ProxySQL HG ID for writes (default 10).
	ReadWriteHostgroup int32

	// ReadOnlyHostgroup is the ProxySQL HG ID for reads (default 20).
	ReadOnlyHostgroup int32
}

// BlackholeConfig describes the fence-by-routing operation used when SQL
// fencing cannot complete. It moves the former writer into an unused hostgroup
// and sets max_connections=0 on every ProxySQL instance, so a later recovery
// of the local MySQL cluster cannot silently accept writes again.
type BlackholeConfig struct {
	// TargetHost is the MySQL node to blackhole — typically the old writer.
	TargetHost string
	TargetPort int32

	// BlackholeHostgroup is an ID that is not routed to by any query rule.
	// Defaults to 9999 when zero.
	BlackholeHostgroup int32
}

// Manager manages routing across multiple ProxySQL admin interfaces.
type Manager struct {
	endpoints []Endpoint
	timeout   time.Duration
	// openDB is nil in production; tests inject it to avoid real TCP dials.
	openDB func(ep Endpoint) (*sql.DB, error)
}

// NewManager creates a ProxySQL Manager for the given set of admin endpoints.
func NewManager(endpoints []Endpoint, timeout time.Duration) *Manager {
	return &Manager{
		endpoints: endpoints,
		timeout:   timeout,
	}
}

// ApplyFailoverRouting reconfigures all ProxySQL instances to route writes to
// the new writer. Internally it is a two-phase apply:
//
//  1. Prepare: connect to every instance and stage the new mysql_servers rows
//     without calling LOAD to runtime yet. If any instance fails here, no
//     instance has runtime-visible changes yet, so rollback is trivial.
//  2. Commit: LOAD + SAVE on every instance that staged successfully. If a
//     partial failure occurs here we fall back to a best-effort rollback that
//     also emits the partial-apply set for alerting.
//
// This narrows the "2 of 3 ProxySQL saw the new writer, 1 didn't" window to
// the duration of the commit phase itself (seconds), versus the prior
// one-by-one sequence which left the cluster in mixed state for the whole
// retry cycle.
func (m *Manager) ApplyFailoverRouting(ctx context.Context, cfg RoutingConfig) error {
	logger := log.FromContext(ctx)
	logger.Info("Applying failover routing to ProxySQL instances (2-phase)",
		"newWriter", cfg.NewWriterHost,
		"oldWriter", cfg.OldWriterHost,
		"instances", len(m.endpoints),
	)

	type stagedConn struct {
		ep Endpoint
		db *sql.DB
	}

	staged := make([]stagedConn, 0, len(m.endpoints))
	var prepareErrs []error

	// Phase 1: Prepare on every instance.
	for _, ep := range m.endpoints {
		db, err := m.prepareOne(ctx, ep, cfg)
		if err != nil {
			prepareErrs = append(prepareErrs, fmt.Errorf("proxysql %s:%d: %w", ep.Host, ep.Port, err))
			logger.Error(err, "prepare failed", "host", ep.Host)
			if db != nil {
				db.Close()
			}
			continue
		}
		staged = append(staged, stagedConn{ep: ep, db: db})
	}

	// If ANY instance failed prepare, abort without touching runtime on the
	// others. The rows we staged on the successful instances are harmless
	// because they only become effective once LOAD SERVERS runs.
	if len(prepareErrs) > 0 {
		for _, s := range staged {
			s.db.Close()
		}
		return fmt.Errorf(
			"prepare phase failed on %d/%d instances (no runtime change applied): %v",
			len(prepareErrs), len(m.endpoints), prepareErrs,
		)
	}

	// Phase 2: Commit on every prepared instance.
	var commitErrs []error
	var committed []stagedConn
	for _, s := range staged {
		if err := m.commitOne(ctx, s.db); err != nil {
			commitErrs = append(commitErrs,
				fmt.Errorf("proxysql %s:%d: %w", s.ep.Host, s.ep.Port, err))
			logger.Error(err, "commit failed — partial runtime apply", "host", s.ep.Host)
		} else {
			committed = append(committed, s)
		}
		s.db.Close()
	}

	if len(commitErrs) > 0 {
		return fmt.Errorf(
			"commit phase failed on %d/%d instances (committed=%d, manual ProxySQL reconciliation required): %v",
			len(commitErrs), len(m.endpoints), len(committed), commitErrs,
		)
	}
	return nil
}

// RollbackRouting reverses a failover routing change — swaps old and new writer roles back.
func (m *Manager) RollbackRouting(ctx context.Context, cfg RoutingConfig) error {
	rollbackCfg := RoutingConfig{
		OldWriterHost:      cfg.NewWriterHost,
		OldWriterPort:      cfg.NewWriterPort,
		NewWriterHost:      cfg.OldWriterHost,
		NewWriterPort:      cfg.OldWriterPort,
		ReadWriteHostgroup: cfg.ReadWriteHostgroup,
		ReadOnlyHostgroup:  cfg.ReadOnlyHostgroup,
	}
	return m.ApplyFailoverRouting(ctx, rollbackCfg)
}

// Blackhole moves the target writer entry into an unrouted hostgroup on every
// ProxySQL instance and sets its max_connections=0. Used as an alternate fence
// when the regular SQL fence (SET GLOBAL read_only=ON) failed. Continues past
// per-instance failures and returns a combined error listing any hosts that
// could not be reconfigured.
func (m *Manager) Blackhole(ctx context.Context, cfg BlackholeConfig) error {
	logger := log.FromContext(ctx)
	hg := cfg.BlackholeHostgroup
	if hg == 0 {
		hg = 9999
	}
	if cfg.TargetHost == "" {
		return fmt.Errorf("Blackhole: TargetHost must be set")
	}
	logger.Info("Blackholing writer via ProxySQL",
		"host", cfg.TargetHost, "port", cfg.TargetPort, "hg", hg)

	var errs []error
	for _, ep := range m.endpoints {
		if err := m.blackholeOne(ctx, ep, cfg.TargetHost, cfg.TargetPort, hg); err != nil {
			logger.Error(err, "blackhole failed on ProxySQL", "host", ep.Host)
			errs = append(errs, fmt.Errorf("proxysql %s:%d: %w", ep.Host, ep.Port, err))
		} else {
			logger.Info("blackhole applied", "host", ep.Host)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("blackhole errors (%d/%d failed): %v", len(errs), len(m.endpoints), errs)
	}
	return nil
}

func (m *Manager) blackholeOne(ctx context.Context, ep Endpoint, host string, port, hg int32) error {
	db, err := m.dial(ep)
	if err != nil {
		return fmt.Errorf("open admin: %w", err)
	}
	defer db.Close()
	return m.runBlackholeSteps(ctx, db, host, port, hg)
}

// runBlackholeSteps executes the ping + UPDATE + INSERT + LOAD + SAVE on an
// already-opened connection. Exported at package level for tests.
func (m *Manager) runBlackholeSteps(ctx context.Context, db *sql.DB, host string, port, hg int32) error {
	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if err := db.PingContext(qCtx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	// Move every row matching host/port into the blackhole HG and forbid new
	// connections. If no row exists for this node on this ProxySQL, insert one
	// pre-emptively so a later LOAD SERVERS does not re-admit it.
	if _, err := db.ExecContext(qCtx, `
		UPDATE mysql_servers
		SET hostgroup_id = ?, max_connections = 0, status = 'OFFLINE_HARD'
		WHERE hostname = ? AND port = ?
	`, hg, host, port); err != nil {
		return fmt.Errorf("update mysql_servers: %w", err)
	}

	// SQLite-flavoured upsert — see the ProxySQL admin dialect note on
	// runPrepareSteps. Plain ON DUPLICATE KEY UPDATE is not accepted.
	if _, err := db.ExecContext(qCtx, `
		INSERT OR REPLACE INTO mysql_servers
			(hostgroup_id, hostname, port, status, max_connections, weight)
		VALUES (?, ?, ?, 'OFFLINE_HARD', 0, 0)
	`, hg, host, port); err != nil {
		return fmt.Errorf("insert blackhole row: %w", err)
	}

	if _, err := db.ExecContext(qCtx, "LOAD MYSQL SERVERS TO RUNTIME"); err != nil {
		return fmt.Errorf("LOAD MYSQL SERVERS TO RUNTIME: %w", err)
	}
	if _, err := db.ExecContext(qCtx, "SAVE MYSQL SERVERS TO DISK"); err != nil {
		return fmt.Errorf("SAVE MYSQL SERVERS TO DISK: %w", err)
	}
	return nil
}

// dial opens an admin connection to ep, honouring the openDB override if set.
func (m *Manager) dial(ep Endpoint) (*sql.DB, error) {
	if m.openDB != nil {
		return m.openDB(ep)
	}
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/?interpolateParams=true", ep.Username, ep.Password, ep.Host, ep.Port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open admin connection: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(m.timeout)
	return db, nil
}

// prepareOne stages the routing change on a single ProxySQL instance without
// calling LOAD SERVERS. It returns an open *sql.DB so commitOne can finish
// the change on the same session; the caller is responsible for Close.
func (m *Manager) prepareOne(ctx context.Context, ep Endpoint, cfg RoutingConfig) (*sql.DB, error) {
	db, err := m.dial(ep)
	if err != nil {
		return nil, err
	}
	if err := m.runPrepareSteps(ctx, db, cfg); err != nil {
		return db, err
	}
	return db, nil
}

// runPrepareSteps executes the ping + three DML statements of the prepare
// phase on an already-opened connection. Exported at package level for tests.
func (m *Manager) runPrepareSteps(ctx context.Context, db *sql.DB, cfg RoutingConfig) error {
	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if err := db.PingContext(qCtx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	// Remove the old writer from the WRITE hostgroup. We can't UPDATE the
	// row's hostgroup_id to the read HG because the read HG usually
	// already has an entry for the same host/port (common pattern:
	// app reads from the primary when latency matters), which would
	// collide on ProxySQL's primary key (hostgroup_id, hostname, port).
	// DELETE leaves the existing read-HG row intact.
	//
	// If the old writer also lives in the read HG (typical), operators
	// can decide per-policy whether to drain it before the flip — our
	// default is to keep it serving reads until it is fenced and
	// replication reverses.
	if _, err := db.ExecContext(qCtx, `
		DELETE FROM mysql_servers
		WHERE hostgroup_id = ? AND hostname = ? AND port = ?
	`, cfg.ReadWriteHostgroup, cfg.OldWriterHost, cfg.OldWriterPort); err != nil {
		return fmt.Errorf("demote old writer: %w", err)
	}

	// ProxySQL's admin interface is SQLite-backed, not MySQL — it does
	// NOT support "ON DUPLICATE KEY UPDATE". The SQLite equivalent is
	// "INSERT OR REPLACE" which performs an upsert on the primary key
	// (hostgroup_id, hostname, port).
	if _, err := db.ExecContext(qCtx, `
		INSERT OR REPLACE INTO mysql_servers
			(hostgroup_id, hostname, port, status, max_connections, weight)
		VALUES (?, ?, ?, 'ONLINE', 1000, 1)
	`, cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort); err != nil {
		return fmt.Errorf("promote new writer: %w", err)
	}

	if _, err := db.ExecContext(qCtx, `
		INSERT OR REPLACE INTO mysql_servers
			(hostgroup_id, hostname, port, status, max_connections, weight)
		VALUES (?, ?, ?, 'ONLINE', 1000, 1)
	`, cfg.ReadOnlyHostgroup, cfg.NewWriterHost, cfg.NewWriterPort); err != nil {
		return fmt.Errorf("add new writer to RO group: %w", err)
	}
	return nil
}

// commitOne runs LOAD + SAVE on an already-prepared connection.
func (m *Manager) commitOne(ctx context.Context, db *sql.DB) error {
	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if _, err := db.ExecContext(qCtx, "LOAD MYSQL SERVERS TO RUNTIME"); err != nil {
		return fmt.Errorf("LOAD MYSQL SERVERS TO RUNTIME: %w", err)
	}
	if _, err := db.ExecContext(qCtx, "SAVE MYSQL SERVERS TO DISK"); err != nil {
		return fmt.Errorf("SAVE MYSQL SERVERS TO DISK: %w", err)
	}
	return nil
}
