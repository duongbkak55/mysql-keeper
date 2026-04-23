// preflight-cli — run the switchover preflight checklist against two
// already-provisioned MySQL clusters and print the result.
//
// Useful for:
//   - bug reproduction on a local PXC compose staging
//   - DBA debugging ("why is the controller refusing to flip?") — the
//     binary runs exactly the same C1..C11 checks as the engine.
//
// Exit codes:
//
//	0  all hard checks passed
//	1  at least one hard check failed
//	2  arg / setup error
//
// Usage:
//
//	preflight-cli \
//	  -dc='root:pw@tcp(127.0.0.1:33011)/?parseTime=true' \
//	  -dr='root:pw@tcp(127.0.0.1:33012)/?parseTime=true' \
//	  -channel=dc-to-dr
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

func main() {
	var (
		dcDSN                 = flag.String("dc", "", "DC MySQL DSN (must include parseTime=true)")
		drDSN                 = flag.String("dr", "", "DR MySQL DSN (must include parseTime=true)")
		channel               = flag.String("channel", "dc-to-dr", "replication channel name")
		catchupTimeout        = flag.Duration("catchup-timeout", 15*time.Second, "C6 WAIT_FOR_EXECUTED_GTID_SET budget")
		minBinlogRetention    = flag.Int64("min-binlog-retention-seconds", 7*24*3600, "C11 threshold")
		queryTimeout          = flag.Duration("query-timeout", 5*time.Second, "per-query timeout against each cluster")
		allowDataLossFailover = flag.Bool("allow-data-loss-failover", false, "treat missing local snapshot as acceptable (promote anyway)")
	)
	flag.Parse()

	if *dcDSN == "" || *drDSN == "" {
		fmt.Fprintln(os.Stderr, "preflight-cli: -dc and -dr are required")
		flag.Usage()
		os.Exit(2)
	}

	local := pxc.NewRemoteManager(*dcDSN, *queryTimeout)
	remote := pxc.NewRemoteManager(*drDSN, *queryTimeout)

	ctx, cancel := context.WithTimeout(context.Background(),
		*catchupTimeout+30*time.Second)
	defer cancel()

	pf := switchover.PreFlight{
		LocalPXC:                 local,
		RemotePXC:                remote,
		LocalInspector:           local,
		RemoteInspector:          remote,
		Channel:                  *channel,
		CatchupTimeout:           *catchupTimeout,
		MinBinlogRetentionSecond: *minBinlogRetention,
		AllowDataLossFailover:    *allowDataLossFailover,
	}

	res := pf.Run(ctx)

	// Colorised terminal output when stdout is a TTY. We keep the same
	// columns regardless so CI log grepping stays simple.
	tty := isTerminal(os.Stdout)
	green := func(s string) string { return s }
	red := func(s string) string { return s }
	dim := func(s string) string { return s }
	if tty {
		green = func(s string) string { return "\033[32m" + s + "\033[0m" }
		red = func(s string) string { return "\033[31m" + s + "\033[0m" }
		dim = func(s string) string { return "\033[2m" + s + "\033[0m" }
	}

	fmt.Println()
	fmt.Println(dim("PREFLIGHT CHECK RESULTS"))
	fmt.Println(dim("-----------------------"))
	for _, c := range res.Checks {
		status := red("FAIL")
		if c.Passed {
			status = green("PASS")
		}
		fmt.Printf("  %s  [%s] %s  (%dms)\n", status, c.Severity, c.Name, c.Elapsed.Milliseconds())
		if c.Message != "" {
			fmt.Printf("          %s\n", dim(c.Message))
		}
	}
	fmt.Println()

	if res.OK() {
		fmt.Println(green("OVERALL: OK — switchover is safe to proceed."))
		os.Exit(0)
	}
	fmt.Println(red("OVERALL: BLOCKED — switchover must not proceed."))
	os.Exit(1)
}

// isTerminal — tiny TTY detector. We avoid pulling in golang.org/x/term
// for just this: a single Stat call is enough to decide whether we paint
// ANSI colours. Non-TTY stdout (file, pipe) keeps bytes plain.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
