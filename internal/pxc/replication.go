// Replication channel helpers: inspect state via performance_schema,
// stop/reset a channel after promotion, and probe reachability cheaply.
// All functions target a single replication channel identified by name.
package pxc

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ReplicationStatus describes the health of one async replication channel.
type ReplicationStatus struct {
	// ChannelName is the performance_schema channel identifier.
	ChannelName string

	// IOServiceState is "ON" when the receiver thread is running.
	IOServiceState string
	IOErrorNumber  int
	IOErrorMessage string

	// SQLServiceState is "ON" when the applier is running.
	SQLServiceState string
	SQLErrorNumber  int
	SQLErrorMessage string

	// ConfigExists is false when no row is found for this channel — either it
	// was never configured or RESET REPLICA ALL was run.
	ConfigExists bool
}

// Running reports whether both threads are operating normally with no last-error.
func (r ReplicationStatus) Running() bool {
	return r.ConfigExists &&
		r.IOServiceState == "ON" && r.SQLServiceState == "ON" &&
		r.IOErrorNumber == 0 && r.SQLErrorNumber == 0
}

// HumanMessage returns a short diagnostic string suitable for logs/status.
func (r ReplicationStatus) HumanMessage() string {
	if !r.ConfigExists {
		return fmt.Sprintf("channel %q has no replication row", r.ChannelName)
	}
	parts := []string{
		fmt.Sprintf("io=%s", r.IOServiceState),
		fmt.Sprintf("sql=%s", r.SQLServiceState),
	}
	if r.IOErrorNumber != 0 {
		parts = append(parts, fmt.Sprintf("ioErr=%d(%s)", r.IOErrorNumber, r.IOErrorMessage))
	}
	if r.SQLErrorNumber != 0 {
		parts = append(parts, fmt.Sprintf("sqlErr=%d(%s)", r.SQLErrorNumber, r.SQLErrorMessage))
	}
	return fmt.Sprintf("channel %q: %v", r.ChannelName, parts)
}

// GetReplicationStatus reads one row each from replication_connection_status
// and replication_applier_status_by_coordinator for the given channel.
// If the channel has no row (ConfigExists=false), the other fields are zeroed.
func (m *Manager) GetReplicationStatus(ctx context.Context, channel string) (ReplicationStatus, error) {
	if channel == "" {
		return ReplicationStatus{}, fmt.Errorf("channel name is empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return ReplicationStatus{}, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	status := ReplicationStatus{ChannelName: channel}

	// IO (receiver) thread.
	var ioMsg sql.NullString
	err = db.QueryRowContext(qCtx, `
		SELECT service_state, last_error_number, last_error_message
		FROM performance_schema.replication_connection_status
		WHERE channel_name = ?`, channel,
	).Scan(&status.IOServiceState, &status.IOErrorNumber, &ioMsg)
	switch {
	case err == sql.ErrNoRows:
		status.ConfigExists = false
		return status, nil
	case err != nil:
		return status, fmt.Errorf("read replication_connection_status: %w", err)
	}
	status.ConfigExists = true
	status.IOErrorMessage = ioMsg.String

	// SQL (applier) thread — prefer the coordinator view (covers MTS too).
	var sqlMsg sql.NullString
	err = db.QueryRowContext(qCtx, `
		SELECT service_state, last_error_number, last_error_message
		FROM performance_schema.replication_applier_status_by_coordinator
		WHERE channel_name = ?`, channel,
	).Scan(&status.SQLServiceState, &status.SQLErrorNumber, &sqlMsg)
	if err == sql.ErrNoRows {
		// Single-threaded mode exposes only replication_applier_status.
		err = db.QueryRowContext(qCtx, `
			SELECT service_state
			FROM performance_schema.replication_applier_status
			WHERE channel_name = ?`, channel,
		).Scan(&status.SQLServiceState)
	}
	if err != nil {
		return status, fmt.Errorf("read replication_applier_status*: %w", err)
	}
	status.SQLErrorMessage = sqlMsg.String
	return status, nil
}

// StopReplica halts the receiver and applier for the channel. Idempotent: a
// no-op when the channel is already stopped.
func (m *Manager) StopReplica(ctx context.Context, channel string) error {
	if channel == "" {
		return fmt.Errorf("channel name is empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// MySQL 8 supports both STOP REPLICA and STOP SLAVE. Prefer REPLICA.
	if _, err := db.ExecContext(qCtx,
		"STOP REPLICA FOR CHANNEL "+quoteChannel(channel)); err != nil {
		// Fallback for older servers.
		if _, err2 := db.ExecContext(qCtx,
			"STOP SLAVE FOR CHANNEL "+quoteChannel(channel)); err2 != nil {
			return fmt.Errorf("STOP REPLICA / STOP SLAVE: %v / %v", err, err2)
		}
	}
	return nil
}

// ResetReplicaAll clears the replication metadata (source host, relay logs,
// master_info) for the channel. Must be called when flipping a former replica
// into a pure source, otherwise stale position info can cause surprises on
// the next direction change.
func (m *Manager) ResetReplicaAll(ctx context.Context, channel string) error {
	if channel == "" {
		return fmt.Errorf("channel name is empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if _, err := db.ExecContext(qCtx,
		"RESET REPLICA ALL FOR CHANNEL "+quoteChannel(channel)); err != nil {
		if _, err2 := db.ExecContext(qCtx,
			"RESET SLAVE ALL FOR CHANNEL "+quoteChannel(channel)); err2 != nil {
			return fmt.Errorf("RESET REPLICA ALL / RESET SLAVE ALL: %v / %v", err, err2)
		}
	}
	return nil
}

// ProbeReachable does a ping + trivial SELECT within `budget` and returns
// whether the node answered. Used to distinguish "local really down" from
// "local slow / partial" so fencing knows whether it is safe to escalate to
// alternate paths without risking split-brain.
func (m *Manager) ProbeReachable(ctx context.Context, budget time.Duration) (bool, error) {
	if budget <= 0 {
		budget = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	db, err := sql.Open("mysql", m.dsn)
	if err != nil {
		return false, fmt.Errorf("open: %w", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(budget)

	if err := db.PingContext(probeCtx); err != nil {
		return false, nil //nolint:nilerr // treat any ping failure as "not reachable"
	}
	var one int
	if err := db.QueryRowContext(probeCtx, "SELECT 1").Scan(&one); err != nil {
		return false, nil //nolint:nilerr
	}
	return one == 1, nil
}

// quoteChannel embeds a channel name as a single-quoted SQL string literal.
// FOR CHANNEL takes a string literal (not an identifier), so we escape single
// quotes and backslashes rather than using backticks. Channel names in PXC
// CRDs are already alphanumeric-with-hyphen in practice.
func quoteChannel(name string) string {
	out := make([]byte, 0, len(name)+2)
	out = append(out, '\'')
	for i := 0; i < len(name); i++ {
		switch name[i] {
		case '\'':
			out = append(out, '\'', '\'')
		case '\\':
			out = append(out, '\\', '\\')
		default:
			out = append(out, name[i])
		}
	}
	out = append(out, '\'')
	return string(out)
}
