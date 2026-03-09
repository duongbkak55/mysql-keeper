package health

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/duongnguyen/mysql-keeper/internal/pxc"
)

// PXCChecker checks the health of a PXC cluster using two methods:
//  1. Kubernetes API — reads the PerconaXtraDBCluster CRD status (local cluster only).
//  2. Direct MySQL — queries wsrep status variables and @@read_only.
type PXCChecker struct {
	// k8sClient is used to query local CRD status. May be nil for remote checks.
	k8sClient client.Client

	// mysqlDSN is the MySQL DSN for direct connection (local or remote).
	mysqlDSN string

	// timeout for each MySQL query.
	timeout time.Duration

	// pxcNamespace and pxcName identify the CRD to fetch (local only).
	pxcNamespace string
	pxcName      string
}

// NewLocalPXCChecker creates a checker for the local (in-cluster) PXC.
// Uses both k8s API and direct MySQL.
func NewLocalPXCChecker(k8sClient client.Client, namespace, name, mysqlDSN string, timeout time.Duration) *PXCChecker {
	return &PXCChecker{
		k8sClient:    k8sClient,
		mysqlDSN:     mysqlDSN,
		timeout:      timeout,
		pxcNamespace: namespace,
		pxcName:      name,
	}
}

// NewRemotePXCChecker creates a checker for the remote cluster.
// Only uses direct MySQL (no cross-cluster k8s API).
func NewRemotePXCChecker(mysqlDSN string, timeout time.Duration) *PXCChecker {
	return &PXCChecker{
		mysqlDSN: mysqlDSN,
		timeout:  timeout,
	}
}

// Check performs all health checks and returns a ClusterHealth snapshot.
func (c *PXCChecker) Check(ctx context.Context) ClusterHealth {
	h := ClusterHealth{
		CheckedAt: time.Now(),
		Healthy:   true,
	}

	// 1. Check local PXC CRD (only if k8s client is configured).
	if c.k8sClient != nil {
		state, readyNodes, err := c.checkPXCCRD(ctx)
		h.PXCState = state
		h.PXCReadyNodes = readyNodes
		if err != nil {
			h.Healthy = false
			h.Message = fmt.Sprintf("PXC CRD check failed: %v", err)
			return h
		}
		if state != "ready" {
			h.Healthy = false
			h.Message = fmt.Sprintf("PXC state is %q (expected 'ready')", state)
			return h
		}
		if readyNodes < 2 {
			h.Healthy = false
			h.Message = fmt.Sprintf("PXC ready nodes %d < 2 (quorum lost)", readyNodes)
			return h
		}
	}

	// 2. Check via direct MySQL.
	wsrepSize, wsrepStatus, isWritable, err := c.checkMySQL(ctx)
	if err != nil {
		h.Healthy = false
		h.Writable = WritableUnknown   // explicitly unknown — not the same as read-only
		h.IsWritable = false
		h.Message = fmt.Sprintf("MySQL unreachable: %v", err)
		return h
	}

	h.WsrepClusterSize = wsrepSize
	h.WsrepClusterStatus = wsrepStatus
	if isWritable {
		h.Writable = WritableYes
		h.IsWritable = true
	} else {
		h.Writable = WritableNo
		h.IsWritable = false
	}

	if wsrepStatus != "Primary" {
		h.Healthy = false
		h.Message = fmt.Sprintf("wsrep_cluster_status=%q (not Primary)", wsrepStatus)
		return h
	}
	if wsrepSize < 2 {
		h.Healthy = false
		h.Message = fmt.Sprintf("wsrep_cluster_size=%d (< 2, quorum lost)", wsrepSize)
		return h
	}

	return h
}

// checkPXCCRD reads PerconaXtraDBCluster CRD status from the local k8s API.
func (c *PXCChecker) checkPXCCRD(ctx context.Context) (state string, readyNodes int32, err error) {
	pxcObj := &pxc.PerconaXtraDBCluster{}
	if err := c.k8sClient.Get(ctx, types.NamespacedName{
		Namespace: c.pxcNamespace,
		Name:      c.pxcName,
	}, pxcObj); err != nil {
		return "", 0, fmt.Errorf("get PerconaXtraDBCluster: %w", err)
	}
	return string(pxcObj.Status.State), pxcObj.Status.PXC.Ready, nil
}

// checkMySQL opens a MySQL connection and queries wsrep status + read_only.
func (c *PXCChecker) checkMySQL(ctx context.Context) (clusterSize int32, clusterStatus string, isWritable bool, err error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	db, err := sql.Open("mysql", c.mysqlDSN)
	if err != nil {
		return 0, "", false, fmt.Errorf("open MySQL connection: %w", err)
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(c.timeout)

	if err := db.PingContext(queryCtx); err != nil {
		return 0, "", false, fmt.Errorf("ping MySQL: %w", err)
	}

	// Query wsrep_cluster_size.
	var varName, varValue string
	row := db.QueryRowContext(queryCtx, "SHOW STATUS LIKE 'wsrep_cluster_size'")
	if err := row.Scan(&varName, &varValue); err != nil {
		return 0, "", false, fmt.Errorf("query wsrep_cluster_size: %w", err)
	}
	if _, err := fmt.Sscanf(varValue, "%d", &clusterSize); err != nil {
		return 0, "", false, fmt.Errorf("parse wsrep_cluster_size %q: %w", varValue, err)
	}

	// Query wsrep_cluster_status.
	row = db.QueryRowContext(queryCtx, "SHOW STATUS LIKE 'wsrep_cluster_status'")
	if err := row.Scan(&varName, &varValue); err != nil {
		return 0, "", false, fmt.Errorf("query wsrep_cluster_status: %w", err)
	}
	clusterStatus = varValue

	// Query read_only.
	var readOnly int
	row = db.QueryRowContext(queryCtx, "SELECT @@read_only")
	if err := row.Scan(&readOnly); err != nil {
		return 0, "", false, fmt.Errorf("query @@read_only: %w", err)
	}
	isWritable = readOnly == 0

	return clusterSize, clusterStatus, isWritable, nil
}
