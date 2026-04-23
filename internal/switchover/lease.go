// Leader-lease integration used by the switchover engine to prevent two
// controllers (one on DC, one on DR) from believing they can promote at the
// same instant. The lease lives as a single row in keeper.leader on whichever
// cluster is currently writable (see internal/pxc/leader_lease.go).
//
// Design (SB-1 + SB-3 combined, from the production-readiness review):
//
//   - The active source writes its ownerID + monotonically-increasing epoch
//     to keeper.leader and refreshes it on every reconcile.
//   - Before a controller promotes the opposite cluster, it calls CheckPeerLease
//     which reads the lease row from the *current* source. If the row's
//     heartbeat is fresh, the peer controller is still in control and this
//     controller aborts. Only a stale lease (owner went away) is safe to
//     take over.
//   - After promotion the new source bumps the epoch. A resurrected former
//     source inspects the new row on its first reconcile, sees an epoch
//     higher than its own, and self-fences.
package switchover

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// LeaseTracker is the mutation surface for keeper.leader. Implemented by
// *pxc.Manager (SQL) and *mano.PXCManager (delegates to embedded SQL helper).
// Exposed here as an interface so tests can substitute a fake.
type LeaseTracker interface {
	EnsureLeaderLeaseSchema(ctx context.Context) error
	GetLeaderLease(ctx context.Context) (pxc.LeaderLease, error)
	AcquireOrRenewLease(ctx context.Context, ownerID string, ttl time.Duration) (pxc.LeaderLease, error)
}

// ErrPeerLeaseFresh is returned when the opposite cluster still has a fresh
// lease row, i.e. its controller is alive and presumably mid-operation.
var ErrPeerLeaseFresh = errors.New("peer controller holds a fresh lease")

// CheckPeerLease returns nil when this controller may safely promote.
//
//   - peer is the tracker against the CURRENT source (the cluster we are
//     about to take over from).
//   - peerOwnerPrefix should be a short identifier that rules out "it was us
//     last time" (e.g. "dc-") so we do not refuse to take over our own stale
//     lease.
//   - ttl is how long without a heartbeat is considered stale.
//
// When the peer table does not exist yet (sql.ErrNoRows on SELECT) we treat
// this as first-run and allow the promotion.
func CheckPeerLease(
	ctx context.Context,
	peer LeaseTracker,
	now time.Time,
	ttl time.Duration,
	selfOwnerID string,
) error {
	if peer == nil {
		return nil
	}
	lease, err := peer.GetLeaderLease(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read peer lease: %w", err)
	}
	if lease.Owner == selfOwnerID {
		// Our own previous lease — safe.
		return nil
	}
	if !lease.Expired(now, ttl) {
		return fmt.Errorf(
			"%w: owner=%q epoch=%d heartbeat=%s (ttl=%s)",
			ErrPeerLeaseFresh, lease.Owner, lease.Epoch, lease.HeartbeatAt, ttl,
		)
	}
	return nil
}

// AcquireLeaseOnSource is called by the new source right after promotion to
// establish itself as the holder. The caller is expected to invoke it through
// a periodic heartbeat (from the reconciler) so lease.heartbeat_at stays
// current.
func AcquireLeaseOnSource(
	ctx context.Context,
	source LeaseTracker,
	selfOwnerID string,
	ttl time.Duration,
) (pxc.LeaderLease, error) {
	if source == nil {
		return pxc.LeaderLease{}, errors.New("no source lease tracker provided")
	}
	if err := source.EnsureLeaderLeaseSchema(ctx); err != nil {
		return pxc.LeaderLease{}, fmt.Errorf("ensure keeper.leader schema: %w", err)
	}
	lease, err := source.AcquireOrRenewLease(ctx, selfOwnerID, ttl)
	if err != nil {
		return lease, fmt.Errorf("acquire/renew lease: %w", err)
	}
	return lease, nil
}
