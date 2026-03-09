// Package mano — PXCManager is the MANO-backed implementation of the
// switchover.PXCManager interface.
//
// State changes (SetReadOnly / SetReadWrite) go exclusively through the
// MANO LCM API:
//   POST /cnflcm/v1/custom-resources/{cnf}/{vdu}/update
//   → poll GET /cnflcm/v1/lcm-op-occ/{id} until COMPLETED
//
// MANO then triggers the PXC operator which enforces the correct read_only
// state on the MySQL cluster. No direct SQL state-change commands are issued.
//
// Read state queries (IsWritable) still use a direct MySQL connection because
// the MANO API does not expose a real-time read_only status endpoint.
package mano

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// PXCManager controls a PXC cluster via the MANO LCM API and satisfies the
// switchover.PXCManager interface without importing that package (duck typing).
type PXCManager struct {
	mano         *Client
	cnfName      string
	vduName      string
	pollInterval time.Duration
	pollTimeout  time.Duration

	// mysqlDSN is used only for IsWritable and write verification queries.
	mysqlDSN     string
	mysqlTimeout time.Duration
}

// NewPXCManager creates a MANO-backed PXCManager.
//   - mano: shared MANO API client (same instance for DC and DR)
//   - cnfName / vduName: identify this cluster's CNF in MANO
//   - mysqlDSN: direct MySQL endpoint for read-state health queries
func NewPXCManager(
	mano *Client,
	cnfName, vduName string,
	pollInterval, pollTimeout time.Duration,
	mysqlDSN string,
	mysqlTimeout time.Duration,
) *PXCManager {
	return &PXCManager{
		mano:         mano,
		cnfName:      cnfName,
		vduName:      vduName,
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
		mysqlDSN:     mysqlDSN,
		mysqlTimeout: mysqlTimeout,
	}
}

// SetReadOnly calls MANO to set replicationChannels[0].isSource=false.
// MANO reconciles the PXC operator, which enforces read_only=ON on the cluster.
func (m *PXCManager) SetReadOnly(ctx context.Context) error {
	return m.mano.SetIsSource(ctx, m.cnfName, m.vduName, false, m.pollInterval, m.pollTimeout)
}

// SetReadWrite calls MANO to set replicationChannels[0].isSource=true.
// MANO reconciles the PXC operator, which enforces read_only=OFF on the cluster.
// After MANO confirms COMPLETED, a direct write probe verifies the cluster
// is accepting writes.
func (m *PXCManager) SetReadWrite(ctx context.Context) error {
	if err := m.mano.SetIsSource(ctx, m.cnfName, m.vduName, true, m.pollInterval, m.pollTimeout); err != nil {
		return err
	}
	return m.verifyWrite(ctx)
}

// IsWritable queries the MySQL cluster directly to check read_only=OFF.
// This uses a direct connection because the MANO API does not expose
// real-time MySQL variable state.
func (m *PXCManager) IsWritable(ctx context.Context) (bool, error) {
	db, err := m.openDB()
	if err != nil {
		return false, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.mysqlTimeout)
	defer cancel()

	var readOnly int
	if err := db.QueryRowContext(qCtx, "SELECT @@read_only").Scan(&readOnly); err != nil {
		return false, fmt.Errorf("query @@read_only: %w", err)
	}
	return readOnly == 0, nil
}

// verifyWrite inserts/updates a sentinel row to confirm the cluster accepts writes.
func (m *PXCManager) verifyWrite(ctx context.Context) error {
	db, err := m.openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.mysqlTimeout)
	defer cancel()

	_, err = db.ExecContext(qCtx, `
		INSERT INTO keeper.probe (id, ts, node) VALUES (1, UNIX_TIMESTAMP(), @@hostname)
		ON DUPLICATE KEY UPDATE ts=UNIX_TIMESTAMP(), node=@@hostname
	`)
	if err != nil {
		return fmt.Errorf("write probe failed after MANO promotion: %w", err)
	}
	return nil
}

func (m *PXCManager) openDB() (*sql.DB, error) {
	db, err := sql.Open("mysql", m.mysqlDSN)
	if err != nil {
		return nil, fmt.Errorf("open MySQL %s: %w", m.mysqlDSN, err)
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(m.mysqlTimeout)
	return db, nil
}
