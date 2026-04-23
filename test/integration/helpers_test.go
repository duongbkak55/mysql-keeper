//go:build integration

package integration_test

import (
	"database/sql"
	"time"
)

// openRootDB is a tiny helper that returns a short-lived *sql.DB for raw
// administrative statements the production Manager types do not expose. Used
// by test cleanup routines only.
func openRootDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(5 * time.Second)
	return db, nil
}
