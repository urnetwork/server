// Provider admission preserves exact immutable allocations across owners. The
// financial prototype separately supplies real payer/escrow throughput work.
package model

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Admission uses provider identity, independently of the originating contract.
// Multi-provider tasks cannot share a writer; applied/invalid owners still reach
// the ordinary durable reader and its existing replay/error policy.
func TestLegacyProviderTotalsClaimGroupsMatchImmutableAllocations(t *testing.T) {
	target := NewLegacyProviderTotalsTaskTarget()
	grouper, ok := target.(task.TaskClaimGroupTarget)
	if !ok {
		t.Fatal("registered provider target has no writer group admission")
	}
	networkIds := []server.Id{server.NewId(), server.NewId()}
	slices.SortFunc(networkIds, func(a, b server.Id) int { return a.Cmp(b) })
	payload := legacyProviderTotalsPayload{Private: true, Version: 1, ContractId: server.NewId(), Totals: []legacyProviderTotal{{NetworkId: networkIds[0], Bytes: 17, Revenue: 29}}}
	encode := func() string { data, err := json.Marshal(payload); server.Raise(err); return string(data) }
	ids, limit := grouper.TaskClaimGroupIds(encode())
	if !reflect.DeepEqual(ids, networkIds[:1]) || limit != legacyProviderTotalsBatchLimit {
		t.Fatal("single provider did not retain its bounded prepared group")
	}
	payload.ContractId = server.NewId()
	otherIds, otherLimit := grouper.TaskClaimGroupIds(encode())
	if !reflect.DeepEqual(ids, otherIds) || otherLimit != limit {
		t.Fatal("another contract bypassed its shared provider group")
	}
	payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: networkIds[1], Bytes: 31, Revenue: 43})
	ids, limit = grouper.TaskClaimGroupIds(encode())
	if !reflect.DeepEqual(ids, networkIds) || limit != 1 {
		t.Fatal("multi-provider allocation failed to reserve every writer exclusively")
	}
	payload.Totals = append(payload.Totals, legacyProviderTotal{NetworkId: server.NewId(), Bytes: 47, Revenue: 59})
	slices.SortFunc(payload.Totals, func(a, b legacyProviderTotal) int { return a.NetworkId.Cmp(b.NetworkId) })
	if ids, _ := grouper.TaskClaimGroupIds(encode()); len(ids) != 0 {
		t.Fatal("oversized resource group escaped the fixed admission work bound")
	}
	payload.Totals = payload.Totals[:1]
	payload.Applied = true
	if ids, _ := grouper.TaskClaimGroupIds(encode()); len(ids) != 0 {
		t.Fatal("applied owner reserved an accounting writer")
	}
	if ids, _ := grouper.TaskClaimGroupIds("invalid"); len(ids) != 0 {
		t.Fatal("malformed owner donated an admission identity")
	}
}

// Valid larger allocations keep the original accounting path and do not poison
// unrelated work merely because the optional group optimization is unsupported.
func TestLegacyProviderTotalsOversizedGroupsKeepOrdinaryAccounting(t *testing.T) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		networkIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		multiId := providerTotalsTestPublish(ctx, server.NewId(), map[server.Id]*contractPayout{
			networkIds[0]: {payoutByteCount: 31, payout: 43}, networkIds[1]: {payoutByteCount: 31, payout: 43}, networkIds[2]: {payoutByteCount: 31, payout: 43},
		})
		healthyId := providerTotalsTestTask(ctx, server.NewId(), networkIds[3])
		ids := []server.Id{multiId, healthyId}
		original := task.GetTasks(ctx, ids...)
		providerTotalsBatchDue(ctx, ids)
		worker := task.NewTaskWorkerWithDefaults(ctx)
		defer worker.Close()
		worker.AddTargets(NewLegacyProviderTotalsTaskTarget())
		finished, retried, posts, err := worker.EvalTasks(2)
		if err != nil || len(finished) != 2 || len(retried)+len(posts) != 0 {
			t.Fatal("optional group bound changed valid accounting eligibility", len(finished), err)
		}
		completed := task.GetFinishedTasks(ctx, ids...)
		if len(completed) != 2 || len(task.GetTasks(ctx, ids...)) != 0 {
			t.Fatal("oversized group lost its exact durable owners")
		}
		for _, id := range ids {
			before, err := decodeLegacyProviderTotals(original[id].ArgsJson)
			server.Raise(err)
			after, err := decodeLegacyProviderTotals(completed[id].ArgsJson)
			server.Raise(err)
			before.Applied = true
			if !reflect.DeepEqual(before, after) {
				t.Fatal("ordinary fallback changed retained allocations")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM account_balance WHERE network_id=ANY($1) AND provided_byte_count=31 AND provided_net_revenue_nano_cents=43)=3
 AND EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2 AND provided_byte_count=17 AND provided_net_revenue_nano_cents=29)
 AND (SELECT count(*) FROM test_provider_total_write)=4`, networkIds[:3], networkIds[3]).Scan(&exact))
			if !exact {
				t.Fatal("ordinary oversized allocation changed exact provider credits")
			}
		})
	})
}

// The wrapper holds before SQL while preserving the registered target's group,
// batch preparation and completion opt-in. It never replaces its accounting.
type providerSerialHeldTarget struct {
	task.Target
	heldIds map[server.Id]bool
	entered chan server.Id
	release <-chan struct{}
}

func (self *providerSerialHeldTarget) TaskClaimGroupIds(args string) ([]server.Id, int) {
	if group, ok := self.Target.(task.TaskClaimGroupTarget); ok {
		return group.TaskClaimGroupIds(args)
	}
	return nil, 0
}

func (self *providerSerialHeldTarget) TaskCompletionBatchEnabled() bool {
	group, ok := self.Target.(task.TaskCompletionBatchTarget)
	return ok && group.TaskCompletionBatchEnabled()
}

func (self *providerSerialHeldTarget) PrepareTaskBatch(tasks []*task.Task) task.Target {
	target := self.Target
	if batch, ok := target.(task.TaskBatchPreparer); ok {
		target = batch.PrepareTaskBatch(tasks)
	}
	return &providerSerialHeldTarget{Target: target, heldIds: self.heldIds, entered: self.entered, release: self.release}
}

func (self *providerSerialHeldTarget) Run(ctx context.Context, queued *task.Task) (any, func(server.PgTx) ([]server.PostFunction, error), error) {
	if self.heldIds[queued.TaskId] {
		self.entered <- queued.TaskId
		select {
		case <-self.release:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	return self.Target.Run(ctx, queued)
}

// A peer cannot enter the same writer while its exact owners are live. A
// canceled owner retains all amounts; a fresh real worker applies each once.
func TestLegacyProviderTotalsSerialClaimReleasePreservesAccounting(t *testing.T) {
	legacyProviderTotalsSerialClaimRecovery(t, false)
}

func TestLegacyProviderTotalsSerialClaimCancellationPreservesAccounting(t *testing.T) {
	legacyProviderTotalsSerialClaimRecovery(t, true)
}

func legacyProviderTotalsSerialClaimRecovery(t *testing.T, cancelHeld bool) {
	providerTotalsTestEnv(t, func(t testing.TB, ctx context.Context) {
		providerTotalsBatchWriteCounter(t, ctx)
		provider, healthy := server.NewId(), server.NewId()
		ids := []server.Id{}
		for index := range 4 {
			networkId := provider
			if index == 3 {
				networkId = healthy
			}
			ids = append(ids, providerTotalsTestTask(ctx, server.NewId(), networkId))
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for index, id := range ids {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$3 WHERE task_id=$1`, id, server.NowUtc().Add(-time.Hour+time.Duration(index)*time.Minute), time.Time{}))
			}
		})
		original := task.GetTasks(ctx, ids...)
		entered, release := make(chan server.Id, 2), make(chan struct{})
		var releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		first, peer := task.NewTaskWorkerWithDefaults(ctx), task.NewTaskWorkerWithDefaults(ctx)
		defer peer.Close()
		first.AddTargets(&providerSerialHeldTarget{Target: NewLegacyProviderTotalsTaskTarget(), heldIds: map[server.Id]bool{ids[0]: true, ids[1]: true}, entered: entered, release: release})
		peer.AddTargets(NewLegacyProviderTotalsTaskTarget())
		type outcome struct {
			finished, retried, posts []server.Id
			err                      error
		}
		done := make(chan struct{})
		var held outcome
		go func() {
			defer close(done)
			server.HandleError(func() { held.finished, held.retried, held.posts, held.err = first.EvalTasks(2) }, func(err error) { held.err = err })
		}()
		defer func() {
			unblock()
			first.Close()
			select {
			case <-done:
			case <-ctx.Done():
				t.Error("held provider evaluator did not join")
			}
		}()
		seen := map[server.Id]bool{}
		for len(seen) < 2 {
			select {
			case id := <-entered:
				seen[id] = true
			case <-ctx.Done():
				t.Fatal("exact claimed providers did not reach the SQL entry barrier")
			}
		}
		finished, retried, posts, err := peer.EvalTasks(2)
		if err != nil || len(finished) != 1 || finished[0] != ids[3] || len(retried)+len(posts) != 0 {
			t.Fatal("shared provider ran concurrently or suppressed the healthy provider", len(finished), len(retried), err)
		}
		waiting := task.GetTasks(ctx, ids[2])[ids[2]]
		if waiting == nil || waiting.ArgsJson != original[ids[2]].ArgsJson || !waiting.ClaimTime.Equal(original[ids[2]].ClaimTime) || !waiting.ReleaseTime.Equal(original[ids[2]].ReleaseTime) || waiting.RescheduleErrorCount != 0 {
			t.Fatal("provider admission refusal changed its durable retry authority")
		}
		if cancelHeld {
			first.Close()
		} else {
			unblock()
		}
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("held accounting owner did not retire")
		}
		if held.err != nil || len(held.posts) != 0 {
			t.Fatal("held accounting finalization failed", held.err)
		}
		wantWrites := 2
		if cancelHeld {
			if len(held.finished) != 0 || len(held.retried) != 2 {
				t.Fatal("canceled owners were acknowledged or discarded")
			}
			for _, id := range ids[:3] {
				requireProviderTotalsTestState(t, ctx, id, provider, false, 0, 0)
			}
			providerTotalsBatchDue(ctx, ids[:3])
			wantWrites = 1
		} else if len(held.finished) != 2 || len(held.retried) != 0 {
			t.Fatal("released owners did not complete their exact accounting")
		}
		remaining := 1
		if cancelHeld {
			remaining = 3
		}
		finished, retried, posts, err = peer.EvalTasks(remaining)
		if err != nil || len(finished) != remaining || len(retried)+len(posts) != 0 {
			t.Fatal("fresh registered reader did not finish retained allocations", len(finished), err)
		}
		completed := task.GetFinishedTasks(ctx, ids...)
		if len(completed) != 4 || len(task.GetTasks(ctx, ids...)) != 0 {
			t.Fatal("serial provider drain changed exact durable identities")
		}
		for _, id := range ids {
			before, err := decodeLegacyProviderTotals(original[id].ArgsJson)
			server.Raise(err)
			after, err := decodeLegacyProviderTotals(completed[id].ArgsJson)
			server.Raise(err)
			before.Applied = true
			if !reflect.DeepEqual(before, after) || completed[id].ResultJson != "{}" || !completed[id].PostCompleted {
				t.Fatal("serial provider finalization changed an immutable allocation")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM account_balance WHERE network_id=$1 AND provided_byte_count=51 AND provided_net_revenue_nano_cents=87)
 AND EXISTS(SELECT 1 FROM account_balance WHERE network_id=$2 AND provided_byte_count=17 AND provided_net_revenue_nano_cents=29)
 AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$1)=$3
 AND (SELECT count(*) FROM test_provider_total_write WHERE network_id=$2)=1`, provider, healthy, wantWrites).Scan(&exact))
			if !exact {
				t.Fatal("serial provider drain duplicated, lost or split exact accounting")
			}
		})
	})
}
