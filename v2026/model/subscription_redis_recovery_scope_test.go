package model

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

// The only default PG connection stays borrowed throughout every optional
// page. Empty Redis pages must finish before their deadlines without waiting
// for that slot, including the first use of the dedicated Redis pool.
func TestRedisRecoveryEmptyColdPagesDoNotAcquirePostgres(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		pop := server.Config.PushSimpleResource(server.DefaultPgConfigResourceName, []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { server.PgReset(); pop() }()
		server.RedisReset()
		defer server.RedisReset()
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		server.Db(ctx, func(server.PgConn) {
			const concurrent = 16
			pendingBefore := testutil.ToFloat64(redisContractReservationResults.WithLabelValues("recovery", "pending"))
			var joined sync.WaitGroup
			joined.Add(concurrent)
			failures := make(chan string, concurrent)
			for range concurrent {
				go func() {
					defer joined.Done()
					page, stop := context.WithTimeout(ctx, 2*time.Second)
					defer stop()
					owner := &redisContractAdmission{contractId: server.NewId(), attemptedBalanceIds: []server.Id{server.NewId()}}
					if owner.recoverRetainedPage(page) {
						failures <- "empty page invented recovery progress"
					} else if page.Err() != nil {
						failures <- "empty Redis page waited for the held PostgreSQL slot"
					}
				}()
			}
			joined.Wait()
			close(failures)
			for failure := range failures {
				t.Error(failure)
			}
			if testutil.ToFloat64(redisContractReservationResults.WithLabelValues("recovery", "pending")) != pendingBefore {
				t.Error("empty pages returned because recovery failed rather than observing an empty Redis page")
			}
		})
	})
}
