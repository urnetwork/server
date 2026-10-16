// Statistics publication joins error recovery, including its diagnostic work.
package model

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The first diagnostic blocks after the query has unwound and before the
// rescue handler has published its error. Later diagnostics reuse the result.
type auditStatsRecoveryError struct {
	entered  chan struct{}
	released chan struct{}
	once     sync.Once
}

// Classification cannot finish until the test releases this barrier.
func (self *auditStatsRecoveryError) Error() string {
	self.once.Do(func() {
		close(self.entered)
		<-self.released
	})
	return "synthetic device statistics query failed"
}

// The real chunk walker receives only the rows each statement would return.
type auditStatsRecoveryConn struct {
	query func(string, ...any) (server.PgResult, error)
}

// Query ownership remains at computeStatsDeviceWindowParallel's boundary.
func (self *auditStatsRecoveryConn) Query(_ context.Context, sql string, args ...any) (server.PgResult, error) {
	return self.query(sql, args...)
}

// An empty or single-row result implements exactly the consumed row methods.
type auditStatsRecoveryRows struct {
	pgx.Rows
	hasRow bool
	scan   func(...any) error
}

// Advancing once consumes the synthetic row.
func (self *auditStatsRecoveryRows) Next() bool {
	hasRow := self.hasRow
	self.hasRow = false
	return hasRow
}

// The query's typed result owns scanning, including injected scan failures.
func (self *auditStatsRecoveryRows) Scan(destinations ...any) error {
	return self.scan(destinations...)
}

// The in-memory result has no retained wire resources.
func (self *auditStatsRecoveryRows) Close() {}

// Statement iteration itself succeeds in these query/scan failure fixtures.
func (self *auditStatsRecoveryRows) Err() error { return nil }

// A real parallel query failure cannot publish the healthy chain's partial sum.
func TestComputeStatsDeviceWindowJoinsQueryFailureRecovery(t *testing.T) {
	testComputeStatsDeviceWindowJoinsRecovery(t, false)
}

// A failure while decoding a chain has the same publication boundary.
func TestComputeStatsDeviceWindowJoinsScanFailureRecovery(t *testing.T) {
	testComputeStatsDeviceWindowJoinsRecovery(t, true)
}

// A histogram forces two chains; one contributes a real span while the other's
// recovery is held. Synctest proves the join without timing or polling.
func testComputeStatsDeviceWindowJoinsRecovery(t *testing.T, failScan bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		failure := &auditStatsRecoveryError{entered: make(chan struct{}), released: make(chan struct{})}
		release := sync.OnceFunc(func() { close(failure.released) })
		defer release()
		cutoff := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
		startDay := cutoff.Format(time.DateOnly)
		endDay := cutoff.AddDate(0, 0, 1).Format(time.DateOnly)
		bounds := fmt.Sprintf("{%s,%s}", auditStatsTestId(1), auditStatsTestId(100))
		healthySpanRead := make(chan struct{})
		conn := &auditStatsRecoveryConn{query: func(sql string, args ...any) (server.PgResult, error) {
			if sql == computeStatsDeviceChainBoundsSql {
				return &auditStatsRecoveryRows{hasRow: true, scan: func(destinations ...any) error {
					*destinations[0].(**string) = &bounds
					return nil
				}}, nil
			}
			arguments := args[0].(server.PgNamedArgs)
			failedChain := arguments["chunkStart"].(server.Id) == (server.Id{})
			if sql == computeStatsDeviceChunkEndSql(true) || sql == computeStatsDeviceChunkEndSql(false) {
				if failedChain && !failScan {
					return nil, failure
				}
				return &auditStatsRecoveryRows{}, nil
			}
			if sql != computeStatsDeviceSpansSql(true) && sql != computeStatsDeviceSpansSql(false) {
				panic("unexpected synthetic device statistics query")
			}
			return &auditStatsRecoveryRows{hasRow: true, scan: func(destinations ...any) error {
				if failedChain {
					return failure
				}
				*destinations[0].(*string) = startDay
				*destinations[1].(**string) = nil
				*destinations[2].(*int) = 9
				close(healthySpanRead)
				return nil
			}}, nil
		}}
		query := func(run func(server.PgCanQuery)) { run(conn) }
		previousData := map[string]int{"synthetic-previous-snapshot": 7}
		stats := &Stats{DevicesData: maps.Clone(previousData), DevicesSummary: 7}
		returned := make(chan any, 1)
		go func() {
			defer func() { returned <- recover() }()
			computeStatsDeviceWindowParallel(context.Background(), stats, query, 2, startDay, endDay, cutoff, 100)
		}()
		<-failure.entered
		synctest.Wait()
		select {
		case <-healthySpanRead:
		default:
			t.Error("the independent healthy chain did not supply its span")
		}
		var recovered any
		premature := false
		select {
		case recovered = <-returned:
			premature = true
			t.Error("statistics returned while a chain's error recovery was still blocked")
		default:
		}
		if !maps.Equal(stats.DevicesData, previousData) || stats.DevicesSummary != 7 {
			t.Errorf("blocked recovery published partial statistics: %+v, summary %d", stats.DevicesData, stats.DevicesSummary)
		}
		release()
		synctest.Wait()
		if !premature {
			recovered = <-returned
		}
		if recovered != failure {
			t.Errorf("query failure was not propagated: recovered %T", recovered)
		}
		if !maps.Equal(stats.DevicesData, previousData) || stats.DevicesSummary != 7 {
			t.Errorf("failed computation replaced the prior snapshot: %+v, summary %d", stats.DevicesData, stats.DevicesSummary)
		}
	})
}
