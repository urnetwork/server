package taskworker

// EVSB-M2-12's nightly pass among evs's jobs (evs_jobs.go; §11.13's bytes and orphans rows, §20.7's count row), a
// sidecar job on the process's evs host:
//
//   - evs.nightly with no args is the nightly start: it queues itself a day on, then one run per shard of each
//     metadata cluster the process holds, each once-keyed by its cluster and shard.
//   - evs.nightly with a shard's args is that shard's run (evsservice.NightlyPass): the cluster's evacuation
//     horizon first, then one LIST of each of its repos' prefixes, the stored bytes they give by network, and
//     orphan GC on them. A run that stops at its bound queues itself from the key it reached.
//
// The stored count (§20.7's count row) is written here, by model.SetNetworkStoredByteCount, since Quota keeps
// §20.7's five methods (CONTRACTS.md). It is an absolute write per network, so it is made only from a run that
// counted every repo the process can see: one metadata cluster, one shard, the whole of it with no read failing.
// A network's repos fall in every shard and on every metadata cluster, and nothing yet carries one shard's sums to
// the next (questions/evsb-m2-23-nightly-count-across-shards), so a sharded or partial run writes no count and
// says so; the count then moves by usage batches alone (§20.6), as it does between passes.
//
// A run that could not read everything returns its first error, so server/task's backoff reruns it.

import (
	"context"
	"errors"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsNightlyTaskName, run: runEvsNightly, first: firstEvsNightly})
}

// The nightly start's chain, from the runtime's start.
func firstEvsNightly(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return evsservice.EvsNightlyStartTaskRequest(host.ClockOrReal().Now())
}

// Sets one network's stored count. A test's seam over model.SetNetworkStoredByteCount.
var evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
	return model.SetNetworkStoredByteCount(ctx, server.Id(networkId), storedByteCount)
}

// Whether a run's sums are every repo's the process can see, so that each network's is its stored count: the one
// shard of the one metadata cluster, listed whole with no read failing.
func evsNightlyCountIsWhole(metadataClusterIds []string, result *evsservice.NightlyResult) bool {
	return result != nil && len(metadataClusterIds) == 1 && result.ClusterId == metadataClusterIds[0] &&
		result.Shard == "" && result.Complete && !result.More
}

// Writes the stored count of each network the run counted, when the run's sums stand for the whole. The count of
// how many it wrote, and the first error.
func evsNightlySetStoredCounts(ctx context.Context, metadataClusterIds []string, result *evsservice.NightlyResult) (int, error) {
	if !evsNightlyCountIsWhole(metadataClusterIds, result) {
		return 0, nil
	}
	setCount := 0
	var firstErr error
	for networkId, stored := range result.NetworkBytes {
		err := evsNightlySetStoredByteCount(ctx, networkId, stored.StoredByteCount())
		switch {
		case err == nil:
			setCount += 1
		case errors.Is(err, model.ErrNetworkQuotaStateUnavailable):
			// the quota tables are not linked on this build: nothing to write to
			return setCount, nil
		case firstErr == nil:
			firstErr = err
		}
	}
	return setCount, firstErr
}

// One run: the nightly start, or one shard's pass.
func runEvsNightly(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunNightlyPass(ctx, host, &evsservice.NightlyPassDeps{Log: evsBudgetLogger{}}, args)
	if result == nil {
		return err
	}
	metadataClusterIds := host.Blobs.MetadataClusterIds()
	setCount, setErr := evsNightlySetStoredCounts(ctx, metadataClusterIds, result)
	if err == nil {
		err = setErr
	}
	if glog.V(1) {
		glog.Infof("[evsjobs]nightly pass of %s shard %q: %d repos, %d networks counted, %d stored counts written (whole %t); orphans: %d candidates, %d deleted, %d replaced, %d held, %d failed, %d map parts; %d repos skipped\n",
			result.ClusterId, result.Shard, result.RepoCount, len(result.NetworkBytes), setCount, evsNightlyCountIsWhole(metadataClusterIds, result),
			result.Orphans.CandidateCount, result.Orphans.DeletedCount, result.Orphans.ReplacedCount, result.Orphans.HeldCount,
			result.Orphans.FailedCount, result.Orphans.MapPartDeletedCount, result.OrphanSkippedRepoCount)
	}
	return err
}
