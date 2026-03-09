package health

import (
	"context"
	"fmt"
)

// Checker aggregates health checks for a cluster (local or remote).
type Checker struct {
	pxcChecker      *PXCChecker
	proxySQLChecker *ProxySQLChecker // nil for remote cluster
	minProxySQL     int32
}

// NewLocalChecker creates a Checker for the local cluster (PXC + ProxySQL).
func NewLocalChecker(pxc *PXCChecker, proxysql *ProxySQLChecker, minProxySQL int32) *Checker {
	return &Checker{
		pxcChecker:      pxc,
		proxySQLChecker: proxysql,
		minProxySQL:     minProxySQL,
	}
}

// NewRemoteChecker creates a Checker for the remote cluster (PXC via MySQL only).
func NewRemoteChecker(pxc *PXCChecker) *Checker {
	return &Checker{pxcChecker: pxc}
}

// Check runs all health probes and returns the aggregated ClusterHealth.
func (c *Checker) Check(ctx context.Context) ClusterHealth {
	h := c.pxcChecker.Check(ctx)

	if c.proxySQLChecker != nil {
		count, _ := c.proxySQLChecker.Check(ctx)
		h.ProxySQLHealthy = count
		if count < c.minProxySQL {
			h.Healthy = false
			msg := fmt.Sprintf("ProxySQL healthy instances %d < required %d", count, c.minProxySQL)
			if h.Message != "" {
				h.Message += "; " + msg
			} else {
				h.Message = msg
			}
		}
	}

	return h
}
