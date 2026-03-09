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

// Manager manages routing across multiple ProxySQL admin interfaces.
type Manager struct {
	endpoints []Endpoint
	timeout   time.Duration
}

// NewManager creates a ProxySQL Manager for the given set of admin endpoints.
func NewManager(endpoints []Endpoint, timeout time.Duration) *Manager {
	return &Manager{
		endpoints: endpoints,
		timeout:   timeout,
	}
}

// ApplyFailoverRouting reconfigures all ProxySQL instances to route writes to the new writer.
// On partial failure, it continues to update all reachable instances and returns a combined error.
func (m *Manager) ApplyFailoverRouting(ctx context.Context, cfg RoutingConfig) error {
	logger := log.FromContext(ctx)
	logger.Info("Applying failover routing to ProxySQL instances",
		"newWriter", cfg.NewWriterHost,
		"oldWriter", cfg.OldWriterHost,
	)

	var errs []error
	for _, ep := range m.endpoints {
		if err := m.applyToOne(ctx, ep, cfg); err != nil {
			logger.Error(err, "Failed to update ProxySQL instance", "host", ep.Host)
			errs = append(errs, fmt.Errorf("proxysql %s:%d: %w", ep.Host, ep.Port, err))
		} else {
			logger.Info("ProxySQL instance updated", "host", ep.Host)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("ProxySQL update errors (%d/%d failed): %v", len(errs), len(m.endpoints), errs)
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

// applyToOne connects to one ProxySQL admin port and applies the routing change.
func (m *Manager) applyToOne(ctx context.Context, ep Endpoint, cfg RoutingConfig) error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/", ep.Username, ep.Password, ep.Host, ep.Port)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("open admin connection: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(m.timeout)

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if err := db.PingContext(qCtx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}

	// Step 1: Remove old writer from RW hostgroup (move to RO or disable).
	// Set max_connections=0 on old writer to drain connections gracefully.
	_, err = db.ExecContext(qCtx, `
		UPDATE mysql_servers
		SET hostgroup_id = ?, max_connections = 0
		WHERE hostname = ? AND port = ?
	`, cfg.ReadOnlyHostgroup, cfg.OldWriterHost, cfg.OldWriterPort)
	if err != nil {
		return fmt.Errorf("demote old writer: %w", err)
	}

	// Step 2: Insert or update new writer into RW hostgroup.
	_, err = db.ExecContext(qCtx, `
		INSERT INTO mysql_servers (hostgroup_id, hostname, port, status, max_connections, weight)
		VALUES (?, ?, ?, 'ONLINE', 1000, 1)
		ON DUPLICATE KEY UPDATE
			hostgroup_id   = VALUES(hostgroup_id),
			status         = 'ONLINE',
			max_connections = 1000,
			weight         = 1
	`, cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort)
	if err != nil {
		return fmt.Errorf("promote new writer: %w", err)
	}

	// Step 3: Ensure new writer is also in RO hostgroup for read traffic.
	_, err = db.ExecContext(qCtx, `
		INSERT INTO mysql_servers (hostgroup_id, hostname, port, status, max_connections, weight)
		VALUES (?, ?, ?, 'ONLINE', 1000, 1)
		ON DUPLICATE KEY UPDATE
			status         = 'ONLINE',
			max_connections = 1000
	`, cfg.ReadOnlyHostgroup, cfg.NewWriterHost, cfg.NewWriterPort)
	if err != nil {
		return fmt.Errorf("add new writer to RO group: %w", err)
	}

	// Step 4: Apply changes to runtime.
	if _, err := db.ExecContext(qCtx, "LOAD MYSQL SERVERS TO RUNTIME"); err != nil {
		return fmt.Errorf("LOAD MYSQL SERVERS TO RUNTIME: %w", err)
	}

	// Step 5: Persist changes to disk.
	if _, err := db.ExecContext(qCtx, "SAVE MYSQL SERVERS TO DISK"); err != nil {
		return fmt.Errorf("SAVE MYSQL SERVERS TO DISK: %w", err)
	}

	return nil
}
