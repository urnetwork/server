// Matched public financial pages are followed by one ordinary Run owner.
// Default eligibility waits and full shutdown remain in the measured work.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

func TestLegacyFinancialCohortRunLoadedSamePayerSerial(t *testing.T) {
	legacyFinancialCohortRunLoaded(t, 1, false)
}

func TestLegacyFinancialCohortRunLoadedSamePayerConcurrent(t *testing.T) {
	legacyFinancialCohortRunLoaded(t, 4, false)
}

func TestLegacyFinancialCohortRunLoadedSamePayerCold(t *testing.T) {
	legacyFinancialCohortRunLoaded(t, 4, true)
}

// One statement observes each durable move atomically. Eligibility is the
// timestamp predicate only; it does not prove advisory-lock availability.
type legacyFinancialRunState struct {
	NowBlock          int64 `json:"client_now_block"`
	Pending           int   `json:"pending"`
	Finished          int   `json:"finished"`
	PostCompleted     int   `json:"post_completed"`
	Eligible          int   `json:"timestamp_eligible_pending"`
	Future            int   `json:"timestamp_future_pending"`
	TimestampClaims   int   `json:"timestamp_claimed_pending"`
	RetryErrors       int   `json:"pending_retry_errors"`
	FinishedErrors    int   `json:"finished_errors"`
	MissingRunTimes   int   `json:"missing_finished_run_times"`
	RegisteredPending int   `json:"all_registered_pending"`
}

// The observer adds real read work and a bounded endpoint-detection delay.
// Neither is subtracted from the matched result.
const legacyFinancialRunObserveSql = `SELECT
 (SELECT count(*) FROM pending_task WHERE task_id=ANY($1)),
 (SELECT count(*) FROM finished_task WHERE task_id=ANY($1)),
 (SELECT count(*) FROM finished_task WHERE task_id=ANY($1) AND post_completed),
 (SELECT count(*) FROM pending_task WHERE task_id=ANY($1) AND available_block <= $2),
 (SELECT count(*) FROM pending_task WHERE task_id=ANY($1) AND available_block > $2),
 (SELECT count(*) FROM pending_task WHERE task_id=ANY($1) AND release_time > $3),
 (SELECT count(*) FROM pending_task WHERE task_id=ANY($1) AND reschedule_error_count > 0),
 (SELECT count(*) FROM finished_task WHERE task_id=ANY($1)
   AND (COALESCE(reschedule_error,'') <> '' OR COALESCE(post_error,'') <> '')),
 (SELECT count(*) FROM finished_task WHERE task_id=ANY($1)
   AND (run_start_time IS NULL OR run_end_time IS NULL OR run_end_time < run_start_time)),
 (SELECT count(*) FROM pending_task WHERE function_name=ANY($4))`

func legacyFinancialRunSnapshot(ctx context.Context, ids []server.Id, names []string) legacyFinancialRunState {
	now := server.NowUtc()
	state := legacyFinancialRunState{NowBlock: now.Unix() / task.BlockSizeSeconds}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, legacyFinancialRunObserveSql, ids, state.NowBlock, now, names).Scan(
			&state.Pending, &state.Finished, &state.PostCompleted, &state.Eligible, &state.Future,
			&state.TimestampClaims, &state.RetryErrors, &state.FinishedErrors, &state.MissingRunTimes,
			&state.RegisteredPending))
	})
	return state
}

// Source identity is recorded by the runner; this result records actual work.
type legacyFinancialRunResult struct {
	Initial                  int                       `json:"initial_pending"`
	Finished                 int                       `json:"finished"`
	Remaining                int                       `json:"remaining"`
	LoopCount                int                       `json:"run_loops"`
	BatchSize                int                       `json:"batch_size"`
	PollTimeoutNs            int64                     `json:"poll_timeout_ns"`
	RetryTimeoutAfterErrorNs int64                     `json:"retry_timeout_after_error_ns"`
	DrainFinishTimeoutNs     int64                     `json:"drain_finish_timeout_ns"`
	DrainCancelTimeoutNs     int64                     `json:"drain_cancel_timeout_ns"`
	FinalizeTimeoutNs        int64                     `json:"finalize_timeout_ns"`
	TargetClaimLimits        map[string]int            `json:"target_claim_limits"`
	ClaimRegisteredOnly      bool                      `json:"claim_registered_targets_only"`
	ProviderCompletionOptIn  bool                      `json:"provider_completion_batch_opt_in"`
	ObserverPeriodNs         int64                     `json:"observer_period_ns"`
	ObserverCalls            int                       `json:"observer_calls"`
	InitialState             legacyFinancialRunState   `json:"initial_state"`
	FinalState               legacyFinancialRunState   `json:"final_state"`
	ChangedStates            []legacyFinancialRunState `json:"changed_states_first64"`
	ChangedStatesOmitted     int                       `json:"changed_states_omitted"`
	RunAndJoinWallNs         int64                     `json:"run_and_join_wall_ns"`
	ShutdownWallNs           int64                     `json:"drain_handback_join_wall_ns"`
	Joined                   bool                      `json:"actual_run_joined"`
	InflightAfterJoin        int                       `json:"inflight_after_join"`
	DrainCanceled            int                       `json:"drain_canceled"`
}

// No wrapper replaces the registered target, so batch preparation and each
// source's opt-in interfaces are exercised by the real worker unchanged.
func legacyFinancialRunDrainOwners(t testing.TB, ctx context.Context, expected int) (result legacyFinancialRunResult, returnErr error) {
	t.Helper()
	server.HandleError(func() {
		provider := NewLegacyProviderTotalsTaskTarget()
		mirror := task.NewTaskTargetWithPost(ApplyLegacyNetEscrowMirror, ApplyLegacyNetEscrowMirrorPost)
		names := []string{provider.TargetFunctionName(), mirror.TargetFunctionName()}
		ids := make([]server.Id, 0, expected)
		ownerArgs := map[server.Id]string{}
		ownerFunctions := map[server.Id]string{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT task_id,function_name,args_json
                FROM pending_task WHERE function_name=ANY($1) ORDER BY task_id LIMIT $2`, names, expected+1)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var name, args string
					server.Raise(rows.Scan(&id, &name, &args))
					if _, exists := ownerArgs[id]; exists {
						server.Raise(fmt.Errorf("duplicate durable owner identity"))
					}
					ids = append(ids, id)
					ownerArgs[id], ownerFunctions[id] = args, name
				}
			})
		})
		if len(ids) != expected {
			returnErr = fmt.Errorf("real Run owner scope differs: have=%d expected=%d", len(ids), expected)
			return
		}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		if settings.BatchSize != 4 || settings.PollTimeout != 5*time.Second || len(settings.TargetClaimLimits) != 0 {
			returnErr = fmt.Errorf("real Run matched default profile changed")
			return
		}
		result.LoopCount, result.BatchSize = 1, settings.BatchSize
		result.PollTimeoutNs = settings.PollTimeout.Nanoseconds()
		result.RetryTimeoutAfterErrorNs = settings.RetryTimeoutAfterError.Nanoseconds()
		result.DrainFinishTimeoutNs = settings.DrainFinishTimeout.Nanoseconds()
		result.DrainCancelTimeoutNs = settings.DrainCancelTimeout.Nanoseconds()
		result.FinalizeTimeoutNs = settings.FinalizeTimeout.Nanoseconds()
		result.TargetClaimLimits = settings.TargetClaimLimits
		result.ClaimRegisteredOnly = settings.ClaimRegisteredTargetsOnly
		if opted, ok := provider.(interface{ TaskCompletionBatchEnabled() bool }); ok {
			result.ProviderCompletionOptIn = opted.TaskCompletionBatchEnabled()
		}
		const period = 25 * time.Millisecond
		result.ObserverPeriodNs = period.Nanoseconds()
		state := legacyFinancialRunSnapshot(ctx, ids, names)
		result.ObserverCalls++
		result.InitialState, result.Initial, result.Remaining = state, state.Pending, state.Pending
		if state.Pending != expected || state.RegisteredPending != expected || state.Finished != 0 ||
			state.TimestampClaims != 0 || state.RetryErrors != 0 {
			returnErr = fmt.Errorf("real Run did not begin with its exact unclaimed owner scope: %+v", state)
			return
		}
		worker := task.NewTaskWorker(ctx, settings)
		worker.AddTargets(provider, mirror)
		done := make(chan struct{})
		var runErr error
		started := time.Now()
		go func() {
			defer close(done)
			server.HandleError(worker.Run, func(err error) { runErr = err })
		}()
		var stopOnce sync.Once
		var stopErr error
		stop := func() error {
			stopOnce.Do(func() {
				shutdown := time.Now()
				worker.Drain()
				handback := worker.WaitFinalHandback()
				worker.Close()
				select {
				case <-done:
					result.Joined = true
					stopErr = runErr
				case <-time.After(5 * time.Second):
					stopErr = fmt.Errorf("actual Run did not join after bounded drain and final handback")
				}
				if !handback && stopErr == nil {
					stopErr = fmt.Errorf("actual Run exhausted final handback")
				}
				result.ShutdownWallNs = time.Since(shutdown).Nanoseconds()
				result.InflightAfterJoin, result.DrainCanceled = worker.InflightCount(), worker.DrainCanceledCount()
				result.RunAndJoinWallNs = time.Since(started).Nanoseconds()
			})
			return stopErr
		}
		defer func() {
			if err := stop(); returnErr == nil {
				returnErr = err
			}
		}()
		tick := time.NewTicker(period)
		defer tick.Stop()
		previous := state
		for {
			state = legacyFinancialRunSnapshot(ctx, ids, names)
			result.ObserverCalls++
			result.FinalState, result.Finished, result.Remaining = state, state.Finished, state.Pending
			// Clock-only changes do not consume the retained state bound.
			comparison := state
			comparison.NowBlock = previous.NowBlock
			if comparison != previous {
				if len(result.ChangedStates) < 64 {
					result.ChangedStates = append(result.ChangedStates, state)
				} else {
					result.ChangedStatesOmitted++
				}
			}
			previous = state
			if state.Pending+state.Finished != expected || state.RegisteredPending != state.Pending ||
				state.RetryErrors != 0 || state.FinishedErrors != 0 || state.MissingRunTimes != 0 {
				returnErr = fmt.Errorf("real Run changed exact durable scope or retried a healthy owner: %+v", state)
				return
			}
			if state.Pending == 0 && state.PostCompleted == expected {
				break
			}
			select {
			case <-done:
				returnErr = fmt.Errorf("actual Run retired before all exact durable owners: state=%+v error=%v", state, runErr)
				return
			case <-ctx.Done():
				returnErr = ctx.Err()
				return
			case <-tick.C:
			}
		}
		if err := stop(); err != nil {
			returnErr = err
			return
		}
		if !result.Joined || result.InflightAfterJoin != 0 || result.DrainCanceled != 0 {
			returnErr = fmt.Errorf("real Run did not finish every admitted lifecycle: %+v", result)
			return
		}
		// The actual joined endpoint is followed by independent payload checks.
		// They are included in the enclosing full wall/SQL denominator.
		finished := task.GetFinishedTasks(ctx, ids...)
		if len(finished) != expected {
			returnErr = fmt.Errorf("real Run lost exact finished identities")
			return
		}
		for _, id := range ids {
			row := finished[id]
			if row == nil || row.FunctionName != ownerFunctions[id] || !row.PostCompleted || row.RescheduleError != "" || row.PostError != "" ||
				row.RunStartTime.IsZero() || row.RunEndTime.IsZero() || row.RunEndTime.Before(row.RunStartTime) {
				returnErr = fmt.Errorf("real Run changed exact finished custody or lifecycle")
				return
			}
			if row.FunctionName == provider.TargetFunctionName() {
				before, err := decodeLegacyProviderTotals(ownerArgs[id])
				if err != nil || before.Applied {
					returnErr = fmt.Errorf("real Run provider input was not an unapplied allocation")
					return
				}
				after, err := decodeLegacyProviderTotals(row.ArgsJson)
				before.Applied = true
				if err != nil || !reflect.DeepEqual(before, after) || row.ResultJson != "{}" {
					returnErr = fmt.Errorf("real Run replaced an immutable provider allocation")
					return
				}
			} else if row.ArgsJson != ownerArgs[id] {
				returnErr = fmt.Errorf("real Run replaced immutable mirror scope")
				return
			}
		}
	}, func(err error) { returnErr = err })
	return
}

// Successful statement calls/outer rows stay separate from exact durable owner
// identities. The complete raw delta remains available if a shape is unclassified.
func legacyFinancialRunSqlWork(delta *legacyTargetSqlWorkDelta) map[string]map[string]float64 {
	work := map[string]map[string]float64{}
	if delta == nil {
		return work
	}
	for _, row := range delta.Statements {
		if !row.TopLevel {
			continue
		}
		compact := strings.ToLower(strings.Join(strings.Fields(row.Query), ""))
		family := ""
		switch {
		case strings.HasPrefix(compact, "withremovedas(deletefrompending_taskwhere") && strings.Contains(compact, "copiedas(insertintofinished_task"):
			family = "completion_batch_outer_count_rows"
		case strings.HasPrefix(compact, "insertintofinished_task"):
			family = "completion_singleton_insert"
		case strings.HasPrefix(compact, "deletefrompending_task"):
			family = "completion_singleton_delete"
		case strings.HasPrefix(compact, "updatepending_tasksetclaim_time=") && strings.Contains(compact, "release_time=greatest("):
			family = "lease_heartbeat_singleton"
		case strings.HasPrefix(compact, "updatepending_tasksetclaim_time=") && strings.Contains(compact, "wheretask_id=any("):
			family = "lease_selected_batch"
		case strings.HasPrefix(compact, "updatepending_tasksetclaim_time=") && strings.Contains(compact, "wheretask_id="):
			family = "lease_selected_singleton"
		case strings.HasPrefix(compact, "insertintoaccount_balance(network_id,provided_byte_count,provided_net_revenue_nano_cents)"):
			family = "provider_account_additive_write"
		case strings.HasPrefix(compact, "updatepending_tasksetargs_json=jsonb_set(") && strings.Contains(compact, "wheretask_id=any("):
			family = "provider_applied_marker_batch"
		case strings.HasPrefix(compact, "updatepending_tasksetargs_json=jsonb_set("):
			family = "provider_applied_marker_singleton"
		case strings.HasPrefix(compact, "selectpg_advisory_unlock("):
			family = "claim_exact_unlock"
		case strings.HasPrefix(compact, "selectpg_advisory_unlock_all("):
			family = "claim_session_unlock_all"
		case strings.HasPrefix(compact, "select(selectcount(*)frompending_taskwheretask_id=any("):
			family = "fixture_endpoint_observer"
		}
		if family == "" {
			continue
		}
		if work[family] == nil {
			work[family] = map[string]float64{}
		}
		for name, value := range row.Metrics {
			work[family][name] += value
		}
	}
	return work
}

// Sixteen shards share one payer network and two grants 15:1; each shard has
// four providers. Independent queued mirror/provider owners drain through the
// actual worker, so reducing inline work cannot hide a growing output backlog.
func legacyFinancialCohortRunLoaded(t *testing.T, workers int, cold bool) {
	if os.Getenv("URN_LEGACY_COHORT_RUN_NATIVE") != "1" || os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("explicit isolated actual Run profile and statement statistics required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		// Each matched fixture owns real monotonic policy state; an earlier case
		// cannot donate an advisory cooldown to a later comparison.
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, newLegacyFinancialCohortCooldown())
		if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
			})
		}
		const count = 1024
		fixture := legacyFinancialCohortSeed(t, ctx, count, 16)
		if cold {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance_net_escrow_snapshot WHERE balance_id=ANY($1)`, fixture.balances))
			})
		}
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error { client.AddHook(hook); return nil }))
		measured := hook.context(ctx)
		protocol, closeProtocol := legacyFinancialRunProtocolBind(t, ctx)
		defer closeProtocol()
		cursors := make([]*LegacySettlementCursor, 16)
		payerCursors := make([]*LegacySettlementPayerCursor, 16)
		type pageResult struct {
			shard int
			page  LegacySettlementShardResult
			err   error
		}
		var pages []LegacySettlementShardResult
		completed, visits, busy, rounds := 0, 0, 0, 0
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		beforeCounter := contractClosedCounter.Snapshot()
		protocol.enabled(true)
		started := time.Now()
		for completed < count {
			previous := completed
			rounds++
			if rounds > count {
				t.Fatal("finite loaded cohort did not drain")
			}
			for offset := 0; offset < 16; offset += workers {
				results := make(chan pageResult, workers)
				var joined sync.WaitGroup
				for slot := 0; slot < workers && offset+slot < 16; slot++ {
					shard := offset + slot
					joined.Add(1)
					go func() {
						defer joined.Done()
						value := pageResult{shard: shard}
						server.HandleError(func() {
							value.page, value.err = FlushLegacySettlementShard(measured, shard, cursors[shard], payerCursors[shard], 256)
						}, func(err error) { value.err = err })
						results <- value
					}()
				}
				joined.Wait()
				close(results)
				for value := range results {
					if value.err != nil || value.page.Failed != 0 {
						t.Fatal("loaded public owner failed", value.shard, value.page, value.err)
					}
					pages = append(pages, value.page)
					completed += value.page.Completed
					visits += value.page.Visited
					busy += value.page.BusyOrGone
					wire, err := json.Marshal(value.page.Cursor)
					server.Raise(err)
					server.Raise(json.Unmarshal(wire, &cursors[value.shard]))
					wire, err = json.Marshal(value.page.PayerCursor)
					server.Raise(err)
					server.Raise(json.Unmarshal(wire, &payerCursors[value.shard]))
				}
			}
			if completed <= previous {
				t.Fatal("joined wave had no committed progress", rounds, completed)
			}
		}
		financialElapsed := time.Since(started)
		financialProtocol := protocol.snapshot()
		if completed != count {
			t.Fatal("loaded completion count escaped fixed cohort", completed)
		}
		beforeOwnersSql := legacyTargetSqlSnapshot(t, ctx)
		drain, drainErr := legacyFinancialRunDrainOwners(t, measured, count+2)
		if drainErr != nil {
			t.Fatal("durable output failed actual Run owner drain", drain, drainErr)
		}
		pendingBefore, finished := drain.Initial, drain.Finished
		includingOwners := time.Since(started)
		protocol.enabled(false)
		fullProtocol := protocol.snapshot()
		afterSql := legacyTargetSqlSnapshot(t, ctx)
		afterCounter := contractClosedCounter.Snapshot()
		if !beforeCounter.Stable || !afterCounter.Stable || afterCounter.Confirmed-beforeCounter.Confirmed != count || afterCounter.Uncertain != beforeCounter.Uncertain || afterCounter.Untracked != beforeCounter.Untracked {
			t.Fatal("actual Run load changed acknowledged financial ownership", beforeCounter, afterCounter)
		}
		fullSql := legacyTargetSqlDelta(t, beforeSql, afterSql)
		ownersSql := legacyTargetSqlDelta(t, beforeOwnersSql, afterSql)
		commands, dispatches := hook.snapshot()
		if finished != count+2 || pendingBefore != count+2 {
			t.Fatal("coalesced mirror or immutable provider owner cardinality differs", pendingBefore, finished)
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		for _, balanceId := range fixture.balances {
			if Testing_NetEscrowByteCount(ctx, balanceId) != 0 {
				t.Fatal("drained owner left stale legacy admission capacity")
			}
		}
		for shard := range 16 {
			page, err := FlushLegacySettlementShard(ctx, shard, nil, nil, 256)
			if err != nil || page.Completed != 0 || page.Visited != 0 {
				t.Fatal("loaded replay repeated financial work", page, err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		out := map[string]any{
			"profile": os.Getenv("URN_LEGACY_FINANCIAL_COHORT_PROFILE"), "contracts": count, "workers": workers, "cold_cache": cold,
			"payer_networks": 1, "shared_grants": 2, "grant_distribution": "15:1 within each shard", "providers": 4, "shards": 16,
			"rounds": rounds, "visits": visits, "completed": completed, "busy": busy, "pages": pages,
			"financial_and_joined_page_wall_ns": financialElapsed.Nanoseconds(), "including_owner_drain_wall_ns": includingOwners.Nanoseconds(),
			"after_financial_including_owner_observation_drain_and_join_wall_ns": (includingOwners - financialElapsed).Nanoseconds(),
			"financial_protocol_by_route":                                        financialProtocol, "including_owners_protocol_by_route": fullProtocol,
			"owner_phase_protocol_counter_delta_by_route": legacyFinancialRunProtocolDelta(financialProtocol, fullProtocol),
			"closes_per_second":                           float64(completed) / financialElapsed.Seconds(), "closes_per_second_including_owners": float64(completed) / includingOwners.Seconds(),
			"pending_owners_before_drain": pendingBefore, "finished_owners": finished, "pending_owners_after_drain": 0, "owner_run": drain,
			"redis_commands": commands, "redis_client_dispatches": dispatches, "sql": fullSql, "owner_run_sql": ownersSql, "owner_run_sql_work": legacyFinancialRunSqlWork(ownersSql),
			"qualifiers": []string{"Matched source/module/fixture runs are required; a profile label is not source identity.",
				"This local finite same-payer load is not Main overall close throughput, workload mix or a sustained5x claim.",
				"The full public shard owner includes payer fairness, cohort rollbacks/fallbacks, individual heads and joined page posts.",
				"Financial pages finish before one actual Run starts. This sequential geometry does not model producer/successor scheduling or overlap. One default BatchSize4/PollTimeout5s Run is used; no due timestamp is changed.",
				"All real task eligibility/poll waits, observer SQL/25ms endpoint detection, finalization, posts, Drain, WaitFinalHandback and actual Run join remain inside total cost.",
				"Provider accounting groups depend on actual already-claimed BatchSize4 work; finite64 grouping counts are not assumed.",
				"Completion CTE PGSS rows count its outer aggregate row, never finished owners. Exact durable identities establish the owner denominator.",
				"Phase families overlap; counts are owner invocations, and a cohort can serve several contract outcomes.",
				"SQL statement totals include explicitly separate nested work; normalized query text volume is not protocol bytes.",
				"Redis hook dispatches are not cluster network round trips. Both ordinary and direct-maintenance PostgreSQL routes use the identical no-delay frame observer in every arm.",
				"ReadyForQuery observations measure protocol completion cycles, including startup and batched SQL; they are not SQL statement counts or IP-packet counts. Byte totals retain frontend reads and complete backend frames only.",
				"Protocol counters cover the full timed work, including the intermediate statement snapshot and owner observer. Initial/final PGSS snapshots are outside the protocol window. Transaction intervals are client observations, not grant-lock residence.",
				"Pages use one or four bounded fixture slots in joined waves. Their join barriers and final sequential output phase remain in elapsed time; production producer scheduling and external open traffic are not modeled."},
		}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_financial_cohort_run_load=%s", raw)
	})
}
