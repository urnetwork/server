// The same actual Run owns discovery, financial turns and durable outputs.
// Competing baseline shards and serialized payer turns use identical slots.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Setup alone rewires three provider slots to one shared provider; the fourth
// remains independent for each payer. The joined fixture preserves the existing
// exact contract/report/grant oracle while summing shared provider credits once.
func legacyPayerPipelineSeed(t testing.TB, ctx context.Context, counts []int) (legacyFinancialCohortFixture, []server.Id) {
	t.Helper()
	var joined legacyFinancialCohortFixture
	providerIndexes := map[server.Id]int{}
	var shared netEscrowOrderingTestFixture
	var payers []server.Id
	for payerIndex, count := range counts {
		current := legacyFinancialCohortSeed(t, ctx, count, 16)
		payers = append(payers, current.payer.sourceNetworkId)
		if payerIndex == 0 {
			shared = current.providers[0]
			joined.payer = current.payer
		}
		for index := range 3 {
			current.providers[index] = shared
		}
		clients, networks := make([]server.Id, count), make([]server.Id, count)
		for index, providerIndex := range current.providerIndexes {
			provider := current.providers[providerIndex]
			clients[index], networks[index] = provider.destinationId, provider.destinationNetworkId
			joinedIndex, found := providerIndexes[provider.destinationNetworkId]
			if !found {
				joinedIndex = len(joined.providers)
				providerIndexes[provider.destinationNetworkId] = joinedIndex
				joined.providers = append(joined.providers, provider)
			}
			joined.providerIndexes = append(joined.providerIndexes, joinedIndex)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract AS contract
                SET destination_id=seed.client_id,destination_network_id=seed.network_id
                FROM unnest($1::uuid[],$2::uuid[],$3::uuid[]) AS seed(contract_id,client_id,network_id)
                WHERE contract.contract_id=seed.contract_id`, current.ids, clients, networks))
		}, server.TxReadCommitted, server.OptNoRetry())
		joined.ids = append(joined.ids, current.ids...)
		joined.balances = append(joined.balances, current.balances...)
		joined.balanceIds = append(joined.balanceIds, current.balanceIds...)
		joined.reservationAmounts = append(joined.reservationAmounts, current.reservationAmounts...)
		joined.shards = append(joined.shards, current.shards...)
	}
	joined.reports = legacyFinancialCohortReports(ctx, joined.ids)
	return joined, payers
}

// One statement observes financial and output custody together. The recurring
// shard owners may remain, but every other queued owner must finish naturally.
// Lock samples are backend observations, not measured wait durations or causes.
const legacyPayerPipelineObserveSql = `SELECT /* legacy_payer_pipeline_observe_v1 */ jsonb_build_object(
 'settled',(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome='settled'),
 'intents',(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1)),
 'unsettled',(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND NOT settled),
 'pending_outputs',(SELECT count(*) FROM pending_task WHERE function_name=ANY($2)),
 'pending_nonrecurring',(SELECT count(*) FROM pending_task WHERE function_name<>$3),
 'pending_recurring',(SELECT count(*) FROM pending_task WHERE function_name=$3),
 'pending_eligible',(SELECT count(*) FROM pending_task WHERE available_block<=$4),
 'pending_future',(SELECT count(*) FROM pending_task WHERE available_block>$4),
 'pending_claimed',(SELECT count(*) FROM pending_task WHERE release_time>$5),
 'pending_retry_count',(SELECT COALESCE(sum(reschedule_error_count),0) FROM pending_task),
 'finished',(SELECT count(*) FROM finished_task),
 'finished_outputs',(SELECT count(*) FROM finished_task WHERE function_name=ANY($2)),
 'provider_finished',(SELECT count(*) FROM finished_task WHERE function_name=$6),
 'unfinished_posts',(SELECT count(*) FROM finished_task WHERE NOT post_completed),
 'finished_retry_owners',(SELECT count(*) FROM finished_task WHERE COALESCE(reschedule_error,'')<>''),
 'finished_post_errors',(SELECT count(*) FROM finished_task WHERE COALESCE(post_error,'')<>''),
 'missing_run_times',(SELECT count(*) FROM finished_task WHERE run_start_time IS NULL OR run_end_time IS NULL OR run_end_time<run_start_time),
 'lock_wait_backends',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock'),
 'active_backends',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='active'),
 'idle_in_tx_backends',(SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND state='idle in transaction')
)`

type legacyPayerPipelineState struct {
	Settled             int   `json:"settled"`
	Intents             int   `json:"intents"`
	Unsettled           int   `json:"unsettled"`
	PendingOutputs      int   `json:"pending_outputs"`
	PendingNonrecurring int   `json:"pending_nonrecurring"`
	PendingRecurring    int   `json:"pending_recurring"`
	PendingEligible     int   `json:"pending_eligible"`
	PendingFuture       int   `json:"pending_future"`
	PendingClaimed      int   `json:"pending_claimed"`
	PendingRetryCount   int64 `json:"pending_retry_count"`
	Finished            int   `json:"finished"`
	FinishedOutputs     int   `json:"finished_outputs"`
	ProviderFinished    int   `json:"provider_finished"`
	UnfinishedPosts     int   `json:"unfinished_posts"`
	FinishedRetryOwners int   `json:"finished_retry_owners"`
	FinishedPostErrors  int   `json:"finished_post_errors"`
	MissingRunTimes     int   `json:"missing_run_times"`
	LockWaitBackends    int   `json:"lock_wait_backends"`
	ActiveBackends      int   `json:"active_backends"`
	IdleInTxBackends    int   `json:"idle_in_tx_backends"`
}

func legacyPayerPipelineSnapshot(ctx context.Context, ids []server.Id, outputNames []string, shardName, providerName string) legacyPayerPipelineState {
	var state legacyPayerPipelineState
	now := server.NowUtc()
	server.Db(ctx, func(conn server.PgConn) {
		var raw []byte
		server.Raise(conn.QueryRow(ctx, legacyPayerPipelineObserveSql, ids, outputNames, shardName,
			now.Unix()/task.BlockSizeSeconds, now, providerName).Scan(&raw))
		server.Raise(json.Unmarshal(raw, &state))
	})
	return state
}

// New ordinary RunOnce handback can also use a removed/copied CTE. Only the
// exact array/unnest form is batch completion; outer rows are never owner counts.
func legacyPayerPipelineSqlWork(delta *legacyTargetSqlWorkDelta) map[string]map[string]float64 {
	work := legacyFinancialRunSqlWork(delta)
	delete(work, "completion_batch_outer_count_rows")
	for _, row := range delta.Statements {
		if !row.TopLevel {
			continue
		}
		query := strings.ToLower(strings.Join(strings.Fields(row.Query), ""))
		family := ""
		switch {
		case strings.HasPrefix(query, "withremovedas(deletefrompending_taskwhere") && strings.Contains(query, "copiedas(insertintofinished_task"):
			if strings.Contains(query, "wheretask_id=any(") && strings.Contains(query, "fromunnest(") {
				family = "completion_batch_outer_count_rows"
			} else if strings.Contains(query, "wheretask_id=$") && !strings.Contains(query, "fromunnest(") {
				family = "completion_singleton_atomic_move"
			} else {
				family = "completion_cte_unclassified_shape"
			}
		case strings.Contains(query, "legacy_payer_pipeline_observe_v1"):
			family = "fixture_pipeline_observer"
		case strings.HasPrefix(query, "selectpg_try_advisory_lock(") && strings.Contains(query, "::integer,"):
			family = "provider_group_try_lock_calls_acceptance_unknown"
		case strings.HasPrefix(query, "selectpg_advisory_unlock(") && strings.Contains(query, "::integer,"):
			family = "provider_group_unlock_calls"
			// The inherited broad unlock family includes this same statement.
			// Move its metrics, so the named task/provider families are disjoint.
			for name, value := range row.Metrics {
				work["claim_exact_unlock"][name] -= value
			}
		case strings.Contains(query, "updatetransfer_contractsetoutcome=owned.outcome"):
			family = "financial_bulk_outcome"
		case strings.Contains(query, "updatetransfer_escrowasescrowsetsettled="):
			family = "financial_bulk_metadata"
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

type legacyPayerPipelineEdge struct {
	at    time.Time
	delta int
}

// Equal endpoint timestamps do not overlap. This is an observed function
// interval bound, not a claim that grants were simultaneously locked.
func legacyPayerPipelinePeak(edges []legacyPayerPipelineEdge) int {
	slices.SortFunc(edges, func(a, b legacyPayerPipelineEdge) int {
		if order := a.at.Compare(b.at); order != 0 {
			return order
		}
		return a.delta - b.delta
	})
	active, peak := 0, 0
	for _, edge := range edges {
		active += edge.delta
		peak = max(peak, active)
	}
	return peak
}

// Successful page result counts describe calls and can overlap task intervals.
// All exact finished output identities are checked independently of these sums.
func legacyPayerPipelineFinished(t testing.TB, ctx context.Context, fixture legacyFinancialCohortFixture,
	shardName, providerName, mirrorName string, payerNames []string, payerIds []server.Id) map[string]any {
	t.Helper()
	expectedProviders := map[server.Id]server.Id{}
	for index, id := range fixture.ids {
		expectedProviders[id] = fixture.providers[fixture.providerIndexes[index]].destinationNetworkId
	}
	balances := map[server.Id]bool{}
	for _, id := range fixture.balances {
		balances[id] = true
	}
	providerSeen := map[server.Id]bool{}
	mirrorSeen := map[server.Id]int{}
	payerSet := map[string]bool{}
	for _, name := range payerNames {
		payerSet[name] = true
	}
	expectedPayers := map[server.Id]bool{}
	for _, id := range payerIds {
		expectedPayers[id] = true
	}
	var shardEdges []legacyPayerPipelineEdge
	payerEdges := map[server.Id][]legacyPayerPipelineEdge{}
	pages := map[string]float64{}
	phases := map[string]LegacySettlementPhaseDuration{}
	functions := map[string]int{}
	functionWallNs := map[string]int64{}
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT task_id,function_name,args_json,result_json,post_completed,run_start_time,run_end_time
            FROM finished_task ORDER BY task_id LIMIT 16385`)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				count++
				var id server.Id
				var name, args, result string
				var postCompleted bool
				var start, end *time.Time
				server.Raise(rows.Scan(&id, &name, &args, &result, &postCompleted, &start, &end))
				if !postCompleted || start == nil || end == nil || end.Before(*start) {
					t.Fatal("pipeline finished owner lacks its complete lifecycle", name)
				}
				functions[name]++
				functionWallNs[name] += end.Sub(*start).Nanoseconds()
				switch {
				case name == providerName:
					payload, err := decodeLegacyProviderTotals(args)
					if err != nil || !payload.Applied || providerSeen[payload.ContractId] || len(payload.Totals) != 1 ||
						expectedProviders[payload.ContractId] != payload.Totals[0].NetworkId || payload.Totals[0].Bytes != 3 || payload.Totals[0].Revenue != 3 || result != "{}" {
						t.Fatal("pipeline changed an exact immutable provider allocation", err)
					}
					providerSeen[payload.ContractId] = true
				case name == mirrorName:
					payload, err := decodeLegacyNetEscrowMirror([]byte(args))
					var published LegacyNetEscrowMirrorResult
					if err != nil || !balances[payload.BalanceId] || json.Unmarshal([]byte(result), &published) != nil || published.Revision < 0 {
						t.Fatal("pipeline changed a durable mirror generation's scope", err)
					}
					mirrorSeen[payload.BalanceId]++
				case name == shardName || payerSet[name]:
					if name == shardName && end.After(*start) {
						shardEdges = append(shardEdges, legacyPayerPipelineEdge{at: *start, delta: 1}, legacyPayerPipelineEdge{at: *end, delta: -1})
					} else if payerSet[name] {
						var scope struct {
							Private        bool      `json:"_private_task_arguments"`
							PayerNetworkId server.Id `json:"payer_network_id"`
						}
						server.Raise(json.Unmarshal([]byte(args), &scope))
						if !scope.Private || !expectedPayers[scope.PayerNetworkId] {
							t.Fatal("pipeline payer task escaped exact fixture ownership")
						}
						if end.After(*start) {
							payerEdges[scope.PayerNetworkId] = append(payerEdges[scope.PayerNetworkId], legacyPayerPipelineEdge{at: *start, delta: 1}, legacyPayerPipelineEdge{at: *end, delta: -1})
						}
					}
					var raw map[string]json.RawMessage
					server.Raise(json.Unmarshal([]byte(result), &raw))
					for _, field := range []string{"visited", "completed", "busy_or_gone", "failed", "financial_cohort_attempts", "financial_cohort_selected", "financial_cohort_completed", "financial_cohort_fallbacks", "head_visited", "head_completed", "head_grant_wait_attempted", "head_grant_wait_completed", "head_grant_wait_timed_out", "busy_intent_unavailable", "busy_contract_unavailable", "busy_grant_set_mismatch", "busy_admission_deferred"} {
						if encoded := raw[field]; len(encoded) > 0 {
							var value float64
							server.Raise(json.Unmarshal(encoded, &value))
							pages[field] += value
						}
					}
					if encoded := raw["timings"]; len(encoded) > 0 {
						var timing map[string]LegacySettlementPhaseDuration
						server.Raise(json.Unmarshal(encoded, &timing))
						for name, phase := range timing {
							previous := phases[name]
							previous.Count += phase.Count
							previous.ElapsedMs += phase.ElapsedMs
							previous.MaxMs = max(previous.MaxMs, phase.MaxMs)
							phases[name] = previous
						}
					}
					if encoded := raw["dispatch"]; len(encoded) > 0 && string(encoded) != "null" {
						var dispatch struct {
							Probes             int  `json:"probes"`
							Registered         int  `json:"registered"`
							RegistrationFailed bool `json:"registration_failed"`
						}
						server.Raise(json.Unmarshal(encoded, &dispatch))
						pages["dispatch_calls"]++
						pages["dispatch_probes"] += float64(dispatch.Probes)
						pages["dispatch_registered"] += float64(dispatch.Registered)
						if dispatch.RegistrationFailed {
							pages["dispatch_registration_failed"]++
						}
					}
				}
			}
		})
	})
	if count > 16384 || len(providerSeen) != len(fixture.ids) {
		t.Fatal("pipeline lost its finite exact provider completion scope", count, len(providerSeen))
	}
	mirrors := 0
	for _, generations := range mirrorSeen {
		mirrors += generations
	}
	shardPeak, payerPeak := legacyPayerPipelinePeak(shardEdges), 0
	for _, edges := range payerEdges {
		payerPeak = max(payerPeak, legacyPayerPipelinePeak(edges))
	}
	if len(payerNames) == 0 && shardPeak < 2 {
		t.Fatal("reference did not exercise actual competing shard function intervals", shardPeak)
	}
	if len(payerNames) > 0 && (len(payerEdges) != len(payerIds) || payerPeak != 1) {
		t.Fatal("candidate did not retain one observed financial function per payer", len(payerEdges), payerPeak)
	}
	return map[string]any{"finished_tasks": count, "provider_owners": len(providerSeen), "mirror_owners": mirrors,
		"mirror_balance_scopes": len(mirrorSeen), "function_counts": functions, "function_interval_sum_ns": functionWallNs,
		"page_counts": pages, "phase_observations": phases, "peak_shard_function_intervals": shardPeak, "peak_same_payer_function_intervals": payerPeak}
}

// The external package supplies exact production discovery registration and
// scheduling. The test never invokes a financial page to make progress.
func TestingLegacyPayerPipeline(t *testing.T, counts []int, shard task.Target,
	schedule func(*session.ClientSession, server.PgTx)) {
	if os.Getenv("URN_LEGACY_PAYER_PIPELINE_NATIVE") != "1" || os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("explicit isolated actual payer pipeline and statement statistics required")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		ctx = context.WithValue(ctx, legacyFinancialCohortCooldownKey{}, newLegacyFinancialCohortCooldown())
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
		})
		fixture, payerIds := legacyPayerPipelineSeed(t, ctx, counts)
		legacySettlementPayerIndexes.stateLock.Lock()
		refreshing := legacySettlementPayerIndexes.refreshing
		legacySettlementPayerIndexes.expires = time.Time{}
		legacySettlementPayerIndexes.stateLock.Unlock()
		ready, indexErr := readLegacySettlementPayerIndexes(ctx)
		if len(fixture.ids) != 1024 || refreshing || indexErr != nil || !ready || !legacySettlementPayerIndexesReady(ctx) {
			t.Fatal("pipeline requires exact1024 inputs and the same ready payer index in both arms")
		}
		legacyFinancialCohortRequire(t, ctx, fixture, map[server.Id]bool{})
		hook := &legacyHotpathRedisCountHook{}
		server.Redis(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.RedisDoOnce(ctx, func(client server.RedisClient) { client.AddHook(hook) })
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error { client.AddHook(hook); return nil }))
		measured := hook.context(ctx)
		protocol, closeProtocol := legacyFinancialRunProtocolBind(t, ctx)
		defer closeProtocol()
		provider := NewLegacyProviderTotalsTaskTarget()
		mirror := legacyPayerPipelineMirrorTarget()
		outputNames := []string{provider.TargetFunctionName(), mirror.TargetFunctionName()}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		if settings.BatchSize != 4 || settings.PollTimeout != 5*time.Second || len(settings.TargetClaimLimits) != 0 {
			t.Fatal("pipeline changed the matched default worker profile")
		}
		additional := legacyPayerPipelineAdditionalTargets()
		var payerNames []string
		for _, target := range additional {
			payerNames = append(payerNames, target.TargetFunctionName())
		}
		worker := task.NewTaskWorker(measured, settings)
		defer worker.Close()
		worker.AddTargets(shard, provider, mirror)
		worker.AddTargets(additional...)
		before := legacyPayerPipelineSnapshot(ctx, fixture.ids, outputNames, shard.TargetFunctionName(), provider.TargetFunctionName())
		if before.Intents != len(fixture.ids) || before.Settled != 0 || before.Unsettled != len(fixture.ids) ||
			before.PendingNonrecurring != 0 || before.PendingRecurring != 0 || before.Finished != 0 {
			t.Fatal("pipeline did not start from equal unscheduled financial scope", before)
		}
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		beforeCounter := contractClosedCounter.Snapshot()
		done := make(chan struct{})
		var runErr error
		var stopErr error
		var joined bool
		var stopOnce sync.Once
		var shutdownNs int64
		stop := func() error {
			stopOnce.Do(func() {
				started := time.Now()
				worker.Drain()
				handback := worker.WaitFinalHandback()
				worker.Close()
				select {
				case <-done:
					joined = true
					stopErr = runErr
				case <-time.After(5 * time.Second):
					stopErr = fmt.Errorf("pipeline Run did not join after bounded drain/handback")
				}
				if !handback && stopErr == nil {
					stopErr = fmt.Errorf("pipeline final handback failed")
				}
				shutdownNs = time.Since(started).Nanoseconds()
			})
			return stopErr
		}
		protocol.enabled(true)
		started := time.Now()
		owner := session.NewLocalClientSession(measured, "", nil)
		defer owner.Cancel()
		server.Tx(measured, func(tx server.PgTx) { schedule(owner, tx) }, server.TxReadCommitted, server.OptNoRetry())
		go func() {
			defer close(done)
			server.HandleError(worker.Run, func(err error) { runErr = err })
		}()
		defer func() {
			if err := stop(); err != nil {
				t.Error(err)
			}
		}()
		const period = 25 * time.Millisecond
		tick := time.NewTicker(period)
		defer tick.Stop()
		observations, lockSamples, peakLockWaiters := 0, 0, 0
		var peakPendingRetryCount int64
		var changed []legacyPayerPipelineState
		omitted := 0
		previous := before
		var financialObserved time.Duration
		var financialProtocol map[string]map[string]int64
		for {
			state := legacyPayerPipelineSnapshot(measured, fixture.ids, outputNames, shard.TargetFunctionName(), provider.TargetFunctionName())
			observations++
			lockSamples += state.LockWaitBackends
			peakLockWaiters = max(peakLockWaiters, state.LockWaitBackends)
			peakPendingRetryCount = max(peakPendingRetryCount, state.PendingRetryCount)
			if state != previous {
				if len(changed) < 128 {
					changed = append(changed, state)
				} else {
					omitted++
				}
			}
			previous = state
			if state.Settled == len(fixture.ids) && state.Intents == 0 && financialObserved == 0 {
				financialObserved = time.Since(started)
				financialProtocol = protocol.snapshot()
			}
			if state.Settled == len(fixture.ids) && state.Intents == 0 && state.Unsettled == 0 && state.PendingNonrecurring == 0 &&
				state.ProviderFinished == len(fixture.ids) && state.UnfinishedPosts == 0 && worker.InflightCount() == 0 {
				break
			}
			select {
			case <-done:
				t.Fatal("actual Run retired before the complete pipeline", state, runErr)
			case <-ctx.Done():
				t.Fatal("finite actual pipeline did not drain", state, ctx.Err())
			case <-tick.C:
			}
		}
		if err := stop(); err != nil {
			t.Fatal(err)
		}
		final := legacyPayerPipelineSnapshot(measured, fixture.ids, outputNames, shard.TargetFunctionName(), provider.TargetFunctionName())
		observations++
		if !joined || worker.InflightCount() != 0 || worker.DrainCanceledCount() != 0 || final.Settled != len(fixture.ids) || final.Intents != 0 ||
			final.Unsettled != 0 || final.PendingNonrecurring != 0 || final.ProviderFinished != len(fixture.ids) || final.UnfinishedPosts != 0 || final.MissingRunTimes != 0 || final.FinishedPostErrors != 0 {
			t.Fatal("pipeline endpoint changed during joined shutdown", final)
		}
		fullWall := time.Since(started)
		protocol.enabled(false)
		fullProtocol := protocol.snapshot()
		afterSql := legacyTargetSqlSnapshot(t, ctx)
		afterCounter := contractClosedCounter.Snapshot()
		if !beforeCounter.Stable || !afterCounter.Stable || afterCounter.Confirmed-beforeCounter.Confirmed != uint64(len(fixture.ids)) ||
			afterCounter.Uncertain != beforeCounter.Uncertain || afterCounter.Untracked != beforeCounter.Untracked {
			t.Fatal("pipeline changed acknowledged financial custody", beforeCounter, afterCounter)
		}
		finished := legacyPayerPipelineFinished(t, ctx, fixture, shard.TargetFunctionName(), provider.TargetFunctionName(), mirror.TargetFunctionName(), payerNames, payerIds)
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		for _, balance := range fixture.balances {
			if Testing_NetEscrowByteCount(ctx, balance) != 0 {
				t.Fatal("pipeline retained stale admission capacity")
			}
		}
		for shard := range 16 {
			page, err := FlushLegacySettlementShard(ctx, shard, nil, nil, 256)
			if err != nil || page.Visited != 0 || page.Completed != 0 {
				t.Fatal("pipeline replay repeated financial work", page, err)
			}
		}
		legacyFinancialCohortRequire(t, ctx, fixture, legacyFinancialCohortCompleted(fixture.ids))
		replayCounter := contractClosedCounter.Snapshot()
		if !replayCounter.Stable || replayCounter.Confirmed != afterCounter.Confirmed || replayCounter.Uncertain != afterCounter.Uncertain || replayCounter.Untracked != afterCounter.Untracked {
			t.Fatal("pipeline replay changed committed accounting")
		}
		for route, counters := range fullProtocol {
			if counters["ready_replies_observed"] == 0 || counters["ready_replies_charged_delay"] != 0 || counters["held_financial_set_replies"] != 0 {
				t.Fatal("pipeline protocol route was missing or injected a delay", route, counters)
			}
		}
		commands, dispatches := hook.snapshot()
		sql := legacyTargetSqlDelta(t, beforeSql, afterSql)
		out := map[string]any{"profile": os.Getenv("URN_LEGACY_FINANCIAL_COHORT_PROFILE"), "contracts": len(fixture.ids), "payer_counts": counts,
			"provider_networks": len(fixture.providers), "provider_pattern": "75 percent shared provider,25 percent payer-specific provider", "shards": 16,
			"run_loops": 1, "batch_size": settings.BatchSize, "poll_timeout_ns": settings.PollTimeout.Nanoseconds(), "target_claim_limits": settings.TargetClaimLimits,
			"retry_timeout_after_error_ns": settings.RetryTimeoutAfterError.Nanoseconds(), "observer_period_ns": period.Nanoseconds(), "observer_calls": observations,
			"first_observed_all_financial_committed_wall_ns": financialObserved.Nanoseconds(), "full_pipeline_drain_join_wall_ns": fullWall.Nanoseconds(),
			"after_financial_observation_wall_ns": (fullWall - financialObserved).Nanoseconds(), "shutdown_wall_ns": shutdownNs,
			"closes_per_second_including_all_outputs": float64(len(fixture.ids)) / fullWall.Seconds(), "state_changes_first128": changed, "state_changes_omitted": omitted,
			"final_state": final, "finished_work": finished, "lock_wait_backend_samples": lockSamples, "peak_observed_lock_wait_backends": peakLockWaiters,
			"max_observed_pending_retry_count":        peakPendingRetryCount,
			"financial_observation_protocol_by_route": financialProtocol, "full_protocol_by_route": fullProtocol, "redis_commands": commands, "redis_dispatches": dispatches,
			"sql": sql, "sql_owner_work": legacyPayerPipelineSqlWork(sql),
			"qualifiers": []string{"Same seeded arrivals occur before one actual default Run. Actual shard tasks compete for shared grants in the reference; the candidate's actual dispatcher/payer targets replace those financial owners. No direct financial call advances measured work.",
				"Both sources retain actual2s task Post delays,5s default polling, current RunOnce generation behavior and BatchSize4. All eligibility, discovery, financial/post/output execution, endpoint observation, Drain/handback/join are in full wall.",
				"Recurring shard tasks may remain. No payer,provider,mirror or RunPost owner capable of publishing unfinished work may remain at the joined endpoint. Exact provider contracts and variable actual mirror generations are separately validated.",
				"Provider targets and any source-level provider-key admission are unchanged production factories. Actual claimed grouping determines account-write cost.",
				"Function/phase durations overlap. Busy gates are ownership observations, not proof of a live grant blocker.25ms Lock backend samples do not quantify wait duration or CPU.",
				"Finished successful function intervals establish observed shard overlap and payer serialization only; historical retry attempts are not reconstructed. This is a registered-target test worker, not the full subnet-operator profile.",
				"Finished tasks retain only the last reschedule error, not a durable retry counter. Finished retry-owner count and maximum observed pending retry count are separate lower-bound observations, not an exact total of retry events.",
				"Both PostgreSQL routes have identical no-delay protocol observers. Ready replies are protocol cycles, not SQL statements; frame byte and transaction-interval qualifications remain inherited.",
				"Full elapsed ends after actual Run joins and its final custody snapshot. Independent payload/ledger/immutable-report/mirror/replay validation then proves the result outside the throughput window.",
				"This is a fixed local backlog with matching source/graph/worker settings, not Main open-rate mix or a sustained5x claim. All observed retries and leftover work remain visible; no machine timing assertion is used."}}
		raw, err := json.Marshal(out)
		server.Raise(err)
		t.Logf("legacy_payer_pipeline_load=%s", raw)
	})
}
