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
			id           INT          NOT NULL DEFAULT 1,
			owner        VARCHAR(128) NOT NULL,
			epoch        BIGINT       NOT NULL,
			acquired_at  DATETIME(6)  NOT NULL,
			heartbeat_at DATETIME(6)  NOT NULL,
			PRIMARY KEY (id),
			CONSTRAINT keeper_leader_singleton CHECK (id = 1)
		) ENGINE=InnoDB`)
	if err != nil {
		return fmt.Errorf("create keeper.leader: %w", err)
	}
	// Pre-seed the singleton row with an empty owner. This turns the race
	// condition "two controllers both discover no row and both try to INSERT"
	// (which deadlocks under SELECT ... FOR UPDATE) into the ordinary
	// "takeover of a stale row" path, which is already serialised by the
	// row lock.
	if _, err := db.ExecContext(qCtx, `
		INSERT IGNORE INTO keeper.leader
			(id, owner, epoch, acquired_at, heartbeat_at)
		VALUES (1, '', 0, NOW(6), '1970-01-01 00:00:01')
	`); err != nil {
		return fmt.Errorf("seed keeper.leader: %w", err)
	}
	return nil
}

// GetLeaderLease reads the current lease row, returning (_, sql.ErrNoRows) if
// no lease has ever been taken.
func (m *Manager) GetLeaderLease(ctx context.Context) (LeaderLease, error) {
	db := m.leasePool()
	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	var l LeaderLease
	err := db.QueryRowContext(qCtx, `
		SELECT owner, epoch, acquired_at, heartbeat_at
		FROM keeper.leader WHERE id = 1`,
	).Scan(&l.Owner, &l.Epoch, &l.AcquiredAt, &l.HeartbeatAt)
	return l, err
}

// AcquireOrRenewLease attempts to take or refresh the lease for `ownerID`.
//
//   - If no lease exists, creates one at epoch 1.
//   - If the caller already owns the lease, refreshes heartbeat_at (no epoch bump).
//   - If another owner holds a fresh lease, returns ErrLeaseHeldElsewhere.
//   - If another owner holds a stale lease, takes over by bumping epoch.
func (m *Manager) AcquireOrRenewLease(
	ctx context.Context,
	ownerID string,
	ttl time.Duration,
) (LeaderLease, error) {
	if ownerID == "" {
		return LeaderLease{}, fmt.Errorf("ownerID must not be empty")
	}
	return m.runAcquireOrRenew(ctx, m.leasePool(), ownerID, ttl)
}

// runAcquireOrRenew is the DB-operations half of AcquireOrRenewLease, split
// out so tests can substitute a sqlmock-backed *sql.DB. Runs inside a
// transaction with SELECT ... FOR UPDATE so two controllers cannot race.
func (m *Manager) runAcquireOrRenew(
	ctx context.Context,
	db *sql.DB,
	ownerID string,
	ttl time.Duration,
) (LeaderLease, error) {
	qCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	tx, err := db.BeginTx(qCtx, nil)
	if err != nil {
		return LeaderLease{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on error path

	var current LeaderLease
	err = tx.QueryRowContext(qCtx, `
		SELECT owner, epoch, acquired_at, heartbeat_at
		FROM keeper.leader WHERE id = 1 FOR UPDATE`,
	).Scan(&current.Owner, &current.Epoch, &current.AcquiredAt, &current.HeartbeatAt)

	if errors.Is(err, sql.ErrNoRows) {
		// Should not happen — EnsureLeaderLeaseSchema pre-seeds the row. But
		// be defensive for clusters that were bootstrapped before this code
		// existed: fall back to INSERT IGNORE, then re-read.
		if _, err := tx.ExecContext(qCtx, `
			INSERT IGNORE INTO keeper.leader
				(id, owner, epoch, acquired_at, heartbeat_at)
			VALUES (1, '', 0, NOW(6), '1970-01-01 00:00:01')`); err != nil {
			return LeaderLease{}, fmt.Errorf("lazy seed keeper.leader: %w", err)
		}
		err = tx.QueryRowContext(qCtx, `
			SELECT owner, epoch, acquired_at, heartbeat_at
			FROM keeper.leader WHERE id = 1 FOR UPDATE`,
		).Scan(&current.Owner, &current.Epoch, &current.AcquiredAt, &current.HeartbeatAt)
	}
	if err != nil {
		return LeaderLease{}, fmt.Errorf("select keeper.leader: %w", err)
	}

	now := time.Now().UTC()
	// The seed row carries owner='' and heartbeat_at = epoch; treat that as
	// "nobody is the leader". Same code path as stale-takeover, so we bump
	// epoch from 0 to 1 on first real acquisition.
	available := current.Owner == "" || current.Expired(now, ttl)

	// result holds what we are about to write; returned after commit so we
	// avoid a second SELECT round-trip to read back what we just wrote.
	var result LeaderLease

	switch {
	case current.Owner == ownerID:
		if _, err := tx.ExecContext(qCtx, `
			UPDATE keeper.leader
			SET heartbeat_at = NOW(6)
			WHERE id = 1`); err != nil {
			return LeaderLease{}, fmt.Errorf("renew lease: %w", err)
		}
		result = LeaderLease{
			Owner:       ownerID,
			Epoch:       current.Epoch,
			AcquiredAt:  current.AcquiredAt,
			HeartbeatAt: now,
		}

	case available:
		newEpoch := current.Epoch + 1
		if _, err := tx.ExecContext(qCtx, `
			UPDATE keeper.leader
			SET owner = ?, epoch = ?,
			    acquired_at = NOW(6), heartbeat_at = NOW(6)
			WHERE id = 1`, ownerID, newEpoch); err != nil {
			return LeaderLease{}, fmt.Errorf("take over lease: %w", err)
		}
		result = LeaderLease{
			Owner:       ownerID,
			Epoch:       newEpoch,
			AcquiredAt:  now,
			HeartbeatAt: now,
		}

	default:
		return current, ErrLeaseHeldElsewhere
	}

	if err := tx.Commit(); err != nil {
		return LeaderLease{}, fmt.Errorf("commit lease write: %w", err)
	}
	return result, nil
}
