package model

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type legacyTargetLoadFixture struct {
	payers          []netEscrowOrderingTestFixture
	providers       []netEscrowOrderingTestFixture
	ids             []server.Id
	targets         map[server.Id]int
	grantCounts     []int
	providerCounts  []int
	legacyNeighbors []server.Id
	redisNeighbors  []server.Id
	redisExpiry     []float64
}

// The same seed shape is used for each source profile: 1,024 contracts, 16 shards,
// two grants shared 15:1, four providers, and one hot/one independent target per
// shard. Setup is bulk synthetic; all measured financial writes are ordinary.
func legacyTargetSeedLoad(t testing.TB, ctx context.Context) legacyTargetLoadFixture {
	t.Helper()
	f := legacyTargetLoadFixture{targets: map[server.Id]int{}, grantCounts: make([]int, 2), providerCounts: make([]int, 4)}
	for range 2 {
		f.payers = append(f.payers, newNetEscrowOrderingTestFixture(t, ctx))
	}
	for range 4 {
		f.providers = append(f.providers, newNetEscrowOrderingTestFixture(t, ctx))
	}
	prefix := server.NewId()
	groups := [2][4][]server.Id{}
	shards := [2][4][]int{}
	due := [2][4][]time.Time{}
	oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	for shard := range 16 {
		for position := range 64 {
			id := prefix
			binary.BigEndian.PutUint32(id[11:15], uint32(1+64*shard+position))
			id[15] = byte(shard)
			grant := 0
			if position%16 == 15 {
				grant = 1
			}
			provider := position % 4
			f.ids = append(f.ids, id)
			groups[grant][provider] = append(groups[grant][provider], id)
			shards[grant][provider] = append(shards[grant][provider], shard)
			due[grant][provider] = append(due[grant][provider], oldest.Add(time.Duration(position)*time.Second))
			f.grantCounts[grant]++
			f.providerCounts[provider]++
			if position == 0 || position == 63 {
				f.targets[id] = 2*shard + grant
			}
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for grant, payer := range f.payers {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET start_balance_byte_count=4000,balance_byte_count=4000,net_revenue_nano_cents=8000 WHERE balance_id=$1`, payer.balanceId))
			for provider, destination := range f.providers {
				ids := groups[grant][provider]
				if len(ids) == 0 {
					continue
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
					SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS row(id)`, ids, payer.sourceNetworkId, payer.sourceId, destination.destinationNetworkId, destination.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
					SELECT id,$2,2 FROM unnest($1::uuid[]) AS row(id)`, ids, payer.balanceId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
					SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS row(id)
					CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
					SELECT id,shard,'settled',false,due FROM unnest($1::uuid[],$2::smallint[],$3::timestamp[]) AS row(id,shard,due)`, ids, shards[grant][provider], due[grant][provider]))
			}
		}
	})
	for _, payer := range f.payers {
		legacy, posts := createNetEscrowOrderingTestContract(ctx, payer, 23)
		server.RunPosts(ctx, posts...)
		f.legacyNeighbors = append(f.legacyNeighbors, legacy.ContractId)
		redis := createRedisAdmissionTest(ctx, payer, 31)
		f.redisNeighbors = append(f.redisNeighbors, redis.ContractId)
		server.Redis(ctx, func(r server.RedisClient) {
			expiry, err := r.ZScore(ctx, redisContractReservationKeys(payer.balanceId)[2], redis.ContractId.String()).Result()
			server.Raise(err)
			f.redisExpiry = append(f.redisExpiry, expiry)
		})
		refreshNetEscrow(ctx, []server.Id{payer.balanceId})
	}
	if len(f.ids) != 1024 || len(f.targets) != 32 || f.grantCounts[0] != 960 || f.grantCounts[1] != 64 {
		t.Fatal("loaded target fixture distribution changed")
	}
	return f
}

type legacyTargetVisit struct {
	Ordinal      int   `json:"ordinal"`
	Attempts     int   `json:"attempts"`
	FirstVisitNs int64 `json:"first_visit_ns"`
	SettledNs    int64 `json:"settled_ns"`
}

type legacyTargetLoadObservation struct {
	mu          sync.Mutex
	started     time.Time
	targets     map[server.Id]int
	visits      map[server.Id]*legacyTargetVisit
	attempts    int
	interrupted int
}

func (self *legacyTargetLoadObservation) settle(ctx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
	self.mu.Lock()
	self.attempts++
	self.mu.Unlock()
	ordinal, target := self.targets[id]
	if target {
		self.mu.Lock()
		visit := self.visits[id]
		if visit == nil {
			visit = &legacyTargetVisit{Ordinal: ordinal, FirstVisitNs: time.Since(self.started).Nanoseconds()}
			self.visits[id] = visit
		}
		visit.Attempts++
		self.mu.Unlock()
	}
	completed, busy, gate, err := flushLegacySettlementWithGrantWait(ctx, id, wait)
	if err != nil && ctx.Err() != nil {
		self.mu.Lock()
		self.interrupted++
		self.mu.Unlock()
	}
	if target && completed && err == nil {
		self.mu.Lock()
		self.visits[id].SettledNs = time.Since(self.started).Nanoseconds()
		self.mu.Unlock()
	}
	return completed, busy, gate, err
}

// Preserve the exact public owner budget and phase observer while adding only
// a test-owned target observer around the ordinary settlement callback.
func legacyTargetObservedPage(ctx context.Context, shard int, cursor *LegacySettlementCursor, limit int, observer *legacyTargetLoadObservation) (LegacySettlementFlushResult, error) {
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errLegacySettlementPageBudget)
	defer cancel()
	timing := &legacySettlementTimingObserver{now: time.Now}
	bounded = context.WithValue(bounded, legacySettlementTimingKey{}, timing)
	result, err := flushLegacySettlementsPage(ctx, bounded, shard, cursor, limit, observer.settle)
	result.Timings = timing.snapshot()
	return result, err
}

// Performance observations are paired with target and exact conservation
// assertions. Wall times are reported, never used as pass/fail thresholds.
// Four fixture workers share sixteen shards. A round barrier ensures each
// shard receives one page before any shard receives its next page.
func TestLegacySettlementLoaded16ShardTargetsAfterCohort(t *testing.T) {
	testLegacySettlementLoaded16ShardTargets(t, "after_cohort", 4)
}

func TestLegacySettlementLoaded16ShardTargetsAfterEachRound(t *testing.T) {
	testLegacySettlementLoaded16ShardTargets(t, "after_each_round", 4)
}

func TestLegacySettlementLoaded16ShardTargetsSerial(t *testing.T) {
	testLegacySettlementLoaded16ShardTargets(t, "after_cohort", 1)
}

func testLegacySettlementLoaded16ShardTargets(t *testing.T, ownerPolicy string, workerCount int) {
	profile := os.Getenv("URN_LEGACY_SOURCE_PROFILE")
	if profile == "" {
		t.Skip("explicit frozen source profile required for the native measurement matrix")
	}
	limit := 256
	switch profile {
	case "V10":
		limit = 64
	case "V13", "V14", "candidate":
	default:
		t.Fatal("explicit frozen URN_LEGACY_SOURCE_PROFILE is required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		// DefaultTestEnv creates an isolated child database from template0.
		// The native runner owns preload; install the extension in this child
		// before setup and baseline measurement, never on Main or during a delta.
		if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
			})
		}
		fixture := legacyTargetSeedLoad(t, ctx)
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(r server.RedisClient) { r.AddHook(hook) })
		measured := hook.context(ctx)
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		observer := &legacyTargetLoadObservation{started: time.Now(), targets: fixture.targets, visits: map[server.Id]*legacyTargetVisit{}}
		var cursors [16]*LegacySettlementCursor
		var pages []LegacySettlementFlushResult
		ownerStarts, ownerFinished, peakQueued, rounds := 0, 0, 0, 0
		type pageResult struct {
			shard  int
			result LegacySettlementFlushResult
			err    error
		}
		for ; rounds < 64; rounds++ {
			jobs := make(chan int, 16)
			results := make(chan pageResult, 16)
			for shard := range 16 {
				jobs <- shard
			}
			close(jobs)
			var workers sync.WaitGroup
			for range workerCount {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for shard := range jobs {
						page, err := legacyTargetObservedPage(measured, shard, cursors[shard], limit, observer)
						results <- pageResult{shard: shard, result: page, err: err}
					}
				}()
			}
			workers.Wait()
			close(results)
			for result := range results {
				if result.err != nil || result.result.Failed != 0 {
					t.Fatalf("loaded financial page failed: shard=%d result=%+v err=%v", result.shard, result.result, result.err)
				}
				cursors[result.shard] = result.result.Cursor
				pages = append(pages, result.result)
			}
			queued := legacyTargetMirrorQueueCount(ctx)
			peakQueued = max(peakQueued, queued)
			if ownerPolicy == "after_each_round" {
				starts, finished := legacyTargetDrainMirrorOwners(t, measured, queued)
				ownerStarts += starts
				ownerFinished += finished
			}
			var remaining int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, fixture.ids).Scan(&remaining))
			})
			if remaining == 0 {
				rounds++
				break
			}
		}
		if rounds >= 64 {
			t.Fatal("bounded round-robin fixture did not settle its fixed targets/cohort")
		}
		cohortElapsed := time.Since(observer.started)
		queuedBeforeDrain := legacyTargetMirrorQueueCount(ctx)
		starts, finished := legacyTargetDrainMirrorOwners(t, measured, queuedBeforeDrain)
		ownerStarts += starts
		ownerFinished += finished
		allElapsed := time.Since(observer.started)
		afterSql := legacyTargetSqlSnapshot(t, ctx)
		commands, dispatches := hook.snapshot()
		visits, completed, busy := 0, 0, 0
		busyIntent, busyContract, busyGrant, grantWaitTimedOut := 0, 0, 0, 0
		var phaseTotals LegacySettlementTimings
		pageVisits := map[int]int{}
		for _, page := range pages {
			visits += page.Visited
			completed += page.Completed
			busy += page.BusyOrGone
			busyIntent += page.BusyIntentUnavailable
			busyContract += page.BusyContractUnavailable
			busyGrant += page.BusyGrantSetMismatch
			grantWaitTimedOut += page.HeadGrantWaitTimedOut
			pageVisits[page.Visited]++
			sources := []*LegacySettlementPhaseDuration{&page.Timings.Selection, &page.Timings.Financial, &page.Timings.JoinedPosts, &page.Timings.Mirror, &page.Timings.ColdCensus, &page.Timings.Clock, &page.Timings.Stream}
			destinations := []*LegacySettlementPhaseDuration{&phaseTotals.Selection, &phaseTotals.Financial, &phaseTotals.JoinedPosts, &phaseTotals.Mirror, &phaseTotals.ColdCensus, &phaseTotals.Clock, &phaseTotals.Stream}
			for index, source := range sources {
				destinations[index].Count += source.Count
				destinations[index].ElapsedMs += source.ElapsedMs
				destinations[index].MaxMs = max(destinations[index].MaxMs, source.MaxMs)
			}
		}
		if observer.interrupted != 0 || observer.attempts != visits {
			t.Fatalf("query-work sample is not qualified: incomplete financial callbacks=%d attempts=%d visited=%d; a production budget yield is not a query-count regression", observer.interrupted, observer.attempts, visits)
		}
		if completed != 1024 {
			t.Fatal("global completions disagree with exact cohort", completed)
		}
		orderedTargets := make([]legacyTargetVisit, 32)
		for id, ordinal := range fixture.targets {
			visit := observer.visits[id]
			if visit == nil || visit.Attempts < 1 || visit.SettledNs <= 0 {
				t.Fatal("global completion count concealed an unvisited or unsettled target", ordinal)
			}
			orderedTargets[ordinal] = *visit
		}
		legacyTargetAssertLoadConservation(t, ctx, fixture)
		for id := range fixture.targets {
			complete, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || complete || !busy {
				t.Fatal("tagged load replay reclaimed a financial owner")
			}
		}
		legacyTargetAssertLoadConservation(t, ctx, fixture)
		sqlDelta := legacyTargetSqlDelta(t, beforeSql, afterSql)
		if sqlDelta != nil {
			legacyTargetRequireSqlFamily(t, sqlDelta, "intent_ownership", float64(visits), float64(visits-busyIntent))
			legacyTargetRequireSqlFamily(t, sqlDelta, "contract_ownership", float64(visits-busyIntent), float64(visits-busyIntent-busyContract))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_membership", float64(visits-busyIntent-busyContract), float64(visits-busyIntent-busyContract))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_ownership", float64(visits-busyIntent-busyContract-grantWaitTimedOut), float64(completed))
			legacyTargetRequireSqlFamily(t, sqlDelta, "outcome_write", float64(completed), float64(completed))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_debit", float64(completed), float64(completed))
		}
		if workerCount == 1 && (busy != 0 || visits != completed) {
			t.Fatal("serial healthy control repeated ownership without a competing owner", busy, visits, completed, rounds)
		}
		out := map[string]any{"kind": "legacy_target_load_v1", "source_profile": profile, "page_limit": limit, "fixture_workers": workerCount, "shards": 16, "contracts": 1024, "tagged_targets": orderedTargets,
			"owner_policy": ownerPolicy, "rounds": rounds, "pages": len(pages), "visited": visits, "settled": completed, "busy": busy, "busy_intent": busyIntent, "busy_contract": busyContract, "busy_grant": busyGrant, "financial_callbacks": observer.attempts, "incomplete_financial_callbacks": observer.interrupted, "sql_call_equalities_qualified": sqlDelta != nil, "grant_wait_timed_out": grantWaitTimedOut,
			"cohort_including_scheduled_owner_drains_wall_ns": cohortElapsed.Nanoseconds(), "including_owner_drain_wall_ns": allElapsed.Nanoseconds(), "visits_per_second": float64(visits) / cohortElapsed.Seconds(), "settled_per_second": float64(completed) / cohortElapsed.Seconds(),
			"mirror_peak_pending": peakQueued, "mirror_pending_before_final_drain": queuedBeforeDrain, "mirror_starts": ownerStarts, "mirror_finished": ownerFinished, "mirror_pending_after_drain": legacyTargetMirrorQueueCount(ctx),
			"redis_commands": commands, "redis_client_dispatches": dispatches, "pg_stat_statements_available": beforeSql != nil, "sql_statement_delta": sqlDelta, "page_phase_totals": phaseTotals, "visits_per_page": pageVisits,
			"qualifiers": []string{"Native fixture measurement, not Main throughput or account ETA.", "Target timestamps observe real callback attempts/completions; barriers and accounting determine correctness, never wall-time cutoffs.", "Client Redis dispatches are not cluster network round trips. Phase families overlap.", "SQL totals include nested statements when tracked; top-level families are separate, and nested execution times overlap their callers. SQL calls are not client network round trips.", "PGSS counts successfully completed statements; an aborted grant-wait statement is reported separately, not invented as a captured call.", "The 1024-row fixture has 64 original rows per shard and probes contention/query amplification; it does not saturate the 256 row ceiling or establish a page-cap speedup.", "SQL statement deltas partition session, transaction, selector, ownership, accounting, mirror/task, and observer work; raw normalized fingerprints and unclassified rows remain visible. Setup and final accounting/replay assertions are outside the interval.", "Source profile labels require the native runner's exact source-hash binding.", "Normalized query text volume excludes bind, protocol and result bytes; it is not SQL network bytes.", "Owner cadence only creates amplification if multiple rounds actually execute. No ratio is asserted from the schedule label."}}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_target_load_metrics=%s", raw)
	})
}

func legacyTargetMirrorQueueCount(ctx context.Context) int {
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE function_name='github.com/urnetwork/server/model.ApplyLegacyNetEscrowMirror'`).Scan(&count))
	})
	return count
}

func legacyTargetAssertLoadConservation(t testing.TB, ctx context.Context, f legacyTargetLoadFixture) {
	t.Helper()
	projectLegacyProviderTotalsForTest(t, ctx)
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled')=1024
			AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1))
			AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND payout_byte_count=1)=1024
			AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1024
			AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1))=1024`, f.ids).Scan(&exact))
		if !exact {
			t.Fatal("loaded cohort outcome, escrow, debit or provider sweep did not conserve")
		}
		for index, payer := range f.payers {
			var balance int64
			server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, payer.balanceId).Scan(&balance))
			if balance != 4000-int64(f.grantCounts[index]) {
				t.Fatal("loaded grant debit mismatch", index, balance)
			}
			var neighbors bool
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$1)
				AND (SELECT NOT settled AND NOT redis_reserved AND balance_byte_count=23 FROM transfer_escrow WHERE contract_id=$1 AND balance_id=$3)
				AND (SELECT outcome IS NULL FROM transfer_contract WHERE contract_id=$2)
				AND (SELECT NOT settled AND redis_reserved AND balance_byte_count=31 FROM transfer_escrow WHERE contract_id=$2 AND balance_id=$3)`, f.legacyNeighbors[index], f.redisNeighbors[index], payer.balanceId).Scan(&neighbors))
			if !neighbors {
				t.Fatal("loaded settlement changed a surviving neighbor", index)
			}
		}
		for index, provider := range f.providers {
			var bytes, revenue int64
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count,provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$1`, provider.destinationNetworkId).Scan(&bytes, &revenue))
			if bytes != int64(f.providerCounts[index]) || revenue != bytes {
				t.Fatal("loaded provider accounting mismatch", index, bytes, revenue)
			}
		}
	})
	for index, payer := range f.payers {
		if got := Testing_NetEscrowByteCount(ctx, payer.balanceId); got != 54 {
			t.Fatal("loaded settlement lost legacy23 or native31 neighbor", got)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			keys := redisContractReservationKeys(payer.balanceId)
			amount, err := r.HGet(ctx, keys[1], f.redisNeighbors[index].String()).Int64()
			server.Raise(err)
			expiry, err := r.ZScore(ctx, keys[2], f.redisNeighbors[index].String()).Result()
			server.Raise(err)
			if amount != 31 || expiry != f.redisExpiry[index] {
				t.Fatal("loaded mirror changed native neighbor token", index, amount, expiry)
			}
		})
	}
}
