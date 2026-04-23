// Leader-lease table used to coordinate between the DC and DR controllers.
// Only one controller can hold the lease at any time; the holder must refresh
// it or the lease expires and the peer may take over.
//
// The table lives on the currently-writable cluster. Writes to it depend on
// read_only=OFF, which means a peer controller cannot grab the lease from a
// fenced cluster — exactly the property we want.
package pxc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLeaseHeldElsewhere is returned when the lease row is owned by a different
// controller and its TTL has not expired. Callers should back off and retry.
var ErrLeaseHeldElsewhere = errors.New("leader lease is held by another controller")

// LeaderLease snapshots the current state of the keeper.leader row.
type LeaderLease struct {
	Owner       string
	Epoch       int64
	AcquiredAt  time.Time
	HeartbeatAt time.Time
	RenewedBy   string
}

// Expired reports whether the lease has gone stale based on the caller's clock
// and the configured TTL.
func (l LeaderLease) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(l.HeartbeatAt) > ttl
}

// EnsureLeaderLeaseSchema creates keeper.leader if it does not exist.
// Safe to call on every reconcile; the CREATE IF NOT EXISTS is cheap.
// Only works on a writable cluster.
func (m *Manager) EnsureLeaderLeaseSchema(ctx context.Context) error {
	db, err := m.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if _, err := db.ExecContext(qCtx, `CREATE DATABASE IF NOT EXISTS keeper`); err != nil {
		return fmt.Errorf("create keeper db: %w", err)
	}
	// epoch is a monotonically-increasing fencing token. When a cluster resumes
	// after being demoted it reads this value from the peer; seeing a higher
	// epoch than its own signal is the authoritative "you are no longer the
	// source" message.
	_, err = db.ExecContext(qCtx, `
		CREATE TABLE IF NOT EXISTS keeper.leader (
			id            INT         NOT NULL DEFAULT 1,
			owner         VARCHAR(128) NOT NULL,
			epoch         BIGINT       NOT NULL,
			acquired_at   DATETIME(6)  NOT NULL,
			heartbeat_at  DATETIME(6)  NOT NULL,
			renewed_by    VARCHAR(128) NOT NULL,
			PRIMARY KEY (id),
			CONSTRAINT keeper_leader_singleton CHECK (id = 1)
		) ENGINE=InnoDB`)
	if err != nil {
		return fmt.Errorf("create keeper.leader: %w", err)
	}
	return nil
}

// GetLeaderLease reads the current lease row, returning (_, sql.ErrNoRows) if
// no lease has ever been taken.
func (m *Manager) GetLeaderLease(ctx context.Context) (LeaderLease, error) {
	db, err := m.openDB(ctx)
	if err != nil {
		return LeaderLease{}, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var l LeaderLease
	err = db.QueryRowContext(qCtx, `
		SELECT owner, epoch, acquired_at, heartbeat_at, renewed_by
		FROM keeper.leader WHERE id = 1`,
	).Scan(&l.Owner, &l.Epoch, &l.AcquiredAt, &l.HeartbeatAt, &l.RenewedBy)
	if err != nil {
		return l, err
	}
	return l, nil
}

// AcquireOrRenewLease attempts to take or refresh the lease for `ownerID`.
//
//   - If no lease exists, creates one at epoch 1.
//   - If the caller already owns the lease, refreshes heartbeat_at (no epoch bump).
//   - If another owner holds a fresh lease, returns ErrLeaseHeldElsewhere.
//   - If another owner holds a stale lease, takes over by bumping epoch.
//
// The function runs inside a transaction with SELECT ... FOR UPDATE to avoid
// two controllers racing against the same row.
func (m *Manager) AcquireOrRenewLease(
	ctx context.Context,
	ownerID string,
	ttl time.Duration,
) (LeaderLease, error) {
	if ownerID == "" {
		return LeaderLease{}, fmt.Errorf("ownerID must not be empty")
	}
	db, err := m.openDB(ctx)
	if err != nil {
		return LeaderLease{}, err
	}
	defer db.Close()

	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	tx, err := db.BeginTx(qCtx, nil)
	if err != nil {
		return LeaderLease{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on error path

	var current LeaderLease
	err = tx.QueryRowContext(qCtx, `
		SELECT owner, epoch, acquired_at, heartbeat_at, renewed_by
		FROM keeper.leader WHERE id = 1 FOR UPDATE`,
	).Scan(&current.Owner, &current.Epoch, &current.AcquiredAt, &current.HeartbeatAt, &current.RenewedBy)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// First-ever acquisition — insert.
		if _, err := tx.ExecContext(qCtx, `
			INSERT INTO keeper.leader (id, owner, epoch, acquired_at, heartbeat_at, renewed_by)
			VALUES (1, ?, 1, NOW(6), NOW(6), ?)`,
			ownerID, ownerID); err != nil {
			return LeaderLease{}, fmt.Errorf("insert initial lease: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return LeaderLease{}, fmt.Errorf("commit initial lease: %w", err)
		}
		return m.GetLeaderLease(ctx)

	case err != nil:
		return LeaderLease{}, fmt.Errorf("select keeper.leader: %w", err)
	}

	now := time.Now().UTC()
	stale := current.Expired(now, ttl)

	switch {
	case current.Owner == ownerID:
		// Renew heartbeat, keep epoch.
		if _, err := tx.ExecContext(qCtx, `
			UPDATE keeper.leader
			SET heartbeat_at = NOW(6), renewed_by = ?
			WHERE id = 1`, ownerID); err != nil {
			return LeaderLease{}, fmt.Errorf("renew lease: %w", err)
		}

	case stale:
		// Previous owner went away — take over with a new epoch.
		newEpoch := current.Epoch + 1
		if _, err := tx.ExecContext(qCtx, `
			UPDATE keeper.leader
			SET owner = ?, epoch = ?,
			    acquired_at = NOW(6), heartbeat_at = NOW(6),
			    renewed_by = ?
			WHERE id = 1`, ownerID, newEpoch, ownerID); err != nil {
			return LeaderLease{}, fmt.Errorf("take over lease: %w", err)
		}

	default:
		// Peer holds a fresh lease — refuse.
		return current, ErrLeaseHeldElsewhere
	}

	if err := tx.Commit(); err != nil {
		return LeaderLease{}, fmt.Errorf("commit lease write: %w", err)
	}
	return m.GetLeaderLease(ctx)
}
