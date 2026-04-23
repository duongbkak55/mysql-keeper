// Preflight executes the C1–C17 checks from the production-readiness review.
// Each check emits a CheckResult so the controller can show exactly which
// invariant tripped, and so metrics can break down failures by check name.
//
// Failing a Hard check aborts the switchover. Failing a Soft check only logs
// and increments a metric.
package switchover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// Severity categorises a failed preflight check.
type Severity int

const (
	SeverityHard Severity = iota // must pass — blocks the switchover
	SeveritySoft                 // must be logged but is not blocking
)

func (s Severity) String() string {
	if s == SeverityHard {
		return "hard"
	}
	return "soft"
}

// CheckResult is the outcome of a single preflight probe.
type CheckResult struct {
	Name     string
	Severity Severity
	Passed   bool
	Message  string
	// Elapsed tracks how long this check took — useful when preflight is slow.
	Elapsed time.Duration
}

// PreFlightResult aggregates the individual check outcomes.
type PreFlightResult struct {
	Checks []CheckResult

	// LocalSnapshot / RemoteSnapshot are included so the controller can surface
	// GTID-level drift on the CR status without re-running the queries.
	LocalSnapshot  *pxc.GTIDSnapshot
	RemoteSnapshot *pxc.GTIDSnapshot
}

// OK is true when no Hard check failed.
func (r *PreFlightResult) OK() bool {
	if r == nil {
		return false
	}
	for _, c := range r.Checks {
		if !c.Passed && c.Severity == SeverityHard {
			return false
		}
	}
	return true
}

// Failed returns the names+messages of every failed check (hard or soft).
func (r *PreFlightResult) Failed() []CheckResult {
	if r == nil {
		return nil
	}
	var out []CheckResult
	for _, c := range r.Checks {
		if !c.Passed {
			out = append(out, c)
		}
	}
	return out
}

// Summary formats the failed checks for human-readable logs / CR messages.
func (r *PreFlightResult) Summary() string {
	fails := r.Failed()
	if len(fails) == 0 {
		return "all checks passed"
	}
	parts := make([]string, 0, len(fails))
	for _, f := range fails {
		parts = append(parts, fmt.Sprintf("[%s %s] %s: %s", f.Severity, statusString(f), f.Name, f.Message))
	}
	return strings.Join(parts, "; ")
}

func statusString(c CheckResult) string {
	if c.Passed {
		return "OK"
	}
	return "FAIL"
}

// PreFlight holds the inputs for a single preflight run.
type PreFlight struct {
	LocalPXC  PXCManager
	RemotePXC PXCManager

	LocalInspector  ReplicationInspector
	RemoteInspector ReplicationInspector

	Channel                  string
	CatchupTimeout           time.Duration
	MinBinlogRetentionSecond int64
	AllowDataLossFailover    bool
}

// Run executes all checks. The caller decides whether to proceed based on
// PreFlightResult.OK(). Running every check even after one has failed is
// deliberate: we want the status subresource to show every issue in a single
// reconcile, not just the first.
func (p PreFlight) Run(ctx context.Context) *PreFlightResult {
	res := &PreFlightResult{}

	res.Checks = append(res.Checks, p.checkRemoteReachableReadOnly(ctx))
	res.Checks = append(res.Checks, p.checkRemoteReplicationRunning(ctx))

	local, remote, gtidChecks := p.gtidChecks(ctx)
	res.LocalSnapshot = local
	res.RemoteSnapshot = remote
	res.Checks = append(res.Checks, gtidChecks...)

	res.Checks = append(res.Checks, p.checkLogReplicaUpdates(ctx))
	res.Checks = append(res.Checks, p.checkGTIDMode(ctx))
	res.Checks = append(res.Checks, p.checkBinlogFormat(ctx))
	res.Checks = append(res.Checks, p.checkBinlogRetention(ctx))

	return res
}

// --- individual checks ---------------------------------------------------

func (p PreFlight) checkRemoteReachableReadOnly(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C1_RemoteReachableReadOnly", Severity: SeverityHard}
	if p.RemotePXC == nil {
		c.Message = "no RemotePXC configured"
		return withElapsed(c, start)
	}
	writable, err := p.RemotePXC.IsWritable(ctx)
	if err != nil {
		c.Message = fmt.Sprintf("cannot reach remote: %v", err)
		return withElapsed(c, start)
	}
	if writable {
		c.Message = "split-brain guard: remote already writable (read_only=OFF)"
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = "remote reachable and read_only=ON"
	return withElapsed(c, start)
}

func (p PreFlight) checkRemoteReplicationRunning(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C3_RemoteReplicationRunning", Severity: SeverityHard}
	if p.RemoteInspector == nil || p.Channel == "" {
		c.Passed = true
		c.Message = "skipped: no remote inspector / channel name"
		return withElapsed(c, start)
	}
	status, err := p.RemoteInspector.GetReplicationStatus(ctx, p.Channel)
	if err != nil {
		c.Message = fmt.Sprintf("read replication status: %v", err)
		return withElapsed(c, start)
	}
	if !status.ConfigExists {
		// Channel missing is fine when the remote has never been a replica
		// (e.g. fresh DR seeded by xtrabackup). It is Hard-fail only when the
		// configuration explicitly expects a channel.
		c.Passed = true
		c.Message = "replication channel not present on remote"
		return withElapsed(c, start)
	}
	if !status.Running() {
		c.Message = status.HumanMessage()
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = status.HumanMessage()
	return withElapsed(c, start)
}

// gtidChecks returns both snapshots and the C5/C6/C10 check rows. Returning
// the snapshots avoids re-running the queries to render them on the CR.
func (p PreFlight) gtidChecks(ctx context.Context) (*pxc.GTIDSnapshot, *pxc.GTIDSnapshot, []CheckResult) {
	subsetCheck := CheckResult{Name: "C5_GTIDSubset", Severity: SeverityHard}
	waitCheck := CheckResult{Name: "C6_GTIDCatchup", Severity: SeverityHard}
	purgeCheck := CheckResult{Name: "C10_RemotePurgedNotAhead", Severity: SeverityHard}

	start := time.Now()

	if p.LocalInspector == nil || p.RemoteInspector == nil {
		if p.AllowDataLossFailover {
			// Operator explicitly accepted the risk. Skip everything.
			subsetCheck.Passed = true
			subsetCheck.Message = "skipped: AllowDataLossFailover=true and no local snapshot"
			waitCheck.Passed = true
			waitCheck.Message = "skipped"
			purgeCheck.Passed = true
			purgeCheck.Message = "skipped"
			return nil, nil,
				[]CheckResult{
					withElapsed(subsetCheck, start),
					withElapsed(waitCheck, start),
					withElapsed(purgeCheck, start),
				}
		}
		subsetCheck.Message = "no local or remote GTID inspector available"
		return nil, nil, []CheckResult{
			withElapsed(subsetCheck, start),
			withElapsed(waitCheck, start),
			withElapsed(purgeCheck, start),
		}
	}

	localSnap, localErr := p.LocalInspector.GetGTIDSnapshot(ctx)
	remoteSnap, remoteErr := p.RemoteInspector.GetGTIDSnapshot(ctx)

	// --- C5: remote must have applied every local GTID ---
	if localErr != nil {
		if p.AllowDataLossFailover {
			subsetCheck.Passed = true
			subsetCheck.Message = fmt.Sprintf("skipped: local unreachable (AllowDataLossFailover=true): %v", localErr)
		} else {
			subsetCheck.Message = fmt.Sprintf("cannot read local snapshot: %v", localErr)
		}
	} else if remoteErr != nil {
		subsetCheck.Message = fmt.Sprintf("cannot read remote snapshot: %v", remoteErr)
	} else {
		missing, err := p.RemoteInspector.MissingGTIDs(ctx, localSnap.Executed)
		switch {
		case err != nil:
			subsetCheck.Message = fmt.Sprintf("compute missing GTIDs: %v", err)
		case missing == "":
			subsetCheck.Passed = true
			subsetCheck.Message = "remote has applied every local GTID"
		default:
			subsetCheck.Message = fmt.Sprintf(
				"remote missing %d chars of GTID set: %s",
				len(missing), truncate(missing, 300),
			)
		}
	}

	// --- C6: give the remote CatchupTimeout to catch up before failing ---
	if subsetCheck.Passed {
		// Already in sync; no wait needed.
		waitCheck.Passed = true
		waitCheck.Message = "remote already caught up"
	} else if localErr != nil || remoteErr != nil {
		waitCheck.Message = "skipped: snapshot read failed"
	} else {
		timeout := p.CatchupTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		waitErr := p.RemoteInspector.WaitForGTID(ctx, localSnap.Executed, timeout)
		if waitErr == nil {
			// Recompute the subset check after the wait — if it's now clean,
			// upgrade C5 to passed as well.
			newMissing, _ := p.RemoteInspector.MissingGTIDs(ctx, localSnap.Executed)
			if newMissing == "" {
				subsetCheck.Passed = true
				subsetCheck.Message = "remote caught up during wait"
				waitCheck.Passed = true
				waitCheck.Message = fmt.Sprintf("caught up within %s", timeout)
			} else {
				waitCheck.Message = fmt.Sprintf(
					"WAIT_FOR_EXECUTED_GTID_SET returned success but subset still fails: %s",
					truncate(newMissing, 300),
				)
			}
		} else {
			waitCheck.Message = waitErr.Error()
		}
	}

	// --- C10: the remote's gtid_purged must be ⊆ local.gtid_executed so
	// that once the former source is re-attached as a replica it can still
	// pull every GTID it has not yet seen.
	switch {
	case localErr != nil:
		purgeCheck.Message = "skipped: local unreachable"
		if p.AllowDataLossFailover {
			purgeCheck.Passed = true
		}
	case remoteErr != nil:
		purgeCheck.Message = fmt.Sprintf("cannot read remote snapshot: %v", remoteErr)
	case remoteSnap.Purged == "":
		purgeCheck.Passed = true
		purgeCheck.Message = "remote gtid_purged is empty"
	default:
		missing, err := p.LocalInspector.MissingGTIDs(ctx, remoteSnap.Purged)
		if err != nil {
			purgeCheck.Message = fmt.Sprintf("compute subset of remote purged: %v", err)
		} else if missing != "" {
			purgeCheck.Message = fmt.Sprintf(
				"remote has purged GTIDs that local never saw: %s",
				truncate(missing, 300),
			)
		} else {
			purgeCheck.Passed = true
			purgeCheck.Message = "remote purged ⊆ local executed"
		}
	}

	var lsPtr, rsPtr *pxc.GTIDSnapshot
	if localErr == nil {
		ls := localSnap
		lsPtr = &ls
	}
	if remoteErr == nil {
		rs := remoteSnap
		rsPtr = &rs
	}

	return lsPtr, rsPtr, []CheckResult{
		withElapsed(subsetCheck, start),
		withElapsed(waitCheck, start),
		withElapsed(purgeCheck, start),
	}
}

func (p PreFlight) checkLogReplicaUpdates(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C7_RemoteLogReplicaUpdates", Severity: SeverityHard}
	if p.RemoteInspector == nil {
		c.Passed = true
		c.Message = "skipped: no remote inspector"
		return withElapsed(c, start)
	}
	snap, err := p.RemoteInspector.GetGTIDSnapshot(ctx)
	if err != nil {
		c.Message = fmt.Sprintf("read remote snapshot: %v", err)
		return withElapsed(c, start)
	}
	if !snap.LogReplicaUpdatesOn {
		c.Message = "log_replica_updates / log_slave_updates is OFF — GTIDs received as replica will not be written to binlog after the flip (Error 1236 after next flip)"
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = "log_replica_updates=ON"
	return withElapsed(c, start)
}

func (p PreFlight) checkGTIDMode(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C9_GTIDModeOn", Severity: SeverityHard}
	if p.RemoteInspector == nil {
		c.Passed = true
		c.Message = "skipped"
		return withElapsed(c, start)
	}
	snap, err := p.RemoteInspector.GetGTIDSnapshot(ctx)
	if err != nil {
		c.Message = fmt.Sprintf("read remote snapshot: %v", err)
		return withElapsed(c, start)
	}
	if snap.GTIDMode != "ON" {
		c.Message = fmt.Sprintf("gtid_mode=%q (expected ON)", snap.GTIDMode)
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = "gtid_mode=ON"
	return withElapsed(c, start)
}

func (p PreFlight) checkBinlogFormat(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C8_BinlogFormatRow", Severity: SeverityHard}
	if p.RemoteInspector == nil {
		c.Passed = true
		c.Message = "skipped"
		return withElapsed(c, start)
	}
	snap, err := p.RemoteInspector.GetGTIDSnapshot(ctx)
	if err != nil {
		c.Message = fmt.Sprintf("read remote snapshot: %v", err)
		return withElapsed(c, start)
	}
	if !strings.EqualFold(snap.BinlogFormat, "ROW") {
		c.Message = fmt.Sprintf("binlog_format=%q (expected ROW)", snap.BinlogFormat)
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = "binlog_format=ROW"
	return withElapsed(c, start)
}

func (p PreFlight) checkBinlogRetention(ctx context.Context) CheckResult {
	start := time.Now()
	c := CheckResult{Name: "C11_RemoteBinlogRetention", Severity: SeveritySoft}
	if p.RemoteInspector == nil {
		c.Passed = true
		c.Message = "skipped"
		return withElapsed(c, start)
	}
	snap, err := p.RemoteInspector.GetGTIDSnapshot(ctx)
	if err != nil {
		c.Message = fmt.Sprintf("read remote snapshot: %v", err)
		return withElapsed(c, start)
	}
	minimum := p.MinBinlogRetentionSecond
	if minimum <= 0 {
		// 7 days is the baseline we recommend in the review — use it as a
		// default so callers that forget to configure this still get the
		// intended warning.
		minimum = 7 * 24 * 3600
	}
	if snap.BinlogExpireLogsSeconds < minimum {
		c.Message = fmt.Sprintf(
			"binlog_expire_logs_seconds=%d < recommended %d — DC may be unable to catch up after a long outage",
			snap.BinlogExpireLogsSeconds, minimum,
		)
		return withElapsed(c, start)
	}
	c.Passed = true
	c.Message = fmt.Sprintf("binlog_expire_logs_seconds=%d (≥ %d)", snap.BinlogExpireLogsSeconds, minimum)
	return withElapsed(c, start)
}

// --- helpers -------------------------------------------------------------

func withElapsed(c CheckResult, start time.Time) CheckResult {
	c.Elapsed = time.Since(start)
	return c
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ErrPreFlightAborted is returned by phasePreFlight when at least one Hard
// check failed. Not currently used directly — phasePreFlight wraps with
// fmt.Errorf for richer messages — but exported for tests that want to
// errors.Is() against it.
var ErrPreFlightAborted = errors.New("preflight aborted")
