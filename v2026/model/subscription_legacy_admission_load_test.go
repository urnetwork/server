// The fixed 1,024-row workload measures candidate overhead and busy shedding.
package model

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestLegacySettlementAdmissionLoadedAfterCohort(t *testing.T) {
	testLegacySettlementAdmissionLoaded(t, "after_cohort", 4, false)
}

func TestLegacySettlementAdmissionLoadedAfterEachRound(t *testing.T) {
	testLegacySettlementAdmissionLoaded(t, "after_each_round", 4, false)
}

func TestLegacySettlementAdmissionLoadedSerial(t *testing.T) {
	testLegacySettlementAdmissionLoaded(t, "after_cohort", 1, false)
}

// Four active shards each contain 320 rows. Real 256-slot initial pages must
// produce continuation cursors before the remaining funded tail can settle.
func TestLegacySettlementAdmissionLoadedContinued(t *testing.T) {
	testLegacySettlementAdmissionLoaded(t, "after_cohort", 4, true)
}

func testLegacySettlementAdmissionLoaded(t *testing.T, ownerPolicy string, workerCount int, dense bool) {
	profile := os.Getenv("URN_LEGACY_SOURCE_PROFILE")
	if profile == "" {
		t.Skip("explicit frozen source profile required for the native measurement matrix")
	}
	limit := 256
	switch profile {
	case "candidate", "candidate_continued", "V14_baseline":
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
		var fixture legacyTargetLoadFixture
		if dense {
			fixture = legacyAdmissionSeedDenseLoad(t, ctx)
		} else {
			fixture = legacyTargetSeedLoad(t, ctx)
		}
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(r server.RedisClient) { r.AddHook(hook) })
		server.Raise(server.RedisWithDeadline(ctx, func(r server.RedisClient) error { r.AddHook(hook); return nil }))
		measured := hook.context(ctx)
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		observer := &legacyTargetLoadObservation{started: time.Now(), targets: fixture.targets, visits: map[server.Id]*legacyTargetVisit{}}
		var cursors [16]*LegacySettlementCursor
		var pages []LegacySettlementFlushResult
		ownerStarts, ownerFinished, peakQueued, rounds := 0, 0, 0, 0
		continuedPages, continuedForwardVisits := 0, 0
		var shardRemaining, eofWithPending, delayedWithPending [16]int
		for _, id := range fixture.ids {
			shardRemaining[int(id[15])%16]++
		}
		activeShards := 0
		for _, remaining := range shardRemaining {
			if remaining > 0 {
				activeShards++
			}
		}
		type pageResult struct {
			shard     int
			continued bool
			result    LegacySettlementFlushResult
			err       error
		}
		// The closed fixture has no outside grant owner or new queue arrivals.
		// Every joined wave must durably remove exactly its completed contracts.
		// Strict positive descent gives a workload-derived wave bound; a busy
		// wave with no progress fails immediately rather than spinning to a cap.
		lastRemaining := len(fixture.ids)
		waveProgress := make([]map[string]int, 0, len(fixture.ids))
		for ; lastRemaining > 0 && rounds < len(fixture.ids); rounds++ {
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
						cursor := cursors[shard]
						page, err := legacyTargetObservedPage(measured, shard, cursor, limit, observer)
						results <- pageResult{shard: shard, continued: cursor != nil, result: page, err: err}
					}
				}()
			}
			workers.Wait()
			close(results)
			waveCompleted, waveVisited, waveBusy := 0, 0, 0
			for result := range results {
				if result.err != nil || result.result.Failed != 0 {
					t.Fatalf("loaded financial page failed: shard=%d result=%+v err=%v", result.shard, result.result, result.err)
				}
				waveCompleted += result.result.Completed
				waveVisited += result.result.Visited
				waveBusy += result.result.BusyOrGone
				shardRemaining[result.shard] -= result.result.Completed
				if shardRemaining[result.shard] < 0 {
					t.Fatal("shard completed outside its fixed cohort", result.shard)
				}
				if shardRemaining[result.shard] > 0 {
					if !result.result.More {
						eofWithPending[result.shard]++
					}
					// This is the frozen task successor's scheduling condition.
					// Count it without sleeping or changing the stress schedule.
					if !result.result.More || result.result.Completed == 0 {
						delayedWithPending[result.shard]++
					}
				}
				if result.continued {
					continuedPages++
					continuedForwardVisits += result.result.Visited - result.result.HeadVisited
				}
				cursors[result.shard] = result.result.Cursor
				if dense {
					// Resume exactly the native result through the task's JSON
					// representation, without synthesizing a frontier tuple.
					raw, err := json.Marshal(result.result.Cursor)
					server.Raise(err)
					var resumed *LegacySettlementCursor
					server.Raise(json.Unmarshal(raw, &resumed))
					cursors[result.shard] = resumed
				}
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
			wave := map[string]int{"wave": rounds + 1, "before_remaining": lastRemaining, "after_remaining": remaining,
				"completed": waveCompleted, "visited": waveVisited, "busy": waveBusy}
			waveProgress = append(waveProgress, wave)
			if remaining < 0 || remaining > lastRemaining || lastRemaining-remaining != waveCompleted {
				t.Fatalf("joined wave changed the exact cohort outside its committed completions: %+v", wave)
			}
			if remaining == lastRemaining {
				t.Fatalf("joined wave made no durable progress despite no external owner or arrivals: %+v", wave)
			}
			lastRemaining = remaining
		}
		if lastRemaining != 0 {
			t.Fatalf("strict cohort descent exceeded its derived bound: rounds=%d remaining=%d", rounds, lastRemaining)
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
		busyAdmission, headVisits := 0, 0
		pageVisits := map[int]int{}
		for _, page := range pages {
			// The same measurement compiles on the exact pre-admission source.
			// New counters are decoded explicitly; absent baseline fields are zero.
			var admission struct {
				Busy int `json:"busy_admission_deferred"`
				Head int `json:"head_busy_admission_deferred"`
			}
			encoded, err := json.Marshal(page)
			server.Raise(err)
			server.Raise(json.Unmarshal(encoded, &admission))
			visits += page.Visited
			completed += page.Completed
			busy += page.BusyOrGone
			busyIntent += page.BusyIntentUnavailable
			busyContract += page.BusyContractUnavailable
			busyGrant += page.BusyGrantSetMismatch
			busyAdmission += admission.Busy
			headVisits += page.HeadVisited
			if admission.Head != 0 {
				t.Fatal("head revisit was excluded by optional admission")
			}
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
		if completed != len(fixture.ids) {
			t.Fatal("global completions disagree with exact cohort", completed)
		}
		if dense && (continuedPages == 0 || continuedForwardVisits == 0) {
			t.Fatal("dense workload never exercised its real continuation", continuedPages, continuedForwardVisits)
		}
		orderedTargets := make([]legacyTargetVisit, len(fixture.targets))
		for id, ordinal := range fixture.targets {
			visit := observer.visits[id]
			if visit == nil || visit.Attempts < 1 || visit.SettledNs <= 0 {
				t.Fatal("global completion count concealed an unvisited or unsettled target", ordinal)
			}
			orderedTargets[ordinal] = *visit
		}
		assertConservation := legacyTargetAssertLoadConservation
		if dense {
			assertConservation = legacyAdmissionAssertDenseLoadConservation
		}
		assertConservation(t, ctx, fixture)
		for id := range fixture.targets {
			complete, busy, _, err := flushLegacySettlement(ctx, id)
			if err != nil || complete || !busy {
				t.Fatal("tagged load replay reclaimed a financial owner")
			}
		}
		assertConservation(t, ctx, fixture)
		sqlDelta := legacyTargetSqlDelta(t, beforeSql, afterSql)
		if sqlDelta != nil {
			legacyTargetRequireSqlFamily(t, sqlDelta, "intent_ownership", float64(visits-busyAdmission), float64(visits-busyAdmission-busyIntent))
			legacyTargetRequireSqlFamily(t, sqlDelta, "contract_ownership", float64(visits-busyAdmission-busyIntent), float64(visits-busyAdmission-busyIntent-busyContract))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_membership", float64(visits-busyAdmission-busyIntent-busyContract), float64(visits-busyAdmission-busyIntent-busyContract))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_ownership", float64(visits-busyAdmission-busyIntent-busyContract-grantWaitTimedOut), float64(completed))
			lookups := visits - headVisits
			if profile == "V14_baseline" {
				lookups = 0
			} else if profile == "candidate_continued" {
				lookups = continuedForwardVisits
			}
			legacyAdmissionRequireLookupSql(t, sqlDelta, lookups)
			legacyTargetRequireSqlFamily(t, sqlDelta, "outcome_write", float64(completed), float64(completed))
			legacyTargetRequireSqlFamily(t, sqlDelta, "grant_debit", float64(completed), float64(completed))
		}
		if workerCount == 1 && (busy != 0 || visits != completed) {
			t.Fatal("serial healthy control repeated ownership without a competing owner", busy, visits, completed, rounds)
		}
		maxScheduledBackoffSeconds := 0
		for shard, remaining := range shardRemaining {
			if remaining != 0 {
				t.Fatal("completed cohort left a shard remainder", shard, remaining)
			}
			maxScheduledBackoffSeconds = max(maxScheduledBackoffSeconds, 2*delayedWithPending[shard])
		}
		out := map[string]any{"kind": "legacy_admission_target_load_v3", "source_profile": profile, "page_limit": limit, "fixture_workers": workerCount, "shards": 16, "active_shards": activeShards, "contracts": len(fixture.ids), "dense_continuation_workload": dense, "dense_cursor_json_resumption": dense, "continued_pages": continuedPages, "continued_forward_visits": continuedForwardVisits, "tagged_targets": orderedTargets,
			"eof_with_remaining_by_shard": eofWithPending, "scheduled_delay_boundaries_with_remaining_by_shard": delayedWithPending, "replayed_page_outcomes_max_shard_backoff_seconds": maxScheduledBackoffSeconds,
			"owner_policy": ownerPolicy, "completion_oracle": "strict_joined_wave_durable_descent", "wave_bound": len(fixture.ids), "wave_progress": waveProgress, "rounds": rounds, "pages": len(pages), "visited": visits, "settled": completed, "busy": busy, "busy_intent": busyIntent, "busy_contract": busyContract, "busy_grant": busyGrant, "busy_admission": busyAdmission, "head_visits": headVisits, "financial_callbacks": observer.attempts, "incomplete_financial_callbacks": observer.interrupted, "sql_call_equalities_qualified": sqlDelta != nil, "grant_wait_timed_out": grantWaitTimedOut,
			"cohort_including_scheduled_owner_drains_wall_ns": cohortElapsed.Nanoseconds(), "including_owner_drain_wall_ns": allElapsed.Nanoseconds(), "visits_per_second": float64(visits) / cohortElapsed.Seconds(), "settled_per_second": float64(completed) / cohortElapsed.Seconds(),
			"mirror_peak_pending": peakQueued, "mirror_pending_before_final_drain": queuedBeforeDrain, "mirror_starts": ownerStarts, "mirror_finished": ownerFinished, "mirror_pending_after_drain": legacyTargetMirrorQueueCount(ctx),
			"redis_commands": commands, "redis_client_dispatches": dispatches, "pg_stat_statements_available": beforeSql != nil, "sql_statement_delta": sqlDelta, "page_phase_totals": phaseTotals, "visits_per_page": pageVisits,
			"qualifiers": []string{"Native fixture measurement, not Main throughput or account ETA.", "Every fully joined wave must reduce the exact fixed-cohort remaining count by exactly its committed completions; zero-progress fails immediately. The workload cardinality bounds strict decreases.", "This stress schedule immediately retries EOF pages. The frozen production scheduler delays EOF or zero-completion successors by two seconds. Reported backoff sums replay the recorded outcomes only; real scheduling can change contention and outcomes, so these sums are not measured production latency or an ETA. Rounds and complete-work costs require an identical baseline comparison.", "The successor enables hints only on forward visits of pages with a nonnil input cursor. Fresh pages and all heads retain their original PG opportunities; enabled pages require a real PG grant-stage probe per observed membership set.", "Candidate financial-phase time includes optional membership lookup, Redis acquisition and token release, not only PostgreSQL ownership.", "Redis counters include both ordinary and deadline pools; dedicated admission PING/EVAL overhead is retained.", "The original frozen workload, accounting and query-family assertions are preserved with admitted attempts separated from hint-only deferrals.", "Target timestamps observe real callback attempts/completions; barriers and accounting determine correctness, never wall-time cutoffs.", "Client Redis dispatches are not cluster network round trips. Phase families overlap.", "SQL totals include nested statements when tracked; top-level families are separate, and nested execution times overlap their callers. SQL calls are not client network round trips.", "PGSS counts successfully completed statements; an aborted grant-wait statement is reported separately, not invented as a captured call.", "The short workload has1024 rows across16 shards (64 each); the dense workload has1280 rows across4 active shards (320 each), with16 scheduled shard slots. Dense continuations come only from real initial pages; neither shape proves Main throughput.", "SQL statement deltas partition session, transaction, selector, ownership, accounting, mirror/task, and observer work; raw normalized fingerprints and unclassified rows remain visible. Setup and final accounting/replay assertions are outside the interval.", "Source profile labels require the native runner's exact source-hash binding.", "Normalized query text volume excludes bind, protocol and result bytes; it is not SQL network bytes.", "Owner cadence only creates amplification if multiple rounds actually execute. No ratio is asserted from the schedule label."}}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_admission_target_load_metrics=%s", raw)
	})
}

// The one new bounded lookup must be visible exactly once per forward visit.
// A failed/incomplete lookup refuses this exact-count sample, not PG authority.
func legacyAdmissionRequireLookupSql(t testing.TB, delta *legacyTargetSqlWorkDelta, count int) {
	t.Helper()
	var calls, rows float64
	for _, statement := range delta.Statements {
		compact := strings.ReplaceAll(strings.ToLower(strings.Join(strings.Fields(statement.Query), " ")), " ", "")
		if statement.TopLevel && strings.HasPrefix(compact, "selectbalance_idfromtransfer_escrowwherecontract_id=$1orderbybalance_idlimit") {
			calls += statement.Metrics["calls"]
			rows += statement.Metrics["rows"]
		}
	}
	if calls != float64(count) || rows != float64(count) {
		t.Fatalf("admission lookup sample not qualified: calls=%v rows=%v, expected=%d; failed lookups may correctly fall through to PG", calls, rows, count)
	}
}
