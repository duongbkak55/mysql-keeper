package health

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ProxySQLEndpointInfo holds connection parameters for one ProxySQL admin port.
type ProxySQLEndpointInfo struct {
	Host     string
	Port     int32
	Username string
	Password string
}

// ProxySQLChecker checks health of ProxySQL instances via their admin port (6032).
type ProxySQLChecker struct {
	endpoints []ProxySQLEndpointInfo
	timeout   time.Duration
}

// NewProxySQLChecker creates a ProxySQLChecker for the given list of admin endpoints.
func NewProxySQLChecker(endpoints []ProxySQLEndpointInfo, timeout time.Duration) *ProxySQLChecker {
	return &ProxySQLChecker{
		endpoints: endpoints,
		timeout:   timeout,
	}
}

// Check pings each ProxySQL admin port and returns the count of healthy instances.
func (c *ProxySQLChecker) Check(ctx context.Context) (healthyCount int32, results []CheckResult) {
	for _, ep := range c.endpoints {
		result := c.checkOne(ctx, ep)
		results = append(results, result)
		if result.Healthy {
			healthyCount++
		}
	}
	return healthyCount, results
}

func (c *ProxySQLChecker) checkOne(ctx context.Context, ep ProxySQLEndpointInfo) CheckResult {
	name := fmt.Sprintf("proxysql:%s:%d", ep.Host, ep.Port)

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/", ep.Username, ep.Password, ep.Host, ep.Port)

	queryCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return CheckResult{Name: name, Healthy: false, Message: fmt.Sprintf("open: %v", err)}
	}
	defer db.Close()

	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(c.timeout)

	if err := db.PingContext(queryCtx); err != nil {
		return CheckResult{Name: name, Healthy: false, Message: fmt.Sprintf("ping: %v", err)}
	}

	// SELECT 1 as a basic sanity check.
	var v int
	if err := db.QueryRowContext(queryCtx, "SELECT 1").Scan(&v); err != nil {
		return CheckResult{Name: name, Healthy: false, Message: fmt.Sprintf("SELECT 1: %v", err)}
	}

	return CheckResult{Name: name, Healthy: true, Message: "ok"}
}
