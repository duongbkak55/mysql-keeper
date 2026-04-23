//go:build ignore

// Diagnostic: time a single ping against a DSN. Useful when preflight-cli
// reports "context deadline exceeded" and we want to isolate whether the
// problem is auth, TLS, or something higher up.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: go run ping-test.go '<dsn>'")
		os.Exit(2)
	}
	dsn := os.Args[1]

	fmt.Printf("Opening %s\n", dsn)
	t0 := time.Now()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Printf("sql.Open: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	fmt.Printf("Open took: %s\n", time.Since(t0))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t1 := time.Now()
	if err := db.PingContext(ctx); err != nil {
		fmt.Printf("Ping failed after %s: %v\n", time.Since(t1), err)
		os.Exit(1)
	}
	fmt.Printf("Ping took: %s\n", time.Since(t1))

	t2 := time.Now()
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		fmt.Printf("Query failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("SELECT 1 took: %s (result=%d)\n", time.Since(t2), one)
}
