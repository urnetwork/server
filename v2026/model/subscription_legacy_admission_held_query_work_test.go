// A real first page establishes each continuation cursor under a held grant.
// The next public page must retain head ownership while shedding cooperative
// forward retries, then release into exact ordinary financial completion.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

type legacyAdmissionHeldLeaseKey struct{}

// A marked acquire command first establishes this fixture owner's real lease
// in the same EVAL as the unchanged production Lua. Redis freezes expiration
// time within a script, so the whole page need not finish within the real 2s
// lease. This controls active presence; separate tests own expiry/recovery.
type legacyAdmissionHeldLeaseHook struct {
	key       string
	token     string
	stateLock sync.Mutex
	injected  int
	commands  map[string]int
}

func (self *legacyAdmissionHeldLeaseHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *legacyAdmissionHeldLeaseHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (self *legacyAdmissionHeldLeaseHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if ctx.Value(legacyAdmissionHeldLeaseKey{}) != self {
			return next(ctx, command)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.commands[command.Name()]++
		}()
		args := command.Args()
		// The release EVAL has only one argument and is never rewritten.
		if command.Name() == "eval" && len(args) == 6 && args[3] == self.key {
			script, ok := args[1].(string)
			if !ok || fmt.Sprint(args[5]) != "2000" {
				return fmt.Errorf("fixture requires the actual two-second acquisition lease")
			}
			args[1] = "redis.call('SET', KEYS[1], '" + self.token + "', 'PX', ARGV[2])\n" + script
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.injected++
			}()
		}
		return next(ctx, command)
	}
}

func (self *legacyAdmissionHeldLeaseHook) snapshot() (int, map[string]int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	commands := map[string]int{}
	for name, count := range self.commands {
		commands[name] = count
	}
	return self.injected, commands
}

// One physical owner supplies both the real grant refusal and fixture lease.
func TestLegacySettlementAdmissionContinuedHeld64QueryWork(t *testing.T) {
	testLegacySettlementAdmissionContinuedHeld64QueryWork(t, 1)
}

// Distinct shard pages share only the grant, avoiding incidental intent races.
func TestLegacySettlementAdmissionContinuedHeld64ConcurrentQueryWork(t *testing.T) {
	testLegacySettlementAdmissionContinuedHeld64QueryWork(t, 4)
}

// All rows, funding and reports are fixed before acquiring the physical owner.
func testLegacySettlementAdmissionContinuedHeld64QueryWork(t *testing.T, clientCount int) {
	if os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("isolated statement statistics are required for cooperative query-work control")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
		})
		f := newNetEscrowOrderingTestFixture(t, ctx)
		const pageLimit = 64
		const headsPerPage = pageLimit / 4
		const forwardsPerPage = pageLimit - headsPerPage
		const cohortPerShard = pageLimit + forwardsPerPage
		count := cohortPerShard * clientCount
		contractIds := make([]server.Id, count)
		due := make([]time.Time, count)
		shards := make([]int, count)
		prefix := server.NewId()
		for index := range contractIds {
			contractIds[index] = prefix
			binary.BigEndian.PutUint32(contractIds[index][11:15], uint32(index+1))
			shards[index] = index/cohortPerShard + 1
			contractIds[index][15] = byte(shards[index])
			due[index] = time.Date(2010, time.January, 1, 0, 0, index, 0, time.UTC)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
				SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS seed(id)`,
				contractIds, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT id,$2,2 FROM unnest($1::uuid[]) AS seed(id)`, contractIds, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS seed(id)
				CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, contractIds))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
				SELECT id,shard,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[],$3::int[]) AS seed(id,due,shard)`, contractIds, due, shards))
		})
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		readReports := func() []byte {
			var reports []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(contract_id,party,used_transfer_byte_count,checkpoint,close_time)
					ORDER BY contract_id,party) FROM contract_close WHERE contract_id=ANY($1)`, contractIds).Scan(&reports))
			})
			return reports
		}
		originalReports := readReports()
		providerTarget := task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()
		const mirrorTarget = "github.com/urnetwork/server/v2026/model.ApplyLegacyNetEscrowMirror"
		requireState := func(settled bool) {
			t.Helper()
			wantCompleted, wantCredit, wantReserved, wantPaid, wantMirrors := 0, int64(1000), ByteCount(2*count), int64(0), 0
			if settled {
				wantCompleted, wantCredit, wantReserved, wantPaid, wantMirrors = count, int64(1000-count), 0, int64(count), 1
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
					SELECT allocation FROM pending_task
					CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
					WHERE function_name=$8 AND (args_json::jsonb->>'applied')::boolean=false
					AND (allocation->>'network_id')::uuid=$3
				) SELECT
					(SELECT balance_byte_count=$4 AND start_balance_byte_count=1000 FROM transfer_balance WHERE balance_id=$2)
					AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=$5
					AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1))=$6
					AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled)=$5
					AND (SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$5
					AND (SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$7
					AND (SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=$7
					AND (SELECT count(*) FROM pending_task WHERE function_name=$9)=$10
					AND (SELECT count(*) FROM pending_task WHERE function_name=$8)=$5
					AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
					 +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=$7
					AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
					 +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=$7`,
					contractIds, f.balanceId, f.destinationNetworkId, wantCredit, wantCompleted, count-wantCompleted,
					wantPaid, providerTarget, mirrorTarget, wantMirrors).Scan(&exact))
				if !exact {
					t.Fatal("cooperative admission changed exact financial ownership", settled)
				}
			})
			if Testing_NetEscrowByteCount(ctx, f.balanceId) != wantReserved || !bytes.Equal(originalReports, readReports()) {
				t.Fatal("cooperative admission changed reservations or original reports", settled)
			}
		}
		requireState(false)
		ownerId := server.NewId()
		hook := &legacyAdmissionHeldLeaseHook{
			key:   "legacy_settlement_owner:{" + f.balanceId.String() + "}:v1",
			token: "v1:" + hex.EncodeToString(ownerId[:]), commands: map[string]int{},
		}
		server.Raise(server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
			r.AddHook(hook)
			return nil
		}))
		measured := context.WithValue(ctx, legacyAdmissionHeldLeaseKey{}, hook)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.balanceId))
		type pageResult struct {
			page LegacySettlementFlushResult
			err  error
		}
		// Every observation joins all workers before reading statement counters.
		runHeldPages := func(cursors []*LegacySettlementCursor) ([]pageResult, time.Duration, *legacyTargetSqlWorkDelta) {
			before := legacyTargetSqlSnapshot(t, ctx)
			began := time.Now()
			start := make(chan struct{})
			results := make([]pageResult, clientCount)
			var workers sync.WaitGroup
			for clientIndex := range clientCount {
				workers.Add(1)
				go func() {
					defer workers.Done()
					<-start
					page, err := FlushLegacySettlements(measured, clientIndex+1, cursors[clientIndex], pageLimit)
					results[clientIndex] = pageResult{page: page, err: err}
				}()
			}
			close(start)
			workers.Wait()
			wall := time.Since(began)
			after := legacyTargetSqlSnapshot(t, ctx)
			return results, wall, legacyTargetSqlDelta(t, before, after)
		}
		// Decode optional counters without a new production symbol so this
		// identical control also builds on the unmodified V14/V15 baseline.
		admissionCounts := func(page LegacySettlementFlushResult) (int, int) {
			var admission struct {
				Deferred     int `json:"busy_admission_deferred"`
				HeadDeferred int `json:"head_busy_admission_deferred"`
			}
			raw, err := json.Marshal(page)
			server.Raise(err)
			server.Raise(json.Unmarshal(raw, &admission))
			return admission.Deferred, admission.HeadDeferred
		}
		cursors := make([]*LegacySettlementCursor, clientCount)
		initialResults, initialWall, initialSql := runHeldPages(cursors)
		initialInjected, initialCommands := hook.snapshot()
		initialPhases := []*LegacySettlementTimings{}
		for clientIndex, result := range initialResults {
			page := result.page
			deferred, headDeferred := admissionCounts(page)
			index := clientIndex*cohortPerShard + pageLimit - 1
			if result.err != nil || page.Visited != pageLimit || page.BusyOrGone != pageLimit || page.BusyGrantSetMismatch != pageLimit || page.Completed != 0 || page.Failed != 0 || page.BusyIntentUnavailable+page.BusyContractUnavailable != 0 || page.HeadVisited != 0 || page.HeadGrantWaitAttempted != 0 || deferred != 0 || headDeferred != 0 || !page.More || page.Cursor == nil || page.Cursor.ContractId != contractIds[index] || !page.Cursor.NextAttemptTime.Equal(due[index]) || page.Cursor.PassEndTime.IsZero() || page.Cursor.HeadAfter != nil {
				t.Fatalf("real initial page failed to establish unchanged PG traversal: visited=%d grant_busy=%d admission=%d heads=%d error=%v", page.Visited, page.BusyGrantSetMismatch, deferred, page.HeadVisited, result.err)
			}
			encoded, err := json.Marshal(page.Cursor)
			server.Raise(err)
			var cursor LegacySettlementCursor
			server.Raise(json.Unmarshal(encoded, &cursor))
			cursors[clientIndex] = &cursor
			initialPhases = append(initialPhases, page.Timings)
		}
		// The real cursor leaves 48 forward rows plus at least 16 older heads.
		// No cursor, due key, financial report or owner is rewritten between pages.
		results, busyWall, busySql := runHeldPages(cursors)
		visits, busyGrant, busyAdmission, headVisits, headWaits, headTimeouts := 0, 0, 0, 0, 0, 0
		phases := []*LegacySettlementTimings{}
		for clientIndex, result := range results {
			page := result.page
			deferred, headDeferred := admissionCounts(page)
			lastForward := (clientIndex+1)*cohortPerShard - 1
			lastHead := clientIndex*cohortPerShard + headsPerPage - 1
			if result.err != nil || page.Visited != pageLimit || page.BusyOrGone != pageLimit || page.Completed != 0 || page.Failed != 0 || page.BusyIntentUnavailable+page.BusyContractUnavailable != 0 || page.HeadVisited != headsPerPage || page.HeadBusyOrGone != headsPerPage || page.HeadBusyGrantSetMismatch != headsPerPage || page.HeadCompleted != 0 || page.HeadFailed != 0 || page.HeadGrantWaitAttempted != 1 || page.HeadGrantWaitTimedOut != 1 || page.HeadGrantWaitCompleted != 0 || headDeferred != 0 || !page.More || page.Cursor == nil || page.Cursor.ContractId != contractIds[lastForward] || !page.Cursor.NextAttemptTime.Equal(due[lastForward]) || !page.Cursor.PassEndTime.Equal(cursors[clientIndex].PassEndTime) || page.Cursor.HeadAfter == nil || page.Cursor.HeadAfter.ContractId != contractIds[lastHead] || !page.Cursor.HeadAfter.NextAttemptTime.Equal(due[lastHead]) {
				t.Fatalf("real continued page failed its forward/head ownership boundary: visited=%d busy=%d heads=%d head_waits=%d head_timeouts=%d error=%v", page.Visited, page.BusyOrGone, page.HeadVisited, page.HeadGrantWaitAttempted, page.HeadGrantWaitTimedOut, result.err)
			}
			visits += page.Visited
			busyGrant += page.BusyGrantSetMismatch
			busyAdmission += deferred
			headVisits += page.HeadVisited
			headWaits += page.HeadGrantWaitAttempted
			headTimeouts += page.HeadGrantWaitTimedOut
			phases = append(phases, page.Timings)
		}
		requireState(false)
		// Release only the owner's PG lock and token-checked Redis hint. No
		// credit, escrow, report, intent or due timestamp is changed here.
		server.Raise(held.Rollback(ctx))
		server.Raise(server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
			return r.Eval(ctx, `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{hook.key}, hook.token).Err()
		}))
		beforeReleased := legacyTargetSqlSnapshot(t, ctx)
		began := time.Now()
		releasedVisits, releasedCompleted := 0, 0
		releasedPhases := []*LegacySettlementTimings{}
		for clientIndex := range clientCount {
			released, err := FlushLegacySettlements(ctx, clientIndex+1, nil, cohortPerShard)
			if err != nil || released.Visited != cohortPerShard || released.Completed != cohortPerShard || released.BusyOrGone != 0 || released.Failed != 0 || released.HeadVisited != 0 || released.HeadGrantWaitAttempted != 0 {
				t.Fatalf("owner release alone failed to settle its entire unchanged cohort: visited=%d completed=%d busy=%d failed=%d error=%v", released.Visited, released.Completed, released.BusyOrGone, released.Failed, err)
			}
			releasedVisits += released.Visited
			releasedCompleted += released.Completed
			releasedPhases = append(releasedPhases, released.Timings)
		}
		releasedWall := time.Since(began)
		afterReleased := legacyTargetSqlSnapshot(t, ctx)
		requireState(true)
		for clientIndex := range clientCount {
			if replay, err := FlushLegacySettlements(ctx, clientIndex+1, nil, pageLimit); err != nil || replay.Visited != 0 {
				t.Fatal("empty public replay repeated its financial transition", replay.Visited, err)
			}
		}
		requireState(true)
		releasedSql := legacyTargetSqlDelta(t, beforeReleased, afterReleased)
		for _, family := range []string{"intent_ownership", "contract_ownership", "grant_membership", "grant_ownership", "outcome_write", "grant_debit"} {
			legacyTargetRequireSqlFamily(t, releasedSql, family, float64(count), float64(count))
		}
		membershipWork := func(delta *legacyTargetSqlWorkDelta) (calls, rows float64) {
			for _, statement := range delta.Statements {
				query := strings.ToLower(strings.Join(strings.Fields(statement.Query), ""))
				if statement.TopLevel && strings.HasPrefix(query, "selectbalance_idfromtransfer_escrowwherecontract_id=$1orderbybalance_idlimit") {
					calls += statement.Metrics["calls"]
					rows += statement.Metrics["rows"]
				}
			}
			return
		}
		initialMembershipCalls, initialMembershipRows := membershipWork(initialSql)
		membershipCalls, membershipRows := membershipWork(busySql)
		attempts := pageLimit * clientCount
		forwardAttempts := forwardsPerPage * clientCount
		injected, commands := hook.snapshot()
		initialProductionCalls := initialSql.TopLevelTotals["calls"] - initialSql.Families["measurement_probe"]["calls"]
		productionCalls := busySql.TopLevelTotals["calls"] - busySql.Families["measurement_probe"]["calls"]
		var ownershipCalls float64
		for _, family := range []string{"intent_ownership", "contract_ownership", "grant_membership", "grant_ownership", "outcome_write", "grant_debit"} {
			ownershipCalls += busySql.Families[family]["calls"]
		}
		// The first forward probe and every head retain PG ownership. The
		// one timed-out head grant query is not a completed PGSS statement.
		probeFamiliesExact := true
		for _, family := range []string{"intent_ownership", "contract_ownership", "grant_membership", "grant_ownership", "outcome_write", "grant_debit"} {
			wantInitialCalls, wantInitialRows := float64(attempts), float64(attempts)
			wantCalls, wantRows := float64((headsPerPage+1)*clientCount), float64((headsPerPage+1)*clientCount)
			if family == "grant_ownership" {
				wantInitialRows = 0
				wantCalls -= float64(clientCount)
				wantRows = 0
			} else if family == "outcome_write" || family == "grant_debit" {
				wantInitialCalls, wantInitialRows = 0, 0
				wantCalls, wantRows = 0, 0
			}
			legacyTargetRequireSqlFamily(t, initialSql, family, wantInitialCalls, wantInitialRows)
			actual := busySql.Families[family]
			probeFamiliesExact = probeFamiliesExact && actual["calls"] == wantCalls && actual["rows"] == wantRows
		}
		if initialInjected != 0 || len(initialCommands) != 0 || initialMembershipCalls != 0 || initialMembershipRows != 0 || initialProductionCalls != float64(9*attempts) {
			t.Fatalf("nil-input page performed optional admission work: injected=%d commands=%v membership_calls=%v membership_rows=%v production_calls=%v; want %d ordinary PG statements", initialInjected, initialCommands, initialMembershipCalls, initialMembershipRows, initialProductionCalls, 9*attempts)
		}
		wantAdmission := forwardAttempts - clientCount
		wantGrantBusy := (headsPerPage + 1) * clientCount
		wantOwnershipCalls := (4*(headsPerPage+1) - 1) * clientCount
		wantProductionCalls := (2*forwardsPerPage + 8 + 9*headsPerPage) * clientCount
		metrics := map[string]any{
			"kind": "legacy_cooperative_continued_held_query_work_v3", "source_profile": os.Getenv("URN_LEGACY_SOURCE_PROFILE"), "cohort": count, "cohort_per_shard": cohortPerShard, "page_limit": pageLimit, "concurrent_clients": clientCount,
			"initial":      map[string]any{"visited": attempts, "busy_grant": attempts, "busy_admission": 0, "fixture_lease_injections": initialInjected, "deadline_redis_commands": initialCommands, "production_top_level_sql_calls": initialProductionCalls, "membership_calls": initialMembershipCalls, "membership_rows": initialMembershipRows, "real_public_cursors": clientCount, "wall_ns": initialWall.Nanoseconds(), "phases": initialPhases, "sql": initialSql},
			"held":         map[string]any{"visited": visits, "forward_visited": visits - headVisits, "head_visited": headVisits, "head_wait_attempted": headWaits, "head_wait_timed_out": headTimeouts, "busy_grant": busyGrant, "busy_admission": busyAdmission, "fixture_lease_injections": injected, "deadline_redis_commands": commands, "production_top_level_sql_calls": productionCalls, "membership_calls": membershipCalls, "membership_rows": membershipRows, "ownership_calls": ownershipCalls, "mandatory_probe_families_exact": probeFamiliesExact, "wall_ns": busyWall.Nanoseconds(), "phases": phases, "sql": busySql},
			"released":     map[string]any{"visited": releasedVisits, "completed": releasedCompleted, "wall_ns": releasedWall.Nanoseconds(), "phases": releasedPhases, "sql": releasedSql},
			"conservation": map[string]any{"payer_debit": count, "provider_bytes": count, "provider_revenue_nano_cents": count, "remaining_credit": 1000 - count, "remaining_escrow": 0, "reports_unchanged": true, "replay_unchanged": true},
			"qualifiers": []string{
				"A real grant-row transaction supplies the physical owner. Fixture Lua supplies its real active lease at each candidate acquisition, without changing the original production Lua or its 2000ms argument.",
				"Controlled active lease presence is not evidence of production lease duration, renewal, crash recovery or fairness.",
				"Each client owns 112 distinct rows in a separate shard; the only shared financial owner is the same grant. This avoids incidental intent-row contention.",
				"A real initial nil-cursor page visits 64 held rows. Its unchanged returned cursor drives the measured page with 48 forward and 16 head visits; no synthetic cursor enables admission.",
				"Every head bypasses hints, including one genuine bounded grant timeout. PGSS omits that failed grant query; its successful timeout SET and rollback remain counted.",
				"The first forward observed stable escrow set still reaches the actual PG grant gate. Its key is pre-transaction membership, not proof that a later PG snapshot has identical membership.",
				"Only PG lock and token-checked hint ownership are released before ordinary public pages settle all 112 or 448 unchanged rows; no funds or reports are repaired.",
				"SQL counts are completed statements, not network round trips; nested execution and phase families overlap. Financial timing includes optional hint lookup, acquisition and release.",
				"Fixture SET inside EVAL is additional test work; its throughput cannot be called production Redis cost.",
				"No sleep or wall-time ratio determines correctness, and no Main performance claim is made.",
			},
		}
		raw, err := json.Marshal(metrics)
		server.Raise(err)
		t.Logf("legacy_cooperative_held_query_work_metrics=%s", raw)
		// All accounting/replay controls precede this sole expected baseline
		// failure. Baseline performs its real PG busy path; candidate sheds it.
		if visits != attempts || busyAdmission != wantAdmission || busyGrant != wantGrantBusy || injected != forwardAttempts || membershipCalls != float64(forwardAttempts) || membershipRows != float64(forwardAttempts) || ownershipCalls != float64(wantOwnershipCalls) || !probeFamiliesExact || productionCalls != float64(wantProductionCalls) {
			t.Fatalf("continued cooperative admission SQL reduction missing: attempts=%d admission=%d grant_busy=%d injected=%d membership_calls=%v membership_rows=%v ownership_calls=%v probe_families_exact=%v production_calls=%v; want admission=%d grant_busy=%d ownership_calls=%d production_calls=%d", attempts, busyAdmission, busyGrant, injected, membershipCalls, membershipRows, ownershipCalls, probeFamiliesExact, productionCalls, wantAdmission, wantGrantBusy, wantOwnershipCalls, wantProductionCalls)
		}
	})
}
