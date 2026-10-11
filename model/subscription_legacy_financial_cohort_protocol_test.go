// Real PostgreSQL framing counts the round trips of owned financial transactions.
package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Counts one owned financial transaction's statements and protocol sends. A
// completed grant lock query that returned rows gives a lower bound on grant
// residence; transaction start through commit gives an upper bound. Neither is
// called server lock time.
type legacyCohortProtocolWork struct {
	grantReady time.Time
	statements int
	dispatches int
}

// One statement or batch is one send; a batch counts each queued statement.
func (self *legacyCohortProtocolWork) hook() *legacyStatementHook {
	return &legacyStatementHook{
		before: func(_ context.Context, _ string, queued []string) {
			self.dispatches++
			self.statements += max(1, len(queued))
		},
		completed: func(sql string, tag pgconn.CommandTag) {
			if self.grantReady.IsZero() && tag.RowsAffected() > 0 && strings.Contains(sql, "FROM transfer_balance") && strings.Contains(sql, "FOR UPDATE") {
				self.grantReady = time.Now()
			}
		},
	}
}

// One profile's protocol inventory over all of its owned financial transactions.
// Only counts and durations are kept, never frame data, bind values,
// credentials or server messages.
type legacyCohortProtocolSample struct {
	Profile          string `json:"profile"`
	DelayNs          int64  `json:"delay_ns_per_ready_for_query"`
	Contracts        int    `json:"contracts"`
	Transactions     int    `json:"transactions"`
	Begins           int64  `json:"begin_commands"`
	Commits          int64  `json:"commit_commands"`
	Ready            int64  `json:"backend_ready_for_query"`
	MaintenanceReady int64  `json:"maintenance_ready_for_query"`
	SentBytes        int64  `json:"frontend_bytes"`
	ReceivedBytes    int64  `json:"backend_bytes"`
	WallNs           int64  `json:"financial_wall_ns"`
	MaxTransactionNs int64  `json:"max_transaction_wall_ns"`
	MaxGrantLowerNs  int64  `json:"max_grant_residence_lower_ns"`
	SqlStatements    int    `json:"observed_statements"`
	Dispatches       int    `json:"observed_dispatches"`
}

// Settles the fixture through one profile's real owned financial transactions
// while the private proxy observes both database routes. The proxy starts on
// fresh pools, so each connection begins cold and is then reused. One warm-up
// exchange completes authentication before counting. Posts run outside the
// observed windows: they are not the financial owner's work.
func legacyCohortProtocolRun(t testing.TB, ctx context.Context, f legacyFinancialCohortFixture, profile string, delay time.Duration) legacyCohortProtocolSample {
	t.Helper()
	protocol, closeProtocol := legacyFinancialRunProtocolBind(t, ctx)
	defer closeProtocol()
	stopHooks := startLegacyStatementHooks(t, ctx)
	defer stopHooks()
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `SELECT 1`))
	})
	for _, proxy := range protocol.proxies {
		proxy.delayNs.Store(int64(delay))
	}
	measured, finish := withLegacySettlementPostBatch(ctx)
	result := legacyCohortProtocolSample{Profile: profile, DelayNs: int64(delay), Contracts: len(f.ids)}
	stride := 1
	if profile == "cohort" {
		stride = 8
	}
	for offset := 0; offset < len(f.ids); offset += stride {
		work := &legacyCohortProtocolWork{}
		var posts []func() any
		protocol.enabled(true)
		started := time.Now()
		server.Tx(measured, func(tx server.PgTx) {
			hooked, unhook := withLegacyStatementHooks(measured, tx, work.hook())
			defer unhook()
			if profile == "individual" {
				var completed, busy bool
				var err error
				posts, completed, busy, _, err = flushLegacySettlementInTx(hooked, tx, f.ids[offset])
				if err != nil || !completed || busy {
					t.Fatal("individual financial control failed", err)
				}
			} else {
				attempts, cohortPosts, err := flushLegacySettlementCohortInTx(hooked, tx, f.ids[offset:offset+stride])
				if err != nil || len(attempts) != stride {
					t.Fatal("cohort financial control failed", err)
				}
				for _, attempt := range attempts {
					if !attempt.completed || attempt.busy || attempt.fallback {
						t.Fatal("healthy protocol cohort fell back", attempt)
					}
				}
				posts = cohortPosts
			}
			if !work.grantReady.IsZero() {
				// Stop before commit dispatch: its reply includes time after
				// PostgreSQL released the grant and cannot be a lower bound.
				result.MaxGrantLowerNs = max(result.MaxGrantLowerNs, time.Since(work.grantReady).Nanoseconds())
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		elapsed := time.Since(started)
		protocol.enabled(false)
		result.Transactions++
		result.WallNs += elapsed.Nanoseconds()
		result.MaxTransactionNs = max(result.MaxTransactionNs, elapsed.Nanoseconds())
		result.SqlStatements += work.statements
		result.Dispatches += work.dispatches
		server.RunPosts(measured, posts...)
	}
	finish()
	wire := protocol.snapshot()
	ordinary, maintenance := wire[server.DefaultPgVaultResourceName], wire[server.MaintenancePgVaultResourceName]
	result.Begins = ordinary["begin_commands_observed"]
	result.Commits = ordinary["commit_commands_observed"]
	result.Ready = ordinary["ready_replies_observed"]
	result.MaintenanceReady = maintenance["ready_replies_observed"]
	result.SentBytes = ordinary["frontend_bytes_read"]
	result.ReceivedBytes = ordinary["complete_backend_frame_bytes_read"]
	return result
}

// The financial owner settles 32 contracts one at a time, then eight at a time,
// each through the real owned server transaction. The modeled 0/1ms cases use
// the same real statements and fixed funds. Returned posts run outside the
// observed windows. Full public-page tests separately cover those owners and
// total throughput.
func TestLegacyFinancialCohortRealProtocolWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		for _, delay := range []time.Duration{0, time.Millisecond} {
			var prior legacyCohortProtocolSample
			for _, profile := range []string{"individual", "cohort"} {
				f := legacyFinancialCohortSeed(t, ctx, 32)
				result := legacyCohortProtocolRun(t, ctx, f, profile, delay)
				legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
				raw, _ := json.Marshal(result)
				t.Logf("legacy_financial_protocol=%s", raw)
				if result.Begins != int64(result.Transactions) || result.Commits != int64(result.Transactions) {
					t.Fatal("protocol windows differ from the owned financial transactions", result)
				}
				if profile == "individual" {
					prior = result
				} else if result.Ready*4 >= prior.Ready {
					t.Fatalf("cohort failed to amortize actual PostgreSQL synchronization: individual=%d cohort=%d", prior.Ready, result.Ready)
				}
			}
		}
	})
}
