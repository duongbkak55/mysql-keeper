// GTID + replication introspection helpers used by the switchover preflight
// (see internal/switchover/preflight.go). These queries never mutate state;
// they exist so the controller can refuse to promote a replica that is not
// fully caught up (the root cause of Error 1236 after DC-DR role flip).
package pxc

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GTIDSnapshot captures the GTID-related state of a single MySQL node at a
// point in time. Used to compare progress between DC and DR before a flip.
type GTIDSnapshot struct {
	// Executed is the union GTID set this node has applied and committed.
	// Returned verbatim from @@GLOBAL.gtid_executed (may contain newlines).
	Executed string

	// Purged is what has been removed from binlogs — used to detect whether
	// the peer can still catch up or whether binlog retention is too short.
	Purged string

	// BinlogExpireLogsSeconds is the retention window enforced by this node.
	BinlogExpireLogsSeconds int64

	// LogReplicaUpdatesOn is true when log_replica_updates (or the legacy
	// log_slave_updates) is ON — required so events received as a replica
	// are written to this node's own binlog, and will thus survive a flip.
	LogReplicaUpdatesOn bool

	// BinlogFormat should always be "ROW" for PXC + GTID.
	BinlogFormat string

	// GTIDMode should always be "ON".
	GTIDMode string
}

// GetGTIDSnapshot collects everything needed for GTID completeness checks.
func (m *Manager) GetGTIDSnapshot(ctx context.Context) (GTIDSnapshot, error) {
	db, err := m.openDB(ctx)
	if err != nil {
		return GTIDSnapshot{}, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var snap GTIDSnapshot

	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.gtid_executed").Scan(&snap.Executed); err != nil {
		return snap, fmt.Errorf("read gtid_executed: %w", err)
	}
	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.gtid_purged").Scan(&snap.Purged); err != nil {
		return snap, fmt.Errorf("read gtid_purged: %w", err)
	}
	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.binlog_expire_logs_seconds").Scan(&snap.BinlogExpireLogsSeconds); err != nil {
		return snap, fmt.Errorf("read binlog_expire_logs_seconds: %w", err)
	}
	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.binlog_format").Scan(&snap.BinlogFormat); err != nil {
		return snap, fmt.Errorf("read binlog_format: %w", err)
	}
	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.gtid_mode").Scan(&snap.GTIDMode); err != nil {
		return snap, fmt.Errorf("read gtid_mode: %w", err)
	}

	snap.LogReplicaUpdatesOn, err = readLogReplicaUpdates(qCtx, db)
	if err != nil {
		return snap, err
	}
	return snap, nil
}

// GetExecutedGTID returns @@GLOBAL.gtid_executed only. Cheaper than GetGTIDSnapshot
// when you just need the frozen position for a subset/wait check.
func (m *Manager) GetExecutedGTID(ctx context.Context) (string, error) {
	db, err := m.openDB(ctx)
	if err != nil {
		return "", err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var g string
	if err := db.QueryRowContext(qCtx, "SELECT @@GLOBAL.gtid_executed").Scan(&g); err != nil {
		return "", fmt.Errorf("read gtid_executed: %w", err)
	}
	return g, nil
}

// IsGTIDSubset returns true when `other` ⊆ gtid_executed on this node.
// Use it to answer "has this replica applied everything the source has?".
// Pass the source's executed set as `other`.
func (m *Manager) IsGTIDSubset(ctx context.Context, other string) (bool, error) {
	if other == "" {
		return true, nil
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return false, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var result sql.NullInt64
	err = db.QueryRowContext(qCtx,
		"SELECT GTID_SUBSET(?, @@GLOBAL.gtid_executed)", other).Scan(&result)
	if err != nil {
		return false, fmt.Errorf("GTID_SUBSET: %w", err)
	}
	return result.Valid && result.Int64 == 1, nil
}

// MissingGTIDs returns GTIDs present in `other` but missing from this node's
// gtid_executed. An empty string means "fully caught up".
func (m *Manager) MissingGTIDs(ctx context.Context, other string) (string, error) {
	if other == "" {
		return "", nil
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return "", err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var missing sql.NullString
	err = db.QueryRowContext(qCtx,
		"SELECT GTID_SUBTRACT(?, @@GLOBAL.gtid_executed)", other).Scan(&missing)
	if err != nil {
		return "", fmt.Errorf("GTID_SUBTRACT: %w", err)
	}
	if !missing.Valid {
		return "", nil
	}
	return missing.String, nil
}

// WaitForGTID blocks until @@GLOBAL.gtid_executed ⊇ gtid or the timeout fires.
// Returns nil on success; a non-nil error if the wait timed out or failed.
// The bounded timeout is the replica-side budget, NOT the query timeout.
func (m *Manager) WaitForGTID(ctx context.Context, gtid string, timeout time.Duration) error {
	if gtid == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	// Allow the query a little longer than the wait itself so we get the
	// result code instead of a context cancellation.
	qCtx, cancel := context.WithTimeout(ctx, timeout+m.timeout)
	defer cancel()

	var result sql.NullInt64
	seconds := int(timeout.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	err = db.QueryRowContext(qCtx,
		"SELECT WAIT_FOR_EXECUTED_GTID_SET(?, ?)", gtid, seconds).Scan(&result)
	if err != nil {
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET: %w", err)
	}
	if !result.Valid {
		return fmt.Errorf("WAIT_FOR_EXECUTED_GTID_SET returned NULL (GTID mode off?)")
	}
	// 0 = reached target, 1 = timeout.
	if result.Int64 != 0 {
		return fmt.Errorf("replica did not reach target GTID within %s", timeout)
	}
	return nil
}

// CountGTIDTransactions parses a GTID set string and returns the total number
// of individual transactions it represents. Used to derive a scalar "missing
// transactions" count from the GTID_SUBTRACT result without needing a DB call.
//
// GTID set format: "uuid:N[-M][:P-Q]...[,uuid2:...]" with optional whitespace.
// A range N-M contributes M-N+1 transactions; a single N contributes 1.
func CountGTIDTransactions(gtidSet string) int64 {
	if gtidSet == "" {
		return 0
	}
	var total int64
	for _, entry := range strings.FieldsFunc(gtidSet, func(r rune) bool { return r == ',' }) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		// Split by ':' — first token is the UUID, remaining tokens are intervals.
		parts := strings.Split(entry, ":")
		for _, iv := range parts[1:] {
			iv = strings.TrimSpace(iv)
			if iv == "" {
				continue
			}
			if dash := strings.IndexByte(iv, '-'); dash >= 0 {
				lo, e1 := strconv.ParseInt(iv[:dash], 10, 64)
				hi, e2 := strconv.ParseInt(iv[dash+1:], 10, 64)
				if e1 == nil && e2 == nil && hi >= lo {
					total += hi - lo + 1
				}
			} else {
				if _, err := strconv.ParseInt(iv, 10, 64); err == nil {
					total++ // single transaction number = 1 transaction
				}
			}
		}
	}
	return total
}

// readLogReplicaUpdates prefers the modern variable name and falls back to
// the 5.7-era one used on many Percona 5.7 clusters still in production.
func readLogReplicaUpdates(ctx context.Context, db *sql.DB) (bool, error) {
	var val int
	err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.log_replica_updates").Scan(&val)
	if err == nil {
		return val == 1, nil
	}
	// Fallback for MySQL 8.0.25- / Percona 5.7.
	err2 := db.QueryRowContext(ctx, "SELECT @@GLOBAL.log_slave_updates").Scan(&val)
	if err2 != nil {
		return false, fmt.Errorf("neither log_replica_updates nor log_slave_updates is readable (%v / %v)", err, err2)
	}
	return val == 1, nil
}
