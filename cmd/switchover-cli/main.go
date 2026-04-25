// switchover-cli — drive the full mysql-keeper switchover engine against
// pre-provisioned MySQL + ProxySQL endpoints. Intended for local drills
// against docker-compose so operators can reproduce production bug
// classes (ProxySQL admin auth, 2-phase apply, fence escalation) without
// standing up Kubernetes.
//
// This binary is the LOCAL analogue of what the controller does during
// a Reconcile after it has decided `shouldSwitchover=true`. It:
//   1. Runs the C1..C11 preflight, prints the results.
//   2. If preflight passes (or --allow-data-loss-failover is set), runs
//      the full phased engine: PreFlight → Fence → Promote → Routing →
//      ReverseReplica → Verify.
//   3. Reports pass/fail with the engine's checkpoint trail.
//
// Not a replacement for the controller in production — no CRD, no
// status updates, no metrics export, no leader election. Strictly a
// drill tool.
//
// Usage against scripts/local/pxc-compose.yml:
//
//	switchover-cli \
//	  -dc-mysql 'root:stagingpass@tcp(127.0.0.1:33011)/?parseTime=true' \
//	  -dr-mysql 'root:stagingpass@tcp(127.0.0.1:33012)/?parseTime=true' \
//	  -dc-proxysql '127.0.0.1:36032' \
//	  -dr-proxysql '127.0.0.1:36042' \
//	  -proxysql-user keeper -proxysql-pass stagingpass
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/proxysql"
	"github.com/duongnguyen/mysql-keeper/internal/pxc"
	"github.com/duongnguyen/mysql-keeper/internal/switchover"
)

func main() {
	var (
		dcDSN    = flag.String("dc-mysql", "", "DC MySQL DSN (parseTime=true required)")
		drDSN    = flag.String("dr-mysql", "", "DR MySQL DSN (parseTime=true required)")
		dcProxy  = flag.String("dc-proxysql", "", "DC ProxySQL admin host:port (only the 'local' side is updated during the drill)")
		drProxy  = flag.String("dr-proxysql", "", "DR ProxySQL admin host:port — currently unused by the cli but accepted for symmetry with future two-sided drills")
		proxyU   = flag.String("proxysql-user", "keeper", "ProxySQL admin username — avoid 'admin' which ProxySQL locks to localhost")
		proxyPW  = flag.String("proxysql-pass", "", "ProxySQL admin password")
		channel  = flag.String("channel", "dc-to-dr", "Replication channel name")
		timeout  = flag.Duration("timeout", 5*time.Second, "Per-query timeout against MySQL / ProxySQL")
		dryRun   = flag.Bool("dry-run", false, "Only run preflight; never change state")
		allowDL  = flag.Bool("allow-data-loss-failover", false, "Accept missing local snapshot (local already destroyed) and promote anyway")
		catchup  = flag.Duration("catchup-timeout", 10*time.Second, "C6 WAIT_FOR_EXECUTED_GTID_SET budget")
		minBinl  = flag.Int64("min-binlog-retention-seconds", 7*24*3600, "C11 soft-warn threshold")
		rwHG     = flag.Int("rw-hg", 10, "Writer hostgroup in ProxySQL")
		roHG     = flag.Int("ro-hg", 20, "Reader hostgroup in ProxySQL")
		blackHG  = flag.Int("blackhole-hg", 9999, "Blackhole hostgroup (unrouted)")
		fenceTO  = flag.Duration("fence-timeout", 10*time.Second, "Budget for the SQL fence")
		drainTO  = flag.Duration("drain-timeout", 0, "Pause between PreFlight and Fence (0 = skip)")
		dcWriter = flag.String("dc-writer-host", "pxc-dc", "Host value for DC writer in ProxySQL mysql_servers — must match proxysql cfg")
		drWriter = flag.String("dr-writer-host", "pxc-dr", "Host value for DR writer")
	)
	flag.Parse()

	if *dcDSN == "" || *drDSN == "" {
		die("usage: --dc-mysql and --dr-mysql are required")
	}
	if !*dryRun && *dcProxy == "" {
		die("usage: --dc-proxysql required unless --dry-run")
	}
	if !*dryRun && *proxyPW == "" {
		die("usage: --proxysql-pass required unless --dry-run")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *catchup+2*time.Minute)
	defer cancel()

	local := pxc.NewRemoteManager(*dcDSN, *timeout)
	remote := pxc.NewRemoteManager(*drDSN, *timeout)

	// Always run preflight first so operators see the same report whether
	// the drill proceeds or aborts.
	pf := switchover.PreFlight{
		LocalPXC:                 local,
		RemotePXC:                remote,
		LocalInspector:           local,
		RemoteInspector:          remote,
		Channel:                  *channel,
		CatchupTimeout:           *catchup,
		MinBinlogRetentionSecond: *minBinl,
		AllowDataLossFailover:    *allowDL,
	}
	pfRes := pf.Run(ctx)
	printPreflight(pfRes)
	if !pfRes.OK() {
		fmt.Println("\nPreFlight failed — switchover will not proceed.")
		os.Exit(1)
	}
	if *dryRun {
		fmt.Println("\n--dry-run specified — preflight passed but not executing engine.")
		os.Exit(0)
	}

	// Build the ProxySQL manager. Only the DC-side proxy is used here —
	// that is the one routing app traffic at the beginning of the drill.
	// After the flip it will point writes at DR; the DR-side ProxySQL is
	// expected to already have the DR writer entry ready (see
	// scripts/local/proxysql-dr.cnf). A production drill may configure
	// both sides and this binary could grow an --also-update-dr-proxy
	// flag later.
	_ = drProxy // reserved for future use
	endpoints := []proxysql.Endpoint{{
		Host:     hostOnly(*dcProxy),
		Port:     portOnly(*dcProxy),
		Username: *proxyU,
		Password: *proxyPW,
	}}
	proxy := proxysql.NewManager(endpoints, *timeout)

	// Drive the full engine. LocalPXC / LocalReplication / etc. are the
	// same *pxc.Manager — the interface split in engine.go is for test
	// mocking; at runtime the same concrete object satisfies all
	// interfaces.
	cfg := switchover.Config{
		LocalPXC:                  local,
		RemotePXC:                 remote,
		LocalInspector:            local,
		RemoteInspector:           remote,
		LocalReplication:          local,
		RemoteReplication:         remote,
		LocalProxySQL:             proxy,
		LocalReplicationChannel:   *channel,
		RemoteReplicationChannel:  *channel,
		Routing: proxysql.RoutingConfig{
			OldWriterHost:      *dcWriter,
			OldWriterPort:     3306,
			NewWriterHost:      *drWriter,
			NewWriterPort:      3306,
			ReadWriteHostgroup: int32(*rwHG),
			ReadOnlyHostgroup:  int32(*roHG),
		},
		BlackholeFence: proxysql.BlackholeConfig{
			TargetHost:         *dcWriter,
			TargetPort:         3306,
			BlackholeHostgroup: int32(*blackHG),
		},
		CatchupTimeout:            *catchup,
		MinBinlogRetentionSeconds: *minBinl,
		FenceTimeout:              *fenceTO,
		DrainTimeout:              *drainTO,
		AllowDataLossFailover:     *allowDL,
		Reason:                    "switchover-cli drill",
		AttemptID:                 fmt.Sprintf("drill-%d", time.Now().Unix()),
		Progress:                  &stdoutReporter{},
	}
	engine := switchover.NewEngine(cfg)

	fmt.Println("\n=== ENGINE EXECUTE ===")
	res := engine.Execute(ctx)
	if res.Success {
		fmt.Println("\n✅ Switchover completed successfully.")
		os.Exit(0)
	}
	fmt.Printf("\n❌ Switchover failed at phase %s — rolledBack=%v\n    %v\n",
		res.FailedPhase, res.RolledBack, res.Error)
	os.Exit(1)
}

type stdoutReporter struct{ completed []switchover.Phase }

func (r *stdoutReporter) OnPhaseStart(_ context.Context, p switchover.Phase) {
	fmt.Printf("  ▶ %s ...\n", p)
}
func (r *stdoutReporter) OnPhaseComplete(_ context.Context, p switchover.Phase) {
	r.completed = append(r.completed, p)
	fmt.Printf("    ✓ %s\n", p)
}
func (r *stdoutReporter) OnPhaseError(_ context.Context, p switchover.Phase, err error) {
	fmt.Printf("    ✗ %s — %v\n", p, err)
}

func printPreflight(res *switchover.PreFlightResult) {
	fmt.Println("=== PREFLIGHT ===")
	for _, c := range res.Checks {
		status := "PASS"
		if !c.Passed {
			status = "FAIL"
		}
		fmt.Printf("  %s  [%s] %s  (%dms)\n",
			status, c.Severity, c.Name, c.Elapsed.Milliseconds())
		if c.Message != "" {
			fmt.Printf("          %s\n", c.Message)
		}
	}
}

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(2)
}

// hostOnly / portOnly — minimal "host:port" splitter. We avoid
// net.SplitHostPort because it fails on bare ports and we want a
// friendly error path for the --dc-proxysql flag.
func hostOnly(hp string) string {
	for i := len(hp) - 1; i >= 0; i-- {
		if hp[i] == ':' {
			return hp[:i]
		}
	}
	return hp
}

func portOnly(hp string) int32 {
	for i := len(hp) - 1; i >= 0; i-- {
		if hp[i] == ':' {
			var p int32
			for _, c := range hp[i+1:] {
				if c < '0' || c > '9' {
					return 6032
				}
				p = p*10 + int32(c-'0')
			}
			return p
		}
	}
	return 6032
}
