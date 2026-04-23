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

	// Clean slate — otherwise a prior test leaves a row that makes the first
	// acquirer a "renew same owner" case instead of the first-INSERT path.
	// We do this by calling the manager and then resetting.
	if _, err := mgr.GetLeaderLease(ctx); err == nil {
		// row exists — delete it via a raw connection so tests are independent
		dropLeaseRow(ctx, t)
	}

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

// dropLeaseRow clears keeper.leader so each test starts from a known state.
func dropLeaseRow(ctx context.Context, t *testing.T) {
	t.Helper()
	// Reuse the shared rootDSN through a temporary Manager only for its
	// openDB path. We do the DELETE directly rather than expose a public
	// reset helper because the production code never needs one.
	mgr := pxc.NewRemoteManager(dcDSN, 5*time.Second)
	_, _ = mgr.GetLeaderLease(ctx) // warm up the connection pool
	// The Manager does not expose a delete — do a surgical reset by
	// re-creating the schema (which is a no-op) and then issuing a raw
	// DELETE via a short-lived *sql.DB.
	exec := func(stmt string) {
		db, err := openRootDB(dcDSN)
		if err != nil {
			t.Fatalf("openRootDB: %v", err)
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	exec(`DELETE FROM keeper.leader`)
}
