// Production placement is an owner-level opt-in, not a task-package default
// or a shared mutable budget between callers.
package taskworker

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

// Loads a synthetic URL geometry through the production settings resolver.
// A zero instance concurrency keeps the built-in default.
func withProbePlacementGeometry(t testing.TB, shardCount int, concurrency int, instanceConcurrency int) {
	t.Helper()
	t.Setenv("WARP_DOMAIN", "probe-placement.example")
	resource := fmt.Sprintf("enabled: true\nshard_count: %d\nmax_time_seconds: 900\n"+
		"url_success_interval_seconds: 1200\nurl_failure_interval_seconds: 60\n"+
		"url_probe:\n  limit: %d\n  concurrency: %d\n  probe_timeout_seconds: 60\n",
		shardCount, concurrency, concurrency)
	if 0 < instanceConcurrency {
		resource += fmt.Sprintf("instance_url_concurrency: %d\n", instanceConcurrency)
	}
	t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressProbeResourceName, []byte(resource)))
}

// Probe placement is production-only; mirror repair is bounded in both
// profiles that can produce it. Other worker settings retain their values.
func TestProviderProbeClaimAdmissionIsProductionOnly(t *testing.T) {
	withProbePlacementGeometry(t, 8, 64, 0)
	settings := task.DefaultTaskWorkerSettings()
	production := taskWorkerSettingsForProfile(settings, WorkloadProfileProduction)
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	mirror := task.NewTaskTarget(model.ApplyLegacyNetEscrowMirror).TargetFunctionName()
	if !reflect.DeepEqual(production.TargetClaimLimits, map[string]int{name: 2, mirror: 1}) || settings.TargetClaimLimits != nil {
		t.Fatal("production target placement differs or mutated its caller", production.TargetClaimLimits)
	}
	production.TargetClaimLimits = nil
	totals := task.NewTaskTarget(model.ApplyLegacyProviderTotals).TargetFunctionName()
	if !reflect.DeepEqual(production.TargetClaimShares, map[string]int{totals: legacyProviderTotalsClaimSharePeriod}) || settings.TargetClaimShares != nil {
		t.Fatal("production provider totals share differs or mutated its caller")
	}
	production.TargetClaimShares = nil
	if !production.FairClaimFunctions || settings.FairClaimFunctions {
		t.Fatal("production fairness is missing or mutated caller settings")
	}
	production.FairClaimFunctions = settings.FairClaimFunctions
	if !reflect.DeepEqual(production, settings) {
		t.Fatal("probe placement changed ordinary worker settings")
	}
	operator := taskWorkerSettingsForProfile(settings, WorkloadProfileSubnetOperator)
	if !reflect.DeepEqual(operator.TargetClaimLimits, map[string]int{mirror: 1}) || !operator.ClaimRegisteredTargetsOnly {
		t.Fatal("independent profile did not retain mirror-only placement")
	}
	if !reflect.DeepEqual(operator.TargetClaimShares, map[string]int{totals: legacyProviderTotalsClaimSharePeriod}) {
		t.Fatal("independent profile lost the provider totals share")
	}
}

// Caller-supplied opt-ins survive without sharing their mutable map. A caller
// cannot accidentally override the production probe placement invariant.
func TestProviderProbeClaimAdmissionDoesNotBorrowCallerMap(t *testing.T) {
	withProbePlacementGeometry(t, 6, 96, 0)
	settings := task.DefaultTaskWorkerSettings()
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	settings.TargetClaimLimits = map[string]int{name: 7, "fixture.example/tasks.Other": 2}
	owner := taskWorkerSettingsForProfile(settings, WorkloadProfileProduction)
	if owner.TargetClaimLimits[name] != 1 || owner.TargetClaimLimits["fixture.example/tasks.Other"] != 2 || settings.TargetClaimLimits[name] != 7 {
		t.Fatal("production opt-in lost caller policy or mutated the caller")
	}
	settings.TargetClaimLimits["fixture.example/tasks.Other"] = 9
	if owner.TargetClaimLimits["fixture.example/tasks.Other"] != 2 {
		t.Fatal("worker placement borrowed a mutable caller map")
	}
}

// Main restored 8 shards of 64 URL workers on six taskworker instances. With
// one probe pass per instance only six shards could run, so a quarter of the
// configured workers never started and nothing reported it. Each instance must
// hold as many passes as fit in its URL concurrency, bounded by the shards.
func TestProductionProbeClaimLimitFollowsUrlGeometry(t *testing.T) {
	const liveInstanceCount = 6
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	cases := []struct {
		shardCount          int
		concurrency         int
		instanceConcurrency int
		want                int
	}{
		{shardCount: 8, concurrency: 64, want: 2},
		{shardCount: 6, concurrency: 96, want: 1},
		{shardCount: 4, concurrency: 8, want: 4},
		{shardCount: 16, concurrency: 32, want: 4},
		{shardCount: 8, concurrency: 64, instanceConcurrency: 256, want: 4},
		{shardCount: 8, concurrency: 64, instanceConcurrency: 64, want: 1},
		// a pass larger than the instance concurrency still runs alone
		{shardCount: 2, concurrency: 200, want: 1},
	}
	for _, c := range cases {
		withProbePlacementGeometry(t, c.shardCount, c.concurrency, c.instanceConcurrency)
		limit := taskWorkerSettingsForProfile(task.DefaultTaskWorkerSettings(), WorkloadProfileProduction).TargetClaimLimits[name]
		if limit != c.want {
			t.Errorf("%d shards x %d workers (instance concurrency %d): pass limit %d, want %d; %d of %d shards run on %d instances",
				c.shardCount, c.concurrency, c.instanceConcurrency, limit, c.want,
				min(c.shardCount, liveInstanceCount*limit), c.shardCount, liveInstanceCount)
		}
	}
}

// Missing or invalid settings cannot widen placement: the worker keeps the
// one-pass limit it had before placement followed the geometry.
func TestProductionProbeClaimLimitKeepsOnePassWithoutValidSettings(t *testing.T) {
	name := task.NewTaskTarget(work.ProviderEgressProbe).TargetFunctionName()
	withProbePlacementGeometry(t, 8, 64, 0)
	t.Cleanup(server.Config.PushSimpleResource(model.ProviderEgressProbeResourceName, []byte(
		"enabled: true\nshard_count: 8\nurl_probe:\n  limit: 64\n  concurrency: 64\n  probe_timeout_seconds: 60\ninstance_url_concurrency: -1\n")))
	if limit := taskWorkerSettingsForProfile(task.DefaultTaskWorkerSettings(), WorkloadProfileProduction).TargetClaimLimits[name]; limit != 1 {
		t.Fatalf("invalid instance concurrency widened placement to %d passes", limit)
	}
	t.Setenv("WARP_DOMAIN", "")
	if limit := taskWorkerSettingsForProfile(task.DefaultTaskWorkerSettings(), WorkloadProfileProduction).TargetClaimLimits[name]; limit != 1 {
		t.Fatalf("unreadable settings widened placement to %d passes", limit)
	}
}
