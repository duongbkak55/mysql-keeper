// Package pxc manages a Percona XtraDB Cluster's read/write state via:
//  1. SQL  — SET GLOBAL read_only / super_read_only (immediate effect).
//  2. CRD  — patches spec.replication.channels[].isSource on the local
//             PerconaXtraDBCluster so the operator enforces the state after
//             any pod restart.
//
// Use pxc.Manager when MANO is not available.
// When MANO is configured, use mano.PXCManager instead (see internal/mano).
package pxc

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Manager manages read_only state on a PXC cluster via SQL + optional CRD patch.
type Manager struct {
	dsn     string
	timeout time.Duration

	// k8sClient patches the local PXC CRD directly.
	// Nil for remote-only (SQL-only) managers.
	k8sClient    client.Client
	pxcNamespace string
	pxcName      string
	channelName  string
}

// NewManager creates a Manager for the LOCAL cluster.
// It patches spec.replication.channels[channelName].isSource via the local k8s API
// so the state persists across operator-triggered pod restarts.
func NewManager(dsn string, timeout time.Duration, k8sClient client.Client, namespace, name, channelName string) *Manager {
	return &Manager{
		dsn:          dsn,
		timeout:      timeout,
		k8sClient:    k8sClient,
		pxcNamespace: namespace,
		pxcName:      name,
		channelName:  channelName,
	}
}

// NewRemoteManager creates a Manager for the REMOTE cluster (SQL only).
// No k8s API access — the remote controller self-corrects its CRD on its next reconcile.
func NewRemoteManager(dsn string, timeout time.Duration) *Manager {
	return &Manager{dsn: dsn, timeout: timeout}
}

// NewRemoteManagerWithKubeAPI creates a Manager for the REMOTE cluster
// with a k8s client pointing to the remote cluster's API server.
func NewRemoteManagerWithKubeAPI(dsn string, timeout time.Duration, k8sClient client.Client, namespace, name, channelName string) *Manager {
	return &Manager{
		dsn:          dsn,
		timeout:      timeout,
		k8sClient:    k8sClient,
		pxcNamespace: namespace,
		pxcName:      name,
		channelName:  channelName,
	}
}

// SetReadOnly fences the cluster: SET GLOBAL super_read_only=ON + read_only=ON.
// Then patches isSource=false on the local CRD (if k8s client is available).
func (m *Manager) SetReadOnly(ctx context.Context) error {
	logger := log.FromContext(ctx)
	logger.Info("Setting cluster to read_only=ON via SQL")

	if err := m.execReadOnly(ctx, true); err != nil {
		return err
	}
	logger.Info("MySQL read_only=ON applied")

	if m.k8sClient != nil {
		if err := m.patchIsSource(ctx, false); err != nil {
			logger.Error(err, "Failed to patch CRD isSource=false (non-fatal: SQL change is active)")
		} else {
			logger.Info("CRD isSource=false patched")
		}
	}
	return nil
}

// SetReadWrite promotes the cluster: verifies wsrep Primary, sets read_only=OFF,
// verifies write, stops any old inbound replication so the new source is not
// fighting its own former upstream, then patches isSource=true on the local CRD.
func (m *Manager) SetReadWrite(ctx context.Context) error {
	logger := log.FromContext(ctx)
	logger.Info("Promoting cluster to read_only=OFF via SQL")

	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Verify Galera is in Primary state before promoting.
	var varName, wsrepStatus string
	if err := db.QueryRowContext(qCtx, "SHOW STATUS LIKE 'wsrep_cluster_status'").Scan(&varName, &wsrepStatus); err != nil {
		return fmt.Errorf("query wsrep_cluster_status: %w", err)
	}
	if wsrepStatus != "Primary" {
		return fmt.Errorf("cannot promote: wsrep_cluster_status=%q (must be 'Primary')", wsrepStatus)
	}

	if _, err := db.ExecContext(qCtx, "SET GLOBAL super_read_only=OFF"); err != nil {
		return fmt.Errorf("SET GLOBAL super_read_only=OFF: %w", err)
	}
	if _, err := db.ExecContext(qCtx, "SET GLOBAL read_only=OFF"); err != nil {
		return fmt.Errorf("SET GLOBAL read_only=OFF: %w", err)
	}
	logger.Info("MySQL read_only=OFF applied")

	if err := m.VerifyWrite(ctx); err != nil {
		_ = m.SetReadOnly(ctx) // rollback
		return fmt.Errorf("write verification failed after promotion: %w", err)
	}

	// Stop any inbound replication for the configured channel. Without this,
	// the new source keeps replicating from the old one — and on the next flip
	// we would ask the old source for GTIDs the new one generated while
	// writable, which would show up as Error 1236.
	if m.channelName != "" {
		if err := m.StopReplica(ctx, m.channelName); err != nil {
			logger.Error(err, "STOP REPLICA after promote failed — investigate",
				"channel", m.channelName)
		}
		if err := m.ResetReplicaAll(ctx, m.channelName); err != nil {
			logger.Error(err, "RESET REPLICA ALL after promote failed — investigate",
				"channel", m.channelName)
		}
	}

	if m.k8sClient != nil {
		if err := m.patchIsSource(ctx, true); err != nil {
			logger.Error(err, "Failed to patch CRD isSource=true (non-fatal: SQL change is active)")
		} else {
			logger.Info("CRD isSource=true patched")
		}
	}
	return nil
}

// IsWritable returns true if the cluster currently has read_only=OFF.
func (m *Manager) IsWritable(ctx context.Context) (bool, error) {
	db, err := m.openDB(ctx)
	if err != nil {
		return false, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var readOnly int
	if err := db.QueryRowContext(qCtx, "SELECT @@read_only").Scan(&readOnly); err != nil {
		return false, fmt.Errorf("query @@read_only: %w", err)
	}
	return readOnly == 0, nil
}

// EnsureKeeperSchema creates the keeper_probe table used for write verification.
// Call once during controller initialization.
func (m *Manager) EnsureKeeperSchema(ctx context.Context) error {
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if _, err := db.ExecContext(qCtx, `CREATE DATABASE IF NOT EXISTS keeper`); err != nil {
		return fmt.Errorf("create keeper database: %w", err)
	}
	_, err = db.ExecContext(qCtx, `
		CREATE TABLE IF NOT EXISTS keeper.probe (
			id   INT NOT NULL DEFAULT 1,
			ts   BIGINT NOT NULL,
			node VARCHAR(64) NOT NULL,
			PRIMARY KEY (id)
		) ENGINE=InnoDB
	`)
	if err != nil {
		return fmt.Errorf("create keeper.probe table: %w", err)
	}
	return nil
}

// patchIsSource sets spec.replication.channels[channelName].isSource on the PXC CRD.
func (m *Manager) patchIsSource(ctx context.Context, isSource bool) error {
	pxcObj := &PerconaXtraDBCluster{}
	if err := m.k8sClient.Get(ctx, types.NamespacedName{
		Namespace: m.pxcNamespace,
		Name:      m.pxcName,
	}, pxcObj); err != nil {
		return fmt.Errorf("get PerconaXtraDBCluster: %w", err)
	}

	patch := client.MergeFrom(pxcObj.DeepCopy())

	if pxcObj.Spec.Replication == nil {
		return fmt.Errorf("spec.replication is nil in PerconaXtraDBCluster %s/%s", m.pxcNamespace, m.pxcName)
	}

	found := false
	for i, ch := range pxcObj.Spec.Replication.Channels {
		if ch.Name == m.channelName {
			pxcObj.Spec.Replication.Channels[i].IsSource = isSource
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("replication channel %q not found in PerconaXtraDBCluster %s/%s", m.channelName, m.pxcNamespace, m.pxcName)
	}

	return m.k8sClient.Patch(ctx, pxcObj, patch)
}

func (m *Manager) execReadOnly(ctx context.Context, readOnly bool) error {
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	val := "ON"
	if !readOnly {
		val = "OFF"
	}
	if _, err := db.ExecContext(qCtx, "SET GLOBAL super_read_only="+val); err != nil {
		return fmt.Errorf("SET GLOBAL super_read_only=%s: %w", val, err)
	}
	if _, err := db.ExecContext(qCtx, "SET GLOBAL read_only="+val); err != nil {
		return fmt.Errorf("SET GLOBAL read_only=%s: %w", val, err)
	}
	return nil
}

// VerifyWrite inserts/updates the sentinel row in keeper.probe as a proof that
// the cluster is accepting writes. Callers should call EnsureKeeperSchema once
// during initialization so the table exists; without it this is a hard error
// rather than a false negative about writability.
func (m *Manager) VerifyWrite(ctx context.Context) error {
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	_, err = db.ExecContext(qCtx, `
		INSERT INTO keeper.probe (id, ts, node) VALUES (1, UNIX_TIMESTAMP(), @@hostname)
		ON DUPLICATE KEY UPDATE ts=UNIX_TIMESTAMP(), node=@@hostname
	`)
	if err != nil {
		return fmt.Errorf("write probe failed: %w", err)
	}
	return nil
}

func (m *Manager) openDB(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("mysql", m.dsn)
	if err != nil {
		return nil, fmt.Errorf("open MySQL: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(m.timeout)

	pingCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping MySQL %s: %w", m.dsn, err)
	}
	return db, nil
}
