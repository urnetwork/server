// Real financial statements retain grant ownership across client round trips.
package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Charge one fixed client round trip before each statement after the real grant
// acquisition. Barriers establish contention; wall time is diagnostic only.
// This models latency outside PostgreSQL without substituting any financial SQL.
type legacyFinancialLatency struct {
	owner                  *legacyGrantOwnerDiagnostic
	roundTrip              time.Duration
	ownedRoundTrips        int
	clockReads             int
	freshGrantReads        int
	cacheWrites            int
	mirrorOwnerWrites      int
	reportReads            int
	participantHeaderReads int
}

// One statement or batch after the grant is one modeled round trip; a batch
// still counts the SQL work of each queued statement.
func (self *legacyFinancialLatency) hook() *legacyStatementHook {
	return &legacyStatementHook{before: func(ctx context.Context, sql string, queued []string) {
		if self.owner.acquiredAt.IsZero() {
			return
		}
		self.ownedRoundTrips++
		if queued == nil {
			self.recordStatement(sql)
		}
		for _, query := range queued {
			self.recordStatement(query)
		}
		if self.roundTrip > 0 {
			select {
			case <-time.After(self.roundTrip):
			case <-ctx.Done():
				server.Raise(ctx.Err())
			}
		}
	}}
}

// Count SQL work separately from protocol sends, including queued statements.
func (self *legacyFinancialLatency) recordStatement(sql string) {
	if strings.Contains(sql, "FROM contract_close") {
		self.reportReads++
	}
	if strings.HasPrefix(strings.TrimSpace(sql), "SELECT") && strings.Contains(sql, "FROM transfer_contract") && strings.Contains(sql, "source_network_id") {
		self.participantHeaderReads++
	}
	if strings.TrimSpace(sql) == `SELECT clock_timestamp() AT TIME ZONE 'UTC'` {
		self.clockReads++
	}
	if strings.Contains(sql, "FOR UPDATE OF transfer_balance") {
		self.freshGrantReads++
	}
	if sql == netEscrowPublishAdmissionCacheSQL {
		self.cacheWrites++
	}
	if strings.Contains(sql, "INSERT INTO pending_task") && strings.Contains(sql, "DO UPDATE") {
		self.mirrorOwnerWrites++
	}
}

// A real owner makes a full 64-visit page refuse its shared grant. Another payer
// completes at the same barrier. After release, the unchanged head cursor must
// revisit the old prefix and drain 129 due keys with exact accounting. A 1ms
// per-call latency exposes the repeated SQL work without a timing pass/fail gate.
func TestLegacySettlementFinancialLatencyDenseSharedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const count = 129
		ids := make([]server.Id, count)
		for index := range ids {
			ids[index] = server.NewId()
			ids[index][15] = 1
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
                (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
                SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS id`,
				ids, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
                SELECT id,$2,2 FROM unnest($1::uuid[]) AS id`, ids, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                SELECT id,party,1,now() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS id
                CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
                SELECT id,1,'settled',false,timestamp '2010-01-01'+position*interval '1 second'
                FROM unnest($1::uuid[]) WITH ORDINALITY AS selected(id,position)`, ids))
		})
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		before := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
		if before.reserved != 2*count {
			t.Fatal("warm synthetic reservation is incomplete", before)
		}
		independent, independentId := legacySettlementTestIntent(t, ctx)
		stopHooks := startLegacyStatementHooks(t, ctx)
		defer stopHooks()
		var busyPage LegacySettlementFlushResult
		var traces []*legacyFinancialLatency
		var ownerResidence time.Duration
		first := true
		settle := func(ctx context.Context, id server.Id, wait *legacySettlementGrantWait) (completed, busy bool, gate legacySettlementBusyGate, returnErr error) {
			owner := &legacyGrantOwnerDiagnostic{afterGrant: func() {}}
			if first {
				first = false
				owner.afterGrant = func() {
					var err error
					busyPage, err = FlushLegacySettlements(ctx, 1, nil, 64)
					// The owner admitted the grant's financial ownership keys before
					// its grant lock, so each cohort of the page yields at admission,
					// including the one holding the owner's own intent.
					if err != nil || busyPage.Visited != 64 || busyPage.Completed != 0 || busyPage.Failed != 0 ||
						busyPage.BusyAdmissionDeferred != 64 || busyPage.BusyOrGone != 64 || busyPage.Cursor == nil {
						t.Fatalf("owned grant did not refuse the exact dense prefix: %+v err=%v", busyPage, err)
					}
					complete, blocked, _, err := flushLegacySettlement(ctx, independentId)
					if err != nil || !complete || blocked {
						t.Fatal("independent payer was blocked by the dense grant", complete, blocked, err)
					}
				}
			}
			latency := &legacyFinancialLatency{owner: owner, roundTrip: time.Millisecond}
			server.HandleError(func() {
				var posts []func() any
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
					var err error
					hooked, unhook := withLegacyStatementHooks(ctx, tx, latency.hook(), owner.hook())
					defer unhook()
					posts, completed, busy, gate, err = flushLegacySettlementWithGrantWaitInTx(hooked, tx, id, wait)
					server.Raise(err)
				}, server.TxReadCommitted, server.OptNoRetry())
				if !owner.acquiredAt.IsZero() {
					ownerResidence += time.Since(owner.acquiredAt) - owner.barrierTime
				}
				traces = append(traces, latency)
				server.RunPosts(ctx, posts...)
			}, func(err error) { returnErr = err })
			return
		}
		complete, busy, _, err := settle(ctx, ids[0], nil)
		if err != nil || !complete || busy {
			t.Fatal("financial latency owner failed", complete, busy, err)
		}
		cursor := busyPage.Cursor
		completed, pages := 1, 0
		for {
			page, err := flushLegacySettlementsPage(ctx, ctx, 1, cursor, 64, settle)
			pages++
			if err != nil || page.Failed != 0 || page.BusyOrGone != 0 || page.Completed != page.Visited {
				t.Fatalf("released dense grant did not advance: %+v err=%v", page, err)
			}
			if pages == 1 && page.HeadCompleted != 16 {
				t.Fatal("released old prefix did not receive its 16 head visits", page)
			}
			completed += page.Completed
			cursor = page.Cursor
			if cursor == nil {
				if completed == count {
					break
				}
				// The finite forward pass may end with busy keys behind its cursor.
				// The ordinary next traversal resumes them from the indexed head.
			}
			if pages >= 8 || completed > count {
				t.Fatal("released dense grant did not drain its finite cohort", completed, pages)
			}
		}
		projectLegacyProviderTotalsForTest(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
                NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
                AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$4
                AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=1)=$4
                AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000-$4
                AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$4
                AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$4
                AND (SELECT provided_byte_count FROM account_balance WHERE network_id=$3)=$4
                AND (SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3)=$4`,
				ids, f.balanceId, f.destinationNetworkId, count).Scan(&exact))
			if !exact {
				t.Fatal("dense grant changed payer debit, metadata or exact provider accounting")
			}
		})
		after := settlementCacheSnapshot(ctx, []server.Id{f.balanceId})[f.balanceId]
		if after.reserved != 0 || after.revision != before.revision+2*count {
			t.Fatal("dense warm cache lost the two exact transitions", before, after)
		}
		requireLegacyOwnedMetadataRedis(t, ctx, f.balanceId, 0)
		requireLegacySettlementTestState(t, ctx, independent, independentId, false, true, 989, 0)
		if replay, err := FlushLegacySettlements(ctx, 1, nil, 64); err != nil || replay.Visited != 0 {
			t.Fatal("drained dense grant was not a no-op on replay", replay, err)
		}
		var calls, clockReads, grantReads, cacheWrites, mirrorWrites, reportReads, participantHeaderReads int
		for _, trace := range traces {
			calls += trace.ownedRoundTrips
			clockReads += trace.clockReads
			grantReads += trace.freshGrantReads
			cacheWrites += trace.cacheWrites
			mirrorWrites += trace.mirrorOwnerWrites
			reportReads += trace.reportReads
			participantHeaderReads += trace.participantHeaderReads
		}
		t.Logf("dense warm grant: completed=%d recovery_pages=%d blocked_prefix=64 owned_round_trips=%d injected_latency=%s client_grant_residence=%s first_owner_calls=%d first_owner_clock_reads=%d fresh_grant_reads=%d cache_writes=%d mirror_owner_writes=%d; local latency model, not Main attribution", completed, pages, calls, time.Duration(calls)*time.Millisecond, ownerResidence, traces[0].ownedRoundTrips, traces[0].clockReads, grantReads, cacheWrites, mirrorWrites)
		if grantReads != count || cacheWrites != 2*count {
			t.Fatal("financial control lost fresh grant acquisition or warm revision publications", grantReads, cacheWrites)
		}
		if clockReads != 0 {
			t.Fatalf("financial owner retained its shared grant for %d unnecessary standalone clock round trips", clockReads)
		}
		if reportReads != count || participantHeaderReads != count {
			t.Fatalf("financial owner reread locked contract facts: completed=%d reports=%d participant_headers=%d", count, reportReads, participantHeaderReads)
		}
	})
}
