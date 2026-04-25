// Package mano — PXCManager is the MANO-backed implementation of the
// switchover.PXCManager / ReplicationInspector / ReplicationController
// interfaces.
//
// State changes (SetReadOnly / SetReadWrite) go exclusively through the
// MANO LCM API:
//
//	POST /cnflcm/v1/custom-resources/{cnf}/{vdu}/update
//	→ poll GET /cnflcm/v1/lcm-op-occ/{id} until COMPLETED
//
// MANO then triggers the PXC operator which enforces the correct read_only
// state on the MySQL cluster. No direct SQL state-change commands are issued
// for isSource.
//
// All read-only queries (IsWritable, GTID snapshots, replication status) and
// the side-effects that must run against the server rather than the operator
// (STOP REPLICA, RESET REPLICA ALL) are delegated to an embedded pxc.Manager
// using the direct MySQL connection — MANO does not expose equivalents.
package mano

import (
	"context"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// sqlOps is the minimal direct-MySQL surface PXCManager needs for reads,
// replication manipulation, and write probes. *pxc.Manager satisfies this
// interface at runtime; tests inject a stub.
type sqlOps interface {
	IsWritable(ctx context.Context) (bool, error)
	GetGTIDSnapshot(ctx context.Context) (pxc.GTIDSnapshot, error)
	GetExecutedGTID(ctx context.Context) (string, error)
	IsGTIDSubset(ctx context.Context, other string) (bool, error)
	MissingGTIDs(ctx context.Context, other string) (string, error)
	WaitForGTID(ctx context.Context, gtid string, timeout time.Duration) error
	GetReplicationStatus(ctx context.Context, channel string) (pxc.ReplicationStatus, error)
	ProbeReachable(ctx context.Context, budget time.Duration) (bool, error)
	StopReplica(ctx context.Context, channel string) error
	ResetReplicaAll(ctx context.Context, channel string) error
	EnsureKeeperSchema(ctx context.Context) error
	VerifyWrite(ctx context.Context) error
}

// PXCManager controls a PXC cluster via the MANO LCM API for state toggles,
// and uses an embedded pxc.Manager for all direct-to-MySQL introspection.
type PXCManager struct {
	mano         *Client
	cnfName      string
	vduName      string
	pollInterval time.Duration
	pollTimeout  time.Duration

	// sql is the direct-MySQL helper used for reads and replication channel
	// manipulation. Created from (mysqlDSN, mysqlTimeout); never talks to MANO.
	sql sqlOps
}

// NewPXCManager creates a MANO-backed PXCManager.
//   - mano: shared MANO API client (same instance for DC and DR)
//   - cnfName / vduName: identify this cluster's CNF in MANO
//   - mysqlDSN: direct MySQL endpoint used for IsWritable / GTID / replication queries
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
		sql:          pxc.NewRemoteManager(mysqlDSN, mysqlTimeout),
	}
}

// NewPXCManagerForTest creates a PXCManager with an injected sqlOps implementation.
// Use only in tests.
func NewPXCManagerForTest(
	mano *Client,
	cnfName, vduName string,
	pollInterval, pollTimeout time.Duration,
	sql sqlOps,
) *PXCManager {
	return &PXCManager{
		mano:         mano,
		cnfName:      cnfName,
		vduName:      vduName,
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
		sql:          sql,
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

// IsWritable delegates to the direct SQL connection; MANO does not expose
// real-time variable state.
func (m *PXCManager) IsWritable(ctx context.Context) (bool, error) {
	return m.sql.IsWritable(ctx)
}

// --- ReplicationInspector ---

func (m *PXCManager) GetGTIDSnapshot(ctx context.Context) (pxc.GTIDSnapshot, error) {
	return m.sql.GetGTIDSnapshot(ctx)
}

func (m *PXCManager) GetExecutedGTID(ctx context.Context) (string, error) {
	return m.sql.GetExecutedGTID(ctx)
}

func (m *PXCManager) IsGTIDSubset(ctx context.Context, other string) (bool, error) {
	return m.sql.IsGTIDSubset(ctx, other)
}

func (m *PXCManager) MissingGTIDs(ctx context.Context, other string) (string, error) {
	return m.sql.MissingGTIDs(ctx, other)
}

func (m *PXCManager) WaitForGTID(ctx context.Context, gtid string, timeout time.Duration) error {
	return m.sql.WaitForGTID(ctx, gtid, timeout)
}

func (m *PXCManager) GetReplicationStatus(ctx context.Context, channel string) (pxc.ReplicationStatus, error) {
	return m.sql.GetReplicationStatus(ctx, channel)
}

func (m *PXCManager) ProbeReachable(ctx context.Context, budget time.Duration) (bool, error) {
	return m.sql.ProbeReachable(ctx, budget)
}

// --- ReplicationController ---

func (m *PXCManager) StopReplica(ctx context.Context, channel string) error {
	return m.sql.StopReplica(ctx, channel)
}

func (m *PXCManager) ResetReplicaAll(ctx context.Context, channel string) error {
	return m.sql.ResetReplicaAll(ctx, channel)
}

// verifyWrite is still a member of this package so we can surface MANO-
// specific error wrapping. It re-uses the embedded direct-MySQL helper.
func (m *PXCManager) verifyWrite(ctx context.Context) error {
	if err := m.sql.EnsureKeeperSchema(ctx); err != nil {
		return fmt.Errorf("ensure keeper schema before write verification: %w", err)
	}
	if err := m.sql.VerifyWrite(ctx); err != nil {
		return fmt.Errorf("write probe after MANO promotion: %w", err)
	}
	return nil
}
