package taskworker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the nightly pass: its chain starts at each runtime's start, the start queues itself a day on
// and one run per shard of each metadata cluster, and a shard's run on an empty cluster ends clean.
func TestEvsJobsBindTheNightlyPass(t *testing.T) {
	var nightly *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsNightlyTaskName {
			nightly = job
		}
	}
	if nightly == nil || nightly.first == nil {
		t.Fatal("no evs.nightly job with a chain")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	if request := nightly.first(host); request.OnceKey != evsservice.EvsNightlyTaskName || len(request.Args) != 0 {
		t.Fatalf("the chain starts under %q with args %q", request.OnceKey, request.Args)
	}
	if err := nightly.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	clusterIds := host.Blobs.MetadataClusterIds()
	nextStart := false
	shardRuns := map[string]bool{}
	for _, request := range fake.Tasks.AllRequests() {
		if request.Name != evsservice.EvsNightlyTaskName {
			continue
		}
		if len(request.Args) == 0 {
			nextStart = nextStart || request.RunAt.Equal(clock.Now().Add(24*time.Hour))
			continue
		}
		clusterId, shard, startAfter, ok := evsservice.ParseEvsNightlyArgs(request.Args)
		if !ok || shard != "" || startAfter != "" {
			t.Fatalf("a shard's run with args %q", request.Args)
		}
		shardRuns[clusterId] = true
	}
	if !nextStart || len(shardRuns) != len(clusterIds) {
		t.Fatalf("next start %t, shard runs %v of %v", nextStart, shardRuns, clusterIds)
	}
	for _, clusterId := range clusterIds {
		if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs(clusterId, "", "")); err != nil {
			t.Fatalf("%s: %v", clusterId, err)
		}
	}
	if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs("no-such-cluster", "", "")); err == nil {
		t.Fatal("a run for a cluster this process does not hold ended clean")
	}
}

// The stored count is an absolute write per network, so it is made only from a run whose sums are every repo's:
// one metadata cluster, its one shard, listed whole. A sharded, resumed, partial or second-cluster run writes none.
func TestEvsNightlyWritesStoredCountsOnlyFromAWholeRun(t *testing.T) {
	networkA, networkB := sidecar.Id{1}, sidecar.Id{2}
	result := func() *evsservice.NightlyResult {
		return &evsservice.NightlyResult{
			ClusterId: "meta-1",
			Complete:  true,
			NetworkBytes: map[sidecar.Id]*evsservice.NightlyBytes{
				networkA: {SegmentByteCount: 100, PackByteCount: 20, ParcelByteCount: 3, BatchRecordByteCount: 4},
				networkB: {SegmentByteCount: 7},
			},
		}
	}
	written := map[sidecar.Id]int64{}
	set := evsNightlySetStoredByteCount
	evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
		written[networkId] = storedByteCount
		return nil
	}
	t.Cleanup(func() { evsNightlySetStoredByteCount = set })

	setCount, err := evsNightlySetStoredCounts(t.Context(), []string{"meta-1"}, result())
	if err != nil || setCount != 2 || written[networkA] != 127 || written[networkB] != 7 {
		t.Fatalf("a whole run wrote %v (%d, %v)", written, setCount, err)
	}

	partial := map[string]func(result *evsservice.NightlyResult) []string{
		"a shard": func(result *evsservice.NightlyResult) []string {
			result.Shard = "a"
			return []string{"meta-1"}
		},
		"a run that stopped at its bound": func(result *evsservice.NightlyResult) []string {
			result.More, result.Complete = true, false
			return []string{"meta-1"}
		},
		"a run with a read that failed": func(result *evsservice.NightlyResult) []string {
			result.Complete = false
			return []string{"meta-1"}
		},
		"one of two metadata clusters": func(result *evsservice.NightlyResult) []string {
			return []string{"meta-1", "meta-2"}
		},
	}
	for name, change := range partial {
		clear(written)
		run := result()
		metadataClusterIds := change(run)
		setCount, err := evsNightlySetStoredCounts(t.Context(), metadataClusterIds, run)
		if err != nil || setCount != 0 || len(written) != 0 {
			t.Fatalf("%s wrote %v (%d, %v)", name, written, setCount, err)
		}
	}

	// a write that fails is the run's error, and the rest are still tried
	failure := errors.New("postgres is away")
	evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
		if networkId == networkA {
			return failure
		}
		written[networkId] = storedByteCount
		return nil
	}
	clear(written)
	setCount, err = evsNightlySetStoredCounts(t.Context(), []string{"meta-1"}, result())
	if !errors.Is(err, failure) || setCount != 1 || written[networkB] != 7 {
		t.Fatalf("a failed write: %v (%d, %v)", written, setCount, err)
	}
}

// evs's registry binds marker collection with no chain of its own, the nightly pass queueing it, and a run whose
// args name no ref is refused.
func TestEvsJobsBindMarkerCollection(t *testing.T) {
	var collect *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsCollectTaskName {
			collect = job
		}
	}
	if collect == nil || collect.first != nil {
		t.Fatal("no evs.collect job, or one with a chain")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	if err := collect.run(t.Context(), fake.ServiceHost(), []byte("meta-1")); err == nil {
		t.Fatal("a run naming no ref was taken")
	}
}
