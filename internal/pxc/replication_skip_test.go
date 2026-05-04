package pxc

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestValidGTIDLiteral_AcceptsRealisticForms covers the canonical GTID forms
// MySQL emits: single-transaction `<UUID>:N`, ranges `<UUID>:1-100`, and
// multi-source intervals separated by ':'.
func TestValidGTIDLiteral_AcceptsRealisticForms(t *testing.T) {
	cases := []string{
		"3E11FA47-71CA-11E1-9E33-C80AA9429562:1",
		"3e11fa47-71ca-11e1-9e33-c80aa9429562:1-100",
		"3E11FA47-71CA-11E1-9E33-C80AA9429562:1-100:200-300",
		"abcdef0123456789",
	}
	for _, c := range cases {
		if !validGTIDLiteral(c) {
			t.Errorf("expected %q to be valid", c)
		}
	}
}

// TestValidGTIDLiteral_RejectsInjection guards against any character that
// could break out of the embedded single-quoted SQL literal in
// SkipNextTransaction. The set here is the minimum that MUST be rejected.
func TestValidGTIDLiteral_RejectsInjection(t *testing.T) {
	cases := []string{
		"",                                 // empty
		"abc:1; DROP TABLE foo",            // statement injection
		"abc:1' OR '1'='1",                 // quote escape
		"abc:1\\",                          // backslash
		"abc:1 -- comment",                 // space
		"abc:1\nBEGIN;COMMIT",              // newline
		"abc:1/*comment*/",                 // block comment
		"3E11FA47-71CA-11E1-9E33-XYZAA9", // 'x','y','z' outside hex
	}
	for _, c := range cases {
		if validGTIDLiteral(c) {
			t.Errorf("expected %q to be REJECTED", c)
		}
	}
}

// ---- restoreApplier cleanup-path tests ------------------------------------

const testGTID = "3E11FA47-71CA-11E1-9E33-C80AA9429562:1"
const testChannel = "pxc-async"

// TestSkipNextTransaction_CommitFails verifies that when COMMIT returns an
// error the cleanup path issues ROLLBACK followed by SET gtid_next=AUTOMATIC
// before returning the original error.
func TestSkipNextTransaction_CommitFails(t *testing.T) {
	db, mock := newSkipMock(t)
	defer db.Close()

	commitErr := errors.New("simulated commit failure")

	// Step 1: STOP REPLICA SQL_THREAD
	mock.ExpectExec("STOP REPLICA SQL_THREAD FOR CHANNEL 'pxc-async'").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Step 2: SET gtid_next
	mock.ExpectExec("SET gtid_next = '" + testGTID + "'").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Step 3: BEGIN
	mock.ExpectExec("BEGIN").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Step 4: COMMIT — fails
	mock.ExpectExec("COMMIT").
		WillReturnError(commitErr)
	// Cleanup: ROLLBACK (issued with a fresh context.WithoutCancel context)
	mock.ExpectExec("ROLLBACK").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Cleanup: SET gtid_next = AUTOMATIC
	mock.ExpectExec("SET gtid_next = AUTOMATIC").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Cleanup: START REPLICA SQL_THREAD
	mock.ExpectExec("START REPLICA SQL_THREAD FOR CHANNEL 'pxc-async'").
		WillReturnResult(sqlmock.NewResult(0, 0))

	m := &Manager{timeout: 2 * time.Second}
	err := m.runSkipNextTransaction(context.Background(), db, testChannel, testGTID)

	if err == nil {
		t.Fatal("expected non-nil error when COMMIT fails, got nil")
	}
	if !strings.Contains(err.Error(), "commit empty txn") {
		t.Errorf("error should mention commit failure, got: %v", err)
	}
	if err2 := mock.ExpectationsWereMet(); err2 != nil {
		t.Errorf("unmet mock expectations: %v", err2)
	}
}

// TestSkipNextTransaction_ContextCancelledBeforeReset verifies that when the
// working context returns DeadlineExceeded at step 5 (SET gtid_next=AUTOMATIC),
// the recovery path still issues SET gtid_next=AUTOMATIC because restoreApplier
// uses context.WithoutCancel(ctx) for its cleanup context.
//
// We inject the error directly via sqlmock rather than relying on a real 1ns
// timeout, which is racy: a sub-nanosecond scheduler delay could expire the
// context before steps 1–4 run, making the mock expectations unmet.
func TestSkipNextTransaction_ContextCancelledBeforeReset(t *testing.T) {
	db, mock := newSkipMock(t)
	defer db.Close()

	// Steps 1–4 succeed normally.
	mock.ExpectExec("STOP REPLICA SQL_THREAD FOR CHANNEL 'pxc-async'").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SET gtid_next = '" + testGTID + "'").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("BEGIN").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("COMMIT").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Step 5 (main path): simulate qCtx deadline exceeded — triggers restoreApplier.
	mock.ExpectExec("SET gtid_next = AUTOMATIC").
		WillReturnError(context.DeadlineExceeded)
	// restoreApplier uses context.WithoutCancel(ctx) so it issues the reset
	// on a fresh, healthy context regardless of the expired qCtx.
	mock.ExpectExec("SET gtid_next = AUTOMATIC").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("START REPLICA SQL_THREAD FOR CHANNEL 'pxc-async'").
		WillReturnResult(sqlmock.NewResult(0, 0))

	m := &Manager{timeout: 2 * time.Second}
	_ = m.runSkipNextTransaction(context.Background(), db, testChannel, testGTID)

	if err2 := mock.ExpectationsWereMet(); err2 != nil {
		t.Errorf("SET gtid_next=AUTOMATIC was not issued after ctx expiry: %v", err2)
	}
}

// newSkipMock creates a sqlmock DB with a substring/prefix query matcher,
// mirroring the pattern used in leader_lease_test.go.
func newSkipMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
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
				return errors.New("expected=" + e + " actual=" + a)
			},
		)),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return raw, m
}
