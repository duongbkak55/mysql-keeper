package pxc

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestLease_FirstAcquireOnSeededRow covers the cold-start path where
// EnsureLeaderLeaseSchema has pre-seeded keeper.leader with owner=''.
// AcquireOrRenewLease sees owner=='' (treated as available) and takes over,
// bumping epoch from 0 to 1.
func TestLease_FirstAcquireOnSeededRow(t *testing.T) {
	db, mock := newLeaseMock(t)
	defer db.Close()

	epoch0 := time.Unix(1, 0).UTC()
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"owner", "epoch", "acquired_at", "heartbeat_at"}).
		AddRow("", int64(0), epoch0, epoch0)
	mock.ExpectQuery("SELECT owner, epoch, acquired_at, heartbeat_at FROM keeper.leader WHERE id = 1 FOR UPDATE").
		WillReturnRows(rows)
	mock.ExpectExec("UPDATE keeper.leader SET owner = ?, epoch = ?, acquired_at = NOW").
		WithArgs("dc-controller", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	m := &Manager{dsn: "mock", timeout: 2 * time.Second}
	lease, err := m.runAcquireOrRenew(context.Background(), db, "dc-controller", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lease.Owner != "dc-controller" || lease.Epoch != 1 {
		t.Errorf("expected dc-controller epoch=1, got %+v", lease)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestLease_RenewSameOwner simulates a periodic heartbeat: the row already
// names us as owner, so only heartbeat_at is bumped (epoch stays).
func TestLease_RenewSameOwner(t *testing.T) {
	db, mock := newLeaseMock(t)
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"owner", "epoch", "acquired_at", "heartbeat_at"}).
		AddRow("dc-controller", int64(5), now.Add(-5*time.Minute), now.Add(-10*time.Second))
	mock.ExpectQuery("SELECT owner, epoch, acquired_at, heartbeat_at FROM keeper.leader WHERE id = 1 FOR UPDATE").
		WillReturnRows(rows)
	mock.ExpectExec("UPDATE keeper.leader SET heartbeat_at = NOW").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	m := &Manager{dsn: "mock", timeout: 2 * time.Second}
	lease, err := m.runAcquireOrRenew(context.Background(), db, "dc-controller", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lease.Epoch != 5 {
		t.Errorf("expected epoch unchanged at 5, got %d", lease.Epoch)
	}
	if lease.Owner != "dc-controller" {
		t.Errorf("expected owner dc-controller, got %q", lease.Owner)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestLease_StaleOwnerTakeover verifies that a peer whose heartbeat has gone
// past the TTL is considered stale and we take over with epoch+1.
func TestLease_StaleOwnerTakeover(t *testing.T) {
	db, mock := newLeaseMock(t)
	defer db.Close()

	stale := time.Now().UTC().Add(-5 * time.Minute) // far past 30s TTL
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"owner", "epoch", "acquired_at", "heartbeat_at"}).
		AddRow("dr-controller", int64(7), stale.Add(-time.Hour), stale)
	mock.ExpectQuery("SELECT owner, epoch, acquired_at, heartbeat_at FROM keeper.leader WHERE id = 1 FOR UPDATE").
		WillReturnRows(rows)
	// Takeover with epoch=8
	mock.ExpectExec("UPDATE keeper.leader SET owner = ?, epoch = ?, acquired_at = NOW").
		WithArgs("dc-controller", int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	m := &Manager{dsn: "mock", timeout: 2 * time.Second}
	lease, err := m.runAcquireOrRenew(context.Background(), db, "dc-controller", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lease.Owner != "dc-controller" || lease.Epoch != 8 {
		t.Errorf("expected takeover to dc-controller epoch=8, got %+v", lease)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// TestLease_FreshOwnerRefused is the split-brain-controller guard: a peer with
// a fresh heartbeat still owns the lease and we must back off.
func TestLease_FreshOwnerRefused(t *testing.T) {
	db, mock := newLeaseMock(t)
	defer db.Close()

	fresh := time.Now().UTC().Add(-2 * time.Second) // well within 30s TTL
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"owner", "epoch", "acquired_at", "heartbeat_at"}).
		AddRow("dr-controller", int64(12), fresh.Add(-time.Minute), fresh)
	mock.ExpectQuery("SELECT owner, epoch, acquired_at, heartbeat_at FROM keeper.leader WHERE id = 1 FOR UPDATE").
		WillReturnRows(rows)
	mock.ExpectRollback()

	m := &Manager{dsn: "mock", timeout: 2 * time.Second}
	lease, err := m.runAcquireOrRenew(context.Background(), db, "dc-controller", 30*time.Second)
	if !errors.Is(err, ErrLeaseHeldElsewhere) {
		t.Fatalf("expected ErrLeaseHeldElsewhere, got %v", err)
	}
	if lease.Owner != "dr-controller" {
		t.Errorf("expected returned lease to reflect current owner dr-controller, got %q", lease.Owner)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// --- helpers -------------------------------------------------------------

func newLeaseMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	raw, m, err := sqlmock.New(
		sqlmock.MonitorPingsOption(true),
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(
			func(expected, actual string) error {
				e := strings.Join(strings.Fields(expected), " ")
				a := strings.Join(strings.Fields(actual), " ")
				if strings.HasPrefix(a, e) || strings.Contains(a, e) {
					return nil
				}
				if re, err := regexp.Compile(e); err == nil && re.MatchString(a) {
					return nil
				}
				return errors.New("expected=" + e + " actual=" + a)
			},
		)),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return raw, m
}
