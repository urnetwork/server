// Applied durable owners retain bounded preparation and exact credit custody.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// This is independent of pool size and timing: replay eligibility must still
// produce one bounded cohort, and malformed/multi-provider work stays separate.
func TestLegacyProviderTotalsAppliedOwnersShareBoundedPreparedReplay(t *testing.T) {
	networkId, otherNetworkId := server.NewId(), server.NewId()
	tasks := make([]*task.Task, 0, legacyProviderTotalsBatchLimit+5)
	appendTask := func(networkId server.Id, applied bool, multi bool) *task.Task {
		payload := legacyProviderTotalsPayload{Private: true, Version: 1, ContractId: server.NewId(), Applied: applied,
			Totals: []legacyProviderTotal{{NetworkId: networkId, Bytes: 17, Revenue: 29}}}
		if multi {
			payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: server.NewId(), Bytes: 31, Revenue: 43})
			slices.SortFunc(payload.Totals, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		queued := &task.Task{TaskId: server.NewId(), ArgsJson: string(data)}
		tasks = append(tasks, queued)
		return queued
	}
	for range legacyProviderTotalsBatchLimit + 1 {
		appendTask(networkId, true, false)
	}
	slices.SortFunc(tasks, func(a, b *task.Task) int { return a.TaskId.Cmp(b.TaskId) })
	mixedApplied := appendTask(otherNetworkId, true, false)
	mixedNew := appendTask(otherNetworkId, false, false)
	multi := appendTask(networkId, true, true)
	malformed := &task.Task{TaskId: server.NewId(), ArgsJson: "invalid"}
	tasks = append(tasks, malformed)
	prepared := NewLegacyProviderTotalsTaskTarget().(*legacyProviderTotalsTaskTarget).PrepareTaskBatch(tasks).(*legacyProviderTotalsBatchTarget)
	first := prepared.taskIdBatches[tasks[0].TaskId]
	if first == nil || len(first.taskIds) != legacyProviderTotalsBatchLimit {
		t.Fatal("applied owners bypassed the bounded prepared replay cohort")
	}
	for _, queued := range tasks[:legacyProviderTotalsBatchLimit] {
		if prepared.taskIdBatches[queued.TaskId] != first {
			t.Fatal("applied owner escaped its original replay cohort")
		}
	}
	mixed := prepared.taskIdBatches[mixedApplied.TaskId]
	if mixed == nil || mixed == first || len(mixed.taskIds) != 2 || prepared.taskIdBatches[mixedNew.TaskId] != mixed {
		t.Fatal("mixed replay markers split one provider or joined unrelated providers")
	}
	for _, queued := range []*task.Task{tasks[legacyProviderTotalsBatchLimit], multi, malformed} {
		if prepared.taskIdBatches[queued.TaskId] != nil {
			t.Fatal("preparation changed its size or payload boundary")
		}
	}
}

// Half the exact owners already committed. One registered replay cohort reads
// all eight markers but credits only the remaining four, then finalizes all eight.
func TestLegacyProviderTotalsMixedAppliedBatchCreditsOnlyUnapplied(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkId := server.NewId()
		ids := make([]server.Id, 0, 8)
		for range 8 {
			ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		original := task.GetTasks(ctx, ids...)
		target := providerQueueTestTarget(original, ids[:4])
		_, post, err := target.Run(ctx, original[ids[0]])
		server.Raise(err)
		if post == nil {
			t.Fatal("initial credit lost its no-post handback")
		}
		posts, err := post(nil)
		server.Raise(err)
		if len(posts) != 0 {
			t.Fatal("initial credit invented an external post")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var applied int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE task_id=ANY($1)
                AND (args_json::jsonb->>'applied')::boolean`, ids).Scan(&applied))
			if applied != 4 {
				t.Fatal("mixed replay fixture lost its exact committed half", applied)
			}
		}, server.OptNoRetry())
		var admissions, reruns atomic.Int32
		key := server.NewPgOwnershipKey("account_balance", networkId)
		observed := server.Testing_WithTxRerunHook(ctx, func() { reruns.Add(1) })
		observed = server.Testing_WithPgOwnershipObservation(observed, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted && slices.Contains(event.Keys, key) {
				admissions.Add(1)
			}
		})
		providerQueueTestFinalize(t, observed, original)
		if admissions.Load() != 1 || reruns.Load() != 0 {
			t.Fatalf("mixed replay opened separate provider transactions: admissions=%d reruns=%d", admissions.Load(), reruns.Load())
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT provided_byte_count=136 AND provided_net_revenue_nano_cents=232
                AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)=2
                FROM account_balance WHERE network_id=$1`, networkId).Scan(&exact))
			if !exact {
				t.Fatal("mixed replay credited an applied owner or lost an unapplied allocation")
			}
		}, server.OptNoRetry())
	})
}

// Read the already-registered finite-role metrics without opening another pool.
// These are point samples and cumulative creations, not a claimed global peak.
func providerReplayPoolSnapshot() (map[string]float64, error) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return nil, err
	}
	result := map[string]float64{}
	for _, family := range families {
		name := family.GetName()
		if name != "urnetwork_pg_pool_connections" && name != "urnetwork_pg_pool_connections_created_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			role, state := "", "created"
			for _, label := range metric.GetLabel() {
				switch label.GetName() {
				case "pool":
					role = label.GetValue()
				case "state":
					state = label.GetValue()
				}
			}
			if role != "default" && role != "maintenance" {
				continue
			}
			value := metric.GetGauge().GetValue()
			if metric.Counter != nil {
				value = metric.Counter.GetValue()
			}
			result[role+"/"+state] = value
		}
	}
	return result, nil
}

// Global slot counts make the retained failure diagnosable without disclosing
// addresses, database/user/application names, task ids or PostgreSQL query text.
func providerReplayCapacitySnapshot(t testing.TB, ctx context.Context, phase string) {
	t.Helper()
	pool, err := providerReplayPoolSnapshot()
	server.Raise(err)
	var maximum, reserved, clients, sameDatabase, idle, active, idleTx, applications int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT current_setting('max_connections')::integer,
            current_setting('superuser_reserved_connections')::integer +
                COALESCE(NULLIF(current_setting('reserved_connections',true),''),'0')::integer,
            count(*) FILTER (WHERE backend_type='client backend'),
            count(*) FILTER (WHERE backend_type='client backend' AND datname=current_database()),
            count(*) FILTER (WHERE backend_type='client backend' AND state='idle'),
            count(*) FILTER (WHERE backend_type='client backend' AND state='active'),
            count(*) FILTER (WHERE backend_type='client backend' AND state='idle in transaction'),
            count(DISTINCT application_name) FILTER (WHERE backend_type='client backend')
            FROM pg_stat_activity`).Scan(&maximum, &reserved, &clients, &sameDatabase, &idle, &active, &idleTx, &applications))
	}, server.OptNoRetry())
	t.Logf("provider replay capacity phase=%s server_max=%d reserved=%d clients=%d same_database=%d idle=%d active=%d idle_tx=%d application_groups=%d pool_sample=%v",
		phase, maximum, reserved, clients, sameDatabase, idle, active, idleTx, applications, pool)
}
