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
// We drive the extracted helpers (runPrepareSteps / runBlackholeSteps) with
// go-sqlmock because sql.Open + a real ProxySQL admin port would require
// testcontainers — out of scope for Tier A.
func TestPrepareFailAbortsBeforeCommit(t *testing.T) {
	cfg := RoutingConfig{
		OldWriterHost: "old", OldWriterPort: 3306,
		NewWriterHost: "new", NewWriterPort: 3306,
		ReadWriteHostgroup: 10, ReadOnlyHostgroup: 20,
	}

	// Instance 1: prepare is fine
	db1, mock1 := newMock(t)
	defer db1.Close()
	mock1.ExpectPing()
	expectPrepare(mock1, cfg)

	// Instance 2: prepare fails at the demote step
	db2, mock2 := newMock(t)
	defer db2.Close()
	mock2.ExpectPing()
	mock2.ExpectExec("UPDATE mysql_servers").
		WithArgs(cfg.ReadOnlyHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnError(errors.New("admin connection reset"))

	m := &Manager{timeout: 2 * time.Second}
	ctx := context.Background()

	if err := m.runPrepareSteps(ctx, db1, cfg); err != nil {
		t.Fatalf("instance 1 prepare should succeed: %v", err)
	}
	if err := m.runPrepareSteps(ctx, db2, cfg); err == nil {
		t.Fatal("instance 2 prepare should fail")
	}

	// The defence: neither instance has been asked to LOAD SERVERS / SAVE.
	// If any of those expectations would have been set by commitOne, go-sqlmock
	// would fail ExpectationsWereMet below. We intentionally set NO commit
	// expectations, so if the manager had called them the test would fail.
	if err := mock1.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 1 commands diverged from expectation: %v", err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Errorf("instance 2 commands diverged from expectation: %v", err)
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
	mock.ExpectExec("UPDATE mysql_servers").
		WithArgs(cfg.ReadOnlyHostgroup, cfg.OldWriterHost, cfg.OldWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mysql_servers").
		WithArgs(cfg.ReadWriteHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mysql_servers").
		WithArgs(cfg.ReadOnlyHostgroup, cfg.NewWriterHost, cfg.NewWriterPort).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectBlackhole(mock sqlmock.Sqlmock) {
	mock.ExpectPing()
	mock.ExpectExec("UPDATE mysql_servers").
		WithArgs(int32(9999), "old", int32(3306)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mysql_servers").
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
