package taskworker

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/model"
)

// evs's two nightly jobs, which must be bound: the pass with its chain, and the closing job with none.
func evsNightlyJobs(t *testing.T) (*evsJob, *evsJob) {
	t.Helper()
	var nightly, closing *evsJob
	for _, job := range evsJobs {
		switch job.name {
		case evsservice.EvsNightlyTaskName:
			nightly = job
		case evsservice.EvsNightlyCloseTaskName:
			closing = job
		}
	}
	if nightly == nil || nightly.first == nil {
		t.Fatal("no evs.nightly job with a chain")
	}
	if closing == nil || closing.first != nil {
		t.Fatal("no evs.nightly_close job, or one with a chain")
	}
	return nightly, closing
}

// A fake host with two metadata clusters and evs's script twins.
func newEvsNightlyHost(t *testing.T) (*sidecartest.FakeClock, *sidecartest.FakeHost) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	settings := sidecartest.DefaultFakeHostSettings()
	settings.Blobs.MetadataClusterIds = []string{"meta-1", "meta-2"}
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, settings)
	sidecartest.RegisterEvsTwins(fake.Redis)
	return clock, fake
}

// What the start queued: each shard's run by cluster, and the night's closing job.
func evsNightlyQueued(t *testing.T, fake *sidecartest.FakeHost) (map[string]*evsservice.EvsNightlyShardArgs, *evsservice.EvsNightlyCloseArgs, *sidecar.TaskRequest) {
	t.Helper()
	shardArgs := map[string]*evsservice.EvsNightlyShardArgs{}
	var closeArgs *evsservice.EvsNightlyCloseArgs
	var closeRequest *sidecar.TaskRequest
	for _, request := range fake.Tasks.Pending() {
		switch {
		case request.Name == evsservice.EvsNightlyCloseTaskName:
			parsed, ok := evsservice.ParseEvsNightlyCloseArgs(request.Args)
			if !ok {
				t.Fatalf("a closing job with args %q", request.Args)
			}
			closeArgs, closeRequest = parsed, request
		case request.Name == evsservice.EvsNightlyTaskName && 0 < len(request.Args):
			parsed, ok := evsservice.ParseEvsNightlyArgs(request.Args)
			if !ok {
				t.Fatalf("a shard's run with args %q", request.Args)
			}
			shardArgs[parsed.ClusterId] = parsed
		}
	}
	return shardArgs, closeArgs, closeRequest
}

// evs's registry binds the nightly pass and its closing job: the pass's chain starts at each runtime's start, and
// the start queues itself a day on, one run per shard of each metadata cluster carrying the night, the start's time,
// and the night's closing job naming every shard; a shard's run on an empty cluster ends clean, and one for a
// cluster this process does not hold does not.
func TestEvsJobsBindTheNightlyPass(t *testing.T) {
	nightly, _ := evsNightlyJobs(t)
	clock, fake := newEvsNightlyHost(t)
	host := fake.ServiceHost()
	if request := nightly.first(host); request.OnceKey != evsservice.EvsNightlyTaskName || len(request.Args) != 0 {
		t.Fatalf("the chain starts under %q with args %q", request.OnceKey, request.Args)
	}
	if err := nightly.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	night := clock.Now().UnixMilli()
	nextStart := false
	for _, request := range fake.Tasks.Pending() {
		if request.Name == evsservice.EvsNightlyTaskName && len(request.Args) == 0 {
			nextStart = nextStart || request.RunAt.Equal(clock.Now().Add(24*time.Hour))
		}
	}
	shardArgs, closeArgs, closeRequest := evsNightlyQueued(t, fake)
	clusterIds := host.Blobs.MetadataClusterIds()
	if !nextStart || len(shardArgs) != len(clusterIds) {
		t.Fatalf("next start %t, shard runs %v of %v", nextStart, shardArgs, clusterIds)
	}
	for _, clusterId := range clusterIds {
		args := shardArgs[clusterId]
		if args.Shard != "" || args.StartAfter != "" || args.Night != night || args.Part != 0 || !args.Whole {
			t.Fatalf("%s's run: %+v", clusterId, args)
		}
	}
	if closeArgs == nil || closeArgs.Night != night || !slices.Equal(closeArgs.ShardFields, []string{"meta-1:", "meta-2:"}) ||
		!closeRequest.RunAt.Equal(clock.Now().Add(10*time.Minute)) {
		t.Fatalf("the closing job: %+v, %+v", closeArgs, closeRequest)
	}
	for _, clusterId := range clusterIds {
		if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs(shardArgs[clusterId])); err != nil {
			t.Fatalf("%s: %v", clusterId, err)
		}
	}
	elsewhere := *shardArgs["meta-1"]
	elsewhere.ClusterId = "no-such-cluster"
	if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs(&elsewhere)); err == nil {
		t.Fatal("a run for a cluster this process does not hold ended clean")
	}
}

// Replaces the closing job's seams over model with counts in memory: what each network holds, and each write in
// order.
func fakeEvsNightlyCounts(t *testing.T, counts map[sidecar.Id]int64) *[]sidecar.Id {
	writes := &[]sidecar.Id{}
	set, list := evsNightlySetStoredByteCount, evsNightlyNetworkIdsWithStoredByteCount
	evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
		counts[networkId] = storedByteCount
		*writes = append(*writes, networkId)
		return nil
	}
	evsNightlyNetworkIdsWithStoredByteCount = func(ctx context.Context, after *sidecar.Id, limit int) ([]sidecar.Id, error) {
		networkIds := []sidecar.Id{}
		for _, networkId := range slices.SortedFunc(maps.Keys(counts), func(a sidecar.Id, b sidecar.Id) int { return bytes.Compare(a[:], b[:]) }) {
			if counts[networkId] <= 0 || (after != nil && bytes.Compare(networkId[:], after[:]) <= 0) {
				continue
			}
			if len(networkIds) == limit {
				break
			}
			networkIds = append(networkIds, networkId)
		}
		return networkIds, nil
	}
	t.Cleanup(func() {
		evsNightlySetStoredByteCount, evsNightlyNetworkIdsWithStoredByteCount = set, list
	})
	return writes
}

// The stored count is an absolute write per network, so it is made only by the closing job, once every shard of
// every metadata cluster has reported: a shard's run writes none, a night missing one cluster's shard writes none
// and asks again, and a whole night writes each network's sum across both clusters once, and zero for a network
// holding a count that no shard summed. Run again it writes the same.
func TestEvsNightlyWritesStoredCountsOnlyFromAWholeRun(t *testing.T) {
	nightly, closing := evsNightlyJobs(t)
	_, fake := newEvsNightlyHost(t)
	host := fake.ServiceHost()
	networkA, networkB, networkGone := sidecar.Id{1}, sidecar.Id{2}, sidecar.Id{3}
	counts := map[sidecar.Id]int64{networkA: 7, networkGone: 50}
	writes := fakeEvsNightlyCounts(t, counts)

	if err := nightly.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	shardArgs, closeArgs, closeRequest := evsNightlyQueued(t, fake)
	if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs(shardArgs["meta-1"])); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 0 {
		t.Fatalf("a shard's run wrote %v", *writes)
	}

	// meta-2's shard has not reported
	requestCount := len(fake.Tasks.AllRequests())
	if err := closing.run(t.Context(), host, closeRequest.Args); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 0 || counts[networkA] != 7 || counts[networkGone] != 50 {
		t.Fatalf("a night missing a shard wrote %v", *writes)
	}
	asked := fake.Tasks.AllRequests()[requestCount:]
	if len(asked) != 1 || asked[0].Name != evsservice.EvsNightlyCloseTaskName || asked[0].OnceKey != closeRequest.OnceKey {
		t.Fatalf("asked again %+v", asked)
	}

	if err := nightly.run(t.Context(), host, evsservice.EvsNightlyArgs(shardArgs["meta-2"])); err != nil {
		t.Fatal(err)
	}
	if len(*writes) != 0 {
		t.Fatalf("a shard's run wrote %v", *writes)
	}
	// the clusters hold no repo, so sums stand in for what each shard counted, as its run reports them
	sums, err := evsservice.NewNightlySums(host.Redis, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sums.Report(t.Context(), closeArgs.Night, "meta-1", "", map[sidecar.Id]int64{networkA: 100, networkB: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sums.Report(t.Context(), closeArgs.Night, "meta-2", "", map[sidecar.Id]int64{networkA: 20}); err != nil {
		t.Fatal(err)
	}
	if err := closing.run(t.Context(), host, closeRequest.Args); err != nil {
		t.Fatal(err)
	}
	want := map[sidecar.Id]int64{networkA: 120, networkB: 1, networkGone: 0}
	if !maps.Equal(counts, want) || len(*writes) != 3 {
		t.Fatalf("counts %v after writes %v, want %v", counts, *writes, want)
	}

	// once more: the same sums, the same writes, and the zeroed network holds nothing left to zero
	if err := closing.run(t.Context(), host, closeRequest.Args); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(counts, want) || len(*writes) != 5 {
		t.Fatalf("run again: counts %v after writes %v", counts, *writes)
	}
}

// The closing job's count store turns a raise into the call's error, and a build without network_quota_state into
// evsservice.ErrNightlyCountsUnavailable, which ends the job with nothing written.
func TestEvsNightlyCountsReportRaisesAndAMissingTable(t *testing.T) {
	fakeEvsNightlyCounts(t, map[sidecar.Id]int64{})
	evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
		return model.ErrNetworkQuotaStateUnavailable
	}
	if err := (evsNightlyCounts{}).SetStoredByteCount(t.Context(), sidecar.Id{1}, 5); !errors.Is(err, evsservice.ErrNightlyCountsUnavailable) {
		t.Fatalf("no table: %v", err)
	}
	failure := errors.New("postgres is away")
	evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
		panic(failure)
	}
	if err := (evsNightlyCounts{}).SetStoredByteCount(t.Context(), sidecar.Id{1}, 5); !errors.Is(err, failure) {
		t.Fatalf("a raise: %v", err)
	}
	evsNightlyNetworkIdsWithStoredByteCount = func(ctx context.Context, after *sidecar.Id, limit int) ([]sidecar.Id, error) {
		panic(failure)
	}
	if networkIds, err := (evsNightlyCounts{}).NetworkIdsWithStoredByteCount(t.Context(), nil, 10); networkIds != nil || !errors.Is(err, failure) {
		t.Fatalf("a raise listing: %v %v", networkIds, err)
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
