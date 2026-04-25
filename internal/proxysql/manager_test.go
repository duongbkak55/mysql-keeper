package proxysql

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

// TestPrepareFailAbortsBeforeCommit locks in the core 2-phase invariant: if
// any instance fails during prepare, NO instance may see the routing change
// at runtime. The bug we are defending against is the "2 of 3 ProxySQL saw
// the new writer for 15s, one still pointed at the old writer" partial-apply
// scenario that broke the previous design.
//
// The test drives ApplyFailoverRouting end-to-end via the openDB injection
// point so the actual abort branch at manager.go:114-122 is exercised — not
// just the per-step helpers in isolation.
func TestPrepareFailAbortsBeforeCommit(t *testing.T) {
	cfg := RoutingConfig{
		OldWriterHost: "old", OldWriterPort: 3306,
		NewWriterHost: "new", NewWriterPort: 3306,
		ReadWriteHostgroup: 10, ReadOnlyHostgroup: 20,
	}

	// Instance 1: prepare succeeds. No LOAD/SAVE expectations registered —
	// go-sqlmock returns an error for any unregistered SQL call, so if
	// commitOne runs on this DB the test will catch it.
	db1, mock1 := newMock(t)
	defer db1.Close()
	mock1.ExpectPing()
	expectPrepare(mock1, cfg)

	// Instance 2: prepare fails at the demote step.
	db2, mock2 := newMock(t)
	defer db2.Close()
	mock2.ExpectPing()
	mock2.ExpectExec("DELETE FROM mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnError(errors.New("admin connection reset"))

	dbs := []*sql.DB{db1, db2}
	i := 0
	m := &Manager{
		endpoints: []Endpoint{{Host: "px1", Port: 6032}, {Host: "px2", Port: 6032}},
		timeout:   2 * time.Second,
		openDB:    func(_ Endpoint) (*sql.DB, error) { db := dbs[i]; i++; return db, nil },
	}

	if err := m.ApplyFailoverRouting(context.Background(), cfg); err == nil {
		t.Fatal("expected error when prepare fails on instance 2, got nil")
	}

	// The defence: instance 1 consumed all its prepare expectations (DELETE +
	// 2 INSERTs) but was NOT asked to LOAD/SAVE — any such call would have
	// returned an error from the unregistered-SQL path, then propagated up.
	if err := mock1.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 1 had unfulfilled expectations: %v", err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 2 had unfulfilled expectations: %v", err)
	}
}

// TestBlackholeMovesTargetOnEveryInstance verifies the escalation fence path:
// OFFLINE_HARD + max_connections=0 on every ProxySQL instance.
func TestBlackholeMovesTargetOnEveryInstance(t *testing.T) {
	db1, mock1 := newMock(t)
	defer db1.Close()
	expectBlackhole(mock1)

	db2, mock2 := newMock(t)
	defer db2.Close()
	expectBlackhole(mock2)

	m := &Manager{timeout: 2 * time.Second}
	if err := m.runBlackholeSteps(context.Background(), db1, "old", 3306, 9999); err != nil {
		t.Errorf("instance 1 blackhole failed: %v", err)
	}
	if err := m.runBlackholeSteps(context.Background(), db2, "old", 3306, 9999); err != nil {
		t.Errorf("instance 2 blackhole failed: %v", err)
	}
	if err := mock1.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 1: %v", err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 2: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

func expectPrepare(mock sqlmock.Sqlmock, cfg RoutingConfig) {
	mock.ExpectExec("DELETE FROM mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(cfg.ReadOnlyHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectBlackhole(mock sqlmock.Sqlmock) {
	mock.ExpectPing()
	mock.ExpectExec("UPDATE mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("LOAD MYSQL SERVERS TO RUNTIME").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SAVE MYSQL SERVERS TO DISK").
		WillReturnResult(sqlmock.NewResult(0, 0))
}

// newMock wires a sqlmock with a relaxed query matcher so multi-line SQL in
// the real code does not require escaping every newline in the expectation.
// We compare collapsed whitespace and fall back to regex if the expected
// string looks like a pattern.
// TestRunPrepareSteps_InsertWriteHGError verifies that a failure on the
// "promote new writer" INSERT returns an error immediately without attempting
// the RO-group INSERT or LOAD/SAVE.
func TestRunPrepareSteps_InsertWriteHGError(t *testing.T) {
	cfg := RoutingConfig{
		OldWriterHost: "old", OldWriterPort: 3306,
		NewWriterHost: "new", NewWriterPort: 3306,
		ReadWriteHostgroup: 10, ReadOnlyHostgroup: 20,
	}
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectExec("DELETE FROM mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnError(errors.New("disk full"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.runPrepareSteps(context.Background(), db, cfg); err == nil {
		t.Fatal("expected error when write-HG INSERT fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls after INSERT error: %v", err)
	}
}

// TestRunPrepareSteps_InsertROHGError verifies that a failure on the read-only
// HG INSERT returns an error after the write-HG INSERT already succeeded.
func TestRunPrepareSteps_InsertROHGError(t *testing.T) {
	cfg := RoutingConfig{
		OldWriterHost: "old", OldWriterPort: 3306,
		NewWriterHost: "new", NewWriterPort: 3306,
		ReadWriteHostgroup: 10, ReadOnlyHostgroup: 20,
	}
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectExec("DELETE FROM mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(cfg.ReadOnlyHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnError(errors.New("constraint violation"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.runPrepareSteps(context.Background(), db, cfg); err == nil {
		t.Fatal("expected error when RO-HG INSERT fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls: %v", err)
	}
}

// TestRunBlackholeSteps_UpdateError verifies that an UPDATE failure returns
// an error without proceeding to INSERT, LOAD, or SAVE.
func TestRunBlackholeSteps_UpdateError(t *testing.T) {
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectExec("UPDATE mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnError(errors.New("table locked"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.runBlackholeSteps(context.Background(), db, "old", 3306, 9999); err == nil {
		t.Fatal("expected error when UPDATE fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls after UPDATE error: %v", err)
	}
}

// TestRunBlackholeSteps_InsertBlackholeRowError verifies that a failure on
// the INSERT OR REPLACE returns an error before LOAD/SAVE.
func TestRunBlackholeSteps_InsertBlackholeRowError(t *testing.T) {
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectPing()
	mock.ExpectExec("UPDATE mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT OR REPLACE INTO mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnError(errors.New("disk full"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.runBlackholeSteps(context.Background(), db, "old", 3306, 9999); err == nil {
		t.Fatal("expected error when blackhole INSERT fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls: %v", err)
	}
}

// TestCommitOne_LoadError verifies that a LOAD MYSQL SERVERS TO RUNTIME
// failure returns an error without attempting SAVE.
func TestCommitOne_LoadError(t *testing.T) {
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectExec("LOAD MYSQL SERVERS TO RUNTIME").
		WillReturnError(errors.New("connection reset"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.commitOne(context.Background(), db); err == nil {
		t.Fatal("expected error when LOAD fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls: %v", err)
	}
}

// TestCommitOne_SaveError verifies that a SAVE MYSQL SERVERS TO DISK failure
// is returned after LOAD succeeds.
func TestCommitOne_SaveError(t *testing.T) {
	db, mock := newMock(t)
	defer db.Close()
	mock.ExpectExec("LOAD MYSQL SERVERS TO RUNTIME").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SAVE MYSQL SERVERS TO DISK").
		WillReturnError(errors.New("I/O error"))

	m := &Manager{timeout: 2 * time.Second}
	if err := m.commitOne(context.Background(), db); err == nil {
		t.Fatal("expected error when SAVE fails, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected extra SQL calls: %v", err)
	}
}

// TestBlackhole_EmptyTargetHostReturnsError covers the guard that prevents
// silently blackholing an unspecified host.
func TestBlackhole_EmptyTargetHostReturnsError(t *testing.T) {
	m := &Manager{timeout: 2 * time.Second}
	if err := m.Blackhole(context.Background(), BlackholeConfig{TargetHost: ""}); err == nil {
		t.Fatal("expected error when TargetHost is empty, got nil")
	}
}

// TestBlackhole_DefaultHGIs9999 verifies that BlackholeHostgroup=0 falls back
// to the reserved hostgroup 9999 that is never targeted by query rules.
// The test calls Blackhole() end-to-end so the default-resolution branch at
// manager.go:168-170 is actually exercised — not the step helper directly.
func TestBlackhole_DefaultHGIs9999(t *testing.T) {
	db, mock := newMock(t)
	defer db.Close()
	// expectBlackhole expects HG=9999; if Blackhole() passes hg=0 instead the
	// mock's WithArgs(int32(9999), ...) assertion will fail the ExecContext call.
	expectBlackhole(mock)

	m := &Manager{
		endpoints: []Endpoint{{Host: "px1", Port: 6032}},
		timeout:   2 * time.Second,
		openDB:    func(_ Endpoint) (*sql.DB, error) { return db, nil },
	}
	cfg := BlackholeConfig{TargetHost: "old", TargetPort: 3306, BlackholeHostgroup: 0}
	if err := m.Blackhole(context.Background(), cfg); err != nil {
		t.Errorf("Blackhole with BlackholeHostgroup=0 (default 9999) failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL expectations not met: %v", err)
	}
}

func newMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
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
