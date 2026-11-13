// The release composition must wire the durable consumer through the real
// production registry and initializer, while preserving workload ownership.
package taskworker

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

// A manually registered fixture consumer is insufficient: the serving worker
// must register it, and a subnet operator must leave external probing alone.
func TestProviderEgressTallyProductionRegistryOwnsConsumer(t *testing.T) {
	name := task.NewTaskTarget(work.RollupProviderEgressTallies).TargetFunctionName()
	for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
		worker, err := InitTaskWorkerForProfile(t.Context(), nil, profile)
		if err != nil {
			t.Fatal(err)
		}
		present := worker.HasTarget(name)
		worker.Close()
		if present != (profile == WorkloadProfileProduction) {
			t.Fatalf("tally consumer registration profile=%q present=%t", profile, present)
		}
	}
}

// Repeated real startup preserves every shard's original generation/cursor.
// An unrelated workload initializer neither reseeds nor changes these owners.
func TestProviderEgressTallyProductionStartupPreservesShardOwners(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		controller.SetStConfig(&controller.StConfig{Enabled: false})
		defer controller.SetStConfig(nil)
		controller.SetVerifySettings(model.DefaultVerifySettings())
		defer controller.SetVerifySettings(nil)
		name := task.NewTaskTarget(work.RollupProviderEgressTallies).TargetFunctionName()
		owners := func() map[server.Id]*task.Task {
			values := map[server.Id]*task.Task{}
			for id, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
				if pending.FunctionName == name {
					values[id] = pending
				}
			}
			return values
		}
		if err := initTaskScheduleForProfile(ctx, WorkloadProfileProduction); err != nil {
			t.Fatal(err)
		}
		before := owners()
		if len(before) != model.ProviderEgressTallyShardCount {
			t.Fatalf("startup armed %d tally owners", len(before))
		}
		shards := map[int]bool{}
		for _, pending := range before {
			var args work.RollupProviderEgressTalliesArgs
			if err := json.Unmarshal([]byte(pending.ArgsJson), &args); err != nil {
				t.Fatal(err)
			}
			if args.Shard < 0 || args.Shard >= model.ProviderEgressTallyShardCount || shards[args.Shard] || args.Generation == "" || args.Cursor != "0-0" || !args.Initialize || args.RepairFrom != "" || pending.RunOnceKey != task.RunOnce("rollup_provider_egress_tallies_v1", args.Shard).String() || time.Duration(pending.RunMaxTimeSeconds)*time.Second <= task.DefaultMaxTime {
				t.Fatalf("invalid or shared startup owner: args=%+v task=%+v", args, pending)
			}
			shards[args.Shard] = true
		}
		for _, profile := range []WorkloadProfile{WorkloadProfileProduction, WorkloadProfileSubnetOperator} {
			if err := initTaskScheduleForProfile(ctx, profile); err != nil {
				t.Fatal(err)
			}
			if after := owners(); !reflect.DeepEqual(before, after) {
				t.Fatalf("profile %q changed retained tally owners", profile)
			}
		}
	})
}
