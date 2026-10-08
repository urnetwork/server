package monitor

import (
	"strings"
	"testing"
)

func TestEgressCoverageParallelPoolModelsRequireArtifactJoin(t *testing.T) {
	geometry := egressCoverageGeometry{
		shardCount: 4, fullConcurrency: 8, fullLimit: 8,
		blackholeConcurrency: 250, blackholeTimeoutSeconds: 15,
	}
	finding, ok := egressBlackholeCapacityFinding("synthetic-fleet", geometry, []egressCoverageSnapshot{
		{eligible: 120, blackholeCurrent: 1, blackholeLastHour: 1},
	})
	if !ok || finding.class != "egress-blackhole-capacity" || finding.healthy {
		t.Fatal("conditional pool models changed measured capacity alert")
	}
	for _, want := range []string{
		"parallel_pool_model=unattested",
		"full_reserved_blackhole_concurrency_per_shard=242",
		"independent_pool_blackhole_concurrency_per_shard=250",
		"independent_pool_combined_concurrency_per_shard=258",
	} {
		if !strings.Contains(finding.observed, want) {
			t.Errorf("pool model missing %q", want)
		}
	}
	for _, want := range []string{"legacy shared-peak", "independent-pool", "running artifact"} {
		if !strings.Contains(finding.context, want) {
			t.Errorf("artifact qualifier missing %q", want)
		}
	}
}

func TestEgressCoverageParallelPoolModelsDoNotInventCapacityFailure(t *testing.T) {
	if _, ok := egressBlackholeCapacityFinding("synthetic-fleet", egressCoverageGeometry{
		shardCount: 4, fullConcurrency: 8, blackholeConcurrency: 250, blackholeTimeoutSeconds: 15,
	}, []egressCoverageSnapshot{{eligible: 120, blackholeCurrent: 120, blackholeLastHour: 120}}); ok {
		t.Fatal("independent-pool geometry overrode complete measured coverage")
	}
}
