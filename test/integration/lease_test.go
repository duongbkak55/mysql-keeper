//go:build integration

package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// TestInteg_LeaderLease_Concurrent simulates two controllers racing against
// the same lease row. Only one may take the initial epoch; the other must
// hit ErrLeaseHeldElsewhere.
//
// This is the real-DB counterpart of the mocked pxc/leader_lease_test.go —
// it proves that our SELECT ... FOR UPDATE really does serialise across
// separate connections as we expect.
func TestInteg_LeaderLease_Concurrent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := pxc.NewRemoteManager(dcDSN, 5*time.Second)
	if err := mgr.EnsureLeaderLeaseSchema(ctx); err != nil {
		t.Fatalf("EnsureLeaderLeaseSchema: %v", err)
	}

	// Reset the singleton row to the "empty owner" sentinel so both test
	// goroutines see the same starting state. We do not DELETE because
	// EnsureLeaderLeaseSchema's seed is now part of the invariant — a plain
	// UPDATE keeps the row's existence and just blanks the owner.
	resetLeaseRow(ctx, t)

	const controllerA = "dc-controller-A"
	const controllerB = "dr-controller-B"

	var wg sync.WaitGroup
	wg.Add(2)

	results := make(chan leaseOutcome, 2)

	acquire := func(owner string) {
		defer wg.Done()
		lease, err := mgr.AcquireOrRenewLease(ctx, owner, 30*time.Second)
		results <- leaseOutcome{owner: owner, lease: lease, err: err}
	}

	go acquire(controllerA)
	go acquire(controllerB)

	wg.Wait()
	close(results)

	var winners, refused int
	var winnerOwner string
	for r := range results {
		switch {
		case r.err == nil:
			winners++
			winnerOwner = r.lease.Owner
		case errors.Is(r.err, pxc.ErrLeaseHeldElsewhere):
			refused++
		default:
			t.Fatalf("unexpected error for %s: %v", r.owner, r.err)
		}
	}

	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
	if refused != 1 {
		t.Errorf("expected exactly 1 ErrLeaseHeldElsewhere, got %d", refused)
	}
	if winnerOwner != controllerA && winnerOwner != controllerB {
		t.Errorf("winner owner is neither A nor B: %q", winnerOwner)
	}

	// Sanity: epoch should be 1 (first acquisition).
	final, err := mgr.GetLeaderLease(ctx)
	if err != nil {
		t.Fatalf("GetLeaderLease final: %v", err)
	}
	if final.Epoch != 1 {
		t.Errorf("expected epoch=1 after first concurrent acquire, got %d", final.Epoch)
	}
}

type leaseOutcome struct {
	owner string
	lease pxc.LeaderLease
	err   error
}

// resetLeaseRow restores keeper.leader to the post-seed state so each test
// starts from a known baseline. Production code never needs this helper —
// it exists purely so tests can be order-independent.
func resetLeaseRow(ctx context.Context, t *testing.T) {
	t.Helper()
	db, err := openRootDB(dcDSN)
	if err != nil {
		t.Fatalf("openRootDB: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
		UPDATE keeper.leader
		SET owner = '', epoch = 0,
		    acquired_at = NOW(6),
		    heartbeat_at = '1970-01-01 00:00:01',
		    renewed_by = ''
		WHERE id = 1
	`); err != nil {
		t.Fatalf("reset keeper.leader: %v", err)
	}
}
