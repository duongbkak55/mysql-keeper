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

// GetReplicationLagSeconds returns how many seconds the replica is behind the
// source, derived from the ORIGINAL_COMMIT_TIMESTAMP of the last applied (or
// last queued) transaction on this node's replication channel.
// Returns -1 when no data is available: channel not configured, no transactions
// replicated yet, or a NULL timestamp. Clamps negative values to 0 (clock skew).
func (m *Manager) GetReplicationLagSeconds(ctx context.Context, channel string) (int64, error) {
	if channel == "" {
		return -1, fmt.Errorf("channel name is empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return -1, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Prefer coordinator's LAST_APPLIED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP
	// (available on both MTS and single-threaded replicas via the coordinator
	// table). Fall back to the queued timestamp when the coordinator row is absent.
	var lag sql.NullInt64
	err = db.QueryRowContext(qCtx, `
		SELECT TIMESTAMPDIFF(SECOND,
			COALESCE(
				NULLIF(ac.LAST_APPLIED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP, '0000-00-00 00:00:00.000000'),
				NULLIF(cs.LAST_QUEUED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP,  '0000-00-00 00:00:00.000000')
			),
			NOW())
		FROM performance_schema.replication_connection_status cs
		LEFT JOIN performance_schema.replication_applier_status_by_coordinator ac
		  ON ac.CHANNEL_NAME = cs.CHANNEL_NAME
		WHERE cs.CHANNEL_NAME = ?`, channel,
	).Scan(&lag)

	if err != nil && err != sql.ErrNoRows {
		// Older schema or PS disabled — fall back to connection_status only.
		err = db.QueryRowContext(qCtx, `
			SELECT TIMESTAMPDIFF(SECOND,
				NULLIF(LAST_QUEUED_TRANSACTION_ORIGINAL_COMMIT_TIMESTAMP, '0000-00-00 00:00:00.000000'),
				NOW())
			FROM performance_schema.replication_connection_status
			WHERE CHANNEL_NAME = ?`, channel,
		).Scan(&lag)
	}

	switch {
	case err == sql.ErrNoRows, !lag.Valid:
		return -1, nil
	case err != nil:
		return -1, fmt.Errorf("get replication lag seconds: %w", err)
	}
	if lag.Int64 < 0 {
		return 0, nil
	}
	return lag.Int64, nil
}

// WorkerError describes one row from
// performance_schema.replication_applier_status_by_worker (or the single-thread
// fallback) that currently reports LAST_ERROR_NUMBER > 0 for a channel.
//
// FailedGTID, when non-empty, is the controller's best estimate of the GTID
// the applier is stuck on — the next transaction after LAST_APPLIED_TRANSACTION.
// MySQL 8 exposes this directly through the worker view; older versions may
// not, in which case FailedGTID is left empty and the controller treats the
// alarm as informational only.
type WorkerError struct {
	Channel    string
	WorkerID   int32
	Errno      int32
	Message    string
	FailedGTID string
	Timestamp  time.Time
}

// DetectWorkerErrors lists all worker rows for the given channel that report
// LAST_ERROR_NUMBER > 0, plus a coordinator-level fallback for single-thread
// configurations where replication_applier_status_by_worker is empty.
//
// Returns an empty slice (not error) when no row reports an error, when the
// channel is unconfigured, or when the performance_schema tables are not
// available. The caller treats nil/empty as "no SQL apply error".
func (m *Manager) DetectWorkerErrors(ctx context.Context, channel string) ([]WorkerError, error) {
	if channel == "" {
		return nil, fmt.Errorf("channel name is empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Worker-level rows. MTS exposes one row per worker; LAST_PROCESSED_TRANSACTION
	// is the GTID currently being applied (i.e. the failing one) when the worker
	// is in error. LAST_APPLIED_TRANSACTION is the GTID of the last txn that
	// succeeded — different field, do not confuse.
	rows, err := db.QueryContext(qCtx, `
		SELECT
			WORKER_ID,
			LAST_ERROR_NUMBER,
			COALESCE(LAST_ERROR_MESSAGE, ''),
			COALESCE(LAST_PROCESSED_TRANSACTION, ''),
			LAST_ERROR_TIMESTAMP
		FROM performance_schema.replication_applier_status_by_worker
		WHERE CHANNEL_NAME = ? AND LAST_ERROR_NUMBER > 0`, channel)
	if err != nil {
		return nil, fmt.Errorf("read replication_applier_status_by_worker: %w", err)
	}
	defer rows.Close()

	var out []WorkerError
	for rows.Next() {
		var w WorkerError
		var ts sql.NullTime
		if err := rows.Scan(&w.WorkerID, &w.Errno, &w.Message, &w.FailedGTID, &ts); err != nil {
			return nil, fmt.Errorf("scan worker row: %w", err)
		}
		w.Channel = channel
		if ts.Valid {
			w.Timestamp = ts.Time
		} else {
			w.Timestamp = time.Now()
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker rows: %w", err)
	}
	if len(out) > 0 {
		return out, nil
	}

	// Single-thread fallback: replication_applier_status surfaces only state,
	// not an errno. Use the coordinator view (which we already query in
	// GetReplicationStatus) to recover the SQL applier error.
	var (
		coordErrno int32
		coordMsg   sql.NullString
	)
	err = db.QueryRowContext(qCtx, `
		SELECT LAST_ERROR_NUMBER, LAST_ERROR_MESSAGE
		FROM performance_schema.replication_applier_status_by_coordinator
		WHERE CHANNEL_NAME = ? AND LAST_ERROR_NUMBER > 0`, channel,
	).Scan(&coordErrno, &coordMsg)
	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		// Older servers (5.6) lack the coordinator view entirely. Treat as
		// "no detectable error" rather than failing the whole reconcile —
		// IO/SQL state metrics still tell the alerting story.
		return nil, nil //nolint:nilerr
	}
	return []WorkerError{{
		Channel:   channel,
		Errno:     coordErrno,
		Message:   coordMsg.String,
		Timestamp: time.Now(),
	}}, nil
}

// SkipNextTransaction injects an empty transaction with the given GTID on the
// channel so the SQL applier can move past a transaction that was failing to
// apply. Implements the standard MySQL 5.7+ skip recipe:
//
//	STOP REPLICA SQL_THREAD FOR CHANNEL '<name>';
//	SET gtid_next = '<failed-gtid>';
//	BEGIN; COMMIT;
//	SET gtid_next = AUTOMATIC;
//	START REPLICA SQL_THREAD FOR CHANNEL '<name>';
//
// All statements run on a single MySQL connection so the gtid_next session
// variable persists between SET and BEGIN/COMMIT. On any failure between SET
// and the AUTOMATIC reset, the function makes a best-effort attempt to
// restore gtid_next=AUTOMATIC and start the SQL thread before returning the
// underlying error. The caller is expected to surface that error to operators
// rather than auto-retrying.
//
// gtid is validated against a strict character set ([0-9a-fA-F:-]) before any
// SQL is run, preventing injection through MySQL's session-variable syntax,
// which does not accept parameter placeholders.
func (m *Manager) SkipNextTransaction(ctx context.Context, channel, gtid string) error {
	if channel == "" {
		return fmt.Errorf("channel name is empty")
	}
	if gtid == "" {
		return fmt.Errorf("gtid is empty")
	}
	if !validGTIDLiteral(gtid) {
		return fmt.Errorf("gtid %q rejected: only [0-9a-fA-F:-] permitted", gtid)
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for skip: %w", err)
	}
	defer conn.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	stopStmt := "STOP REPLICA SQL_THREAD FOR CHANNEL " + quoteChannel(channel)
	stopFallback := "STOP SLAVE SQL_THREAD FOR CHANNEL " + quoteChannel(channel)
	if _, err := conn.ExecContext(qCtx, stopStmt); err != nil {
		if _, err2 := conn.ExecContext(qCtx, stopFallback); err2 != nil {
			return fmt.Errorf("STOP REPLICA/SLAVE SQL_THREAD: %v / %v", err, err2)
		}
	}

	// SET gtid_next does NOT accept parameter binding — embed the validated
	// literal directly between single quotes (the regex above forbids quote
	// and backslash characters, so escaping is unnecessary).
	setNext := "SET gtid_next = '" + gtid + "'"
	resetNext := "SET gtid_next = AUTOMATIC"
	startStmt := "START REPLICA SQL_THREAD FOR CHANNEL " + quoteChannel(channel)
	startFallback := "START SLAVE SQL_THREAD FOR CHANNEL " + quoteChannel(channel)

	// restoreApplier is the cleanup path on any failure between SET and the
	// final AUTOMATIC reset. It uses a fresh timeout so a near-expired qCtx
	// from the original failure does not poison the recovery, which would
	// leave the connection's gtid_next session variable set to the failed
	// GTID when the conn returns to the pool.
	restoreApplier := func(orig error) error {
		recoverCtx, recoverCancel := context.WithTimeout(ctx, m.timeout)
		defer recoverCancel()
		if _, e := conn.ExecContext(recoverCtx, resetNext); e != nil {
			orig = fmt.Errorf("%w; cleanup reset gtid_next failed: %v", orig, e)
		}
		if _, e := conn.ExecContext(recoverCtx, startStmt); e != nil {
			if _, e2 := conn.ExecContext(recoverCtx, startFallback); e2 != nil {
				orig = fmt.Errorf("%w; cleanup START SQL_THREAD failed: %v / %v", orig, e, e2)
			}
		}
		return orig
	}

	if _, err := conn.ExecContext(qCtx, setNext); err != nil {
		return restoreApplier(fmt.Errorf("SET gtid_next: %w", err))
	}
	if _, err := conn.ExecContext(qCtx, "BEGIN"); err != nil {
		return restoreApplier(fmt.Errorf("BEGIN: %w", err))
	}
	if _, err := conn.ExecContext(qCtx, "COMMIT"); err != nil {
		return restoreApplier(fmt.Errorf("COMMIT: %w", err))
	}
	if _, err := conn.ExecContext(qCtx, resetNext); err != nil {
		return restoreApplier(fmt.Errorf("SET gtid_next AUTOMATIC: %w", err))
	}
	if _, err := conn.ExecContext(qCtx, startStmt); err != nil {
		if _, err2 := conn.ExecContext(qCtx, startFallback); err2 != nil {
			return fmt.Errorf("START REPLICA/SLAVE SQL_THREAD: %v / %v", err, err2)
		}
	}
	return nil
}

// maxGTIDLiteralLength caps the validated GTID payload at a value that
// comfortably accommodates real-world multi-source GTIDs (a server-uuid plus
// up to ~10 numeric ranges) while preventing arbitrarily large strings from
// being smuggled through validGTIDLiteral.
const maxGTIDLiteralLength = 200

// validGTIDLiteral returns true when s contains only characters MySQL accepts
// as part of a GTID literal: hex digits, ':' and '-'. It does not enforce a
// full grammar; that is the server's job. The point is to defang any string
// that could break out of the embedded single-quoted literal in
// SkipNextTransaction. A length cap defends against operators or upstream
// systems sending pathological inputs.
func validGTIDLiteral(s string) bool {
	if len(s) == 0 || len(s) > maxGTIDLiteralLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		case c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
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
