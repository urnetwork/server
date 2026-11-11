package taskworker

// EVSB-M2-12's nightly pass and EVSB-M2-26's closing job among evs's jobs (evs_jobs.go; §11.13's bytes and orphans
// rows, §20.7's count row; owner decision 198), sidecar jobs on the process's evs host:
//
//   - evs.nightly with no args is the nightly start: it queues itself a day on, then one run per shard of each
//     metadata cluster the process holds, each once-keyed by its cluster and shard and carrying the night, the
//     start's time, and last the night's closing job, once-keyed by the night.
//   - evs.nightly with a shard's args is that shard's run (evsservice.NightlyPass): the cluster's evacuation
//     horizon first, then one LIST of each of its repos' prefixes, the stored bytes they give by network, and
//     orphan GC on them. A run that stops at its bound queues itself from the key it reached. The run that lists
//     the shard to its end, every run before it having counted with no read failing, replaces the shard's sums for
//     the night in Redis {evsn_<night>} (evsservice.NightlySums).
//   - evs.nightly_close is the night's closing job (evsservice.RunNightlyClose): once every shard of every
//     metadata cluster has reported, it writes each network's sum, and zero for each network holding a count that
//     no sum names. A night some shard never finished within 48 h, or whose sums Redis lost, writes nothing, and
//     the old counts stand, moved between nights by usage batches alone (§20.6).
//
// The stored count is written by the closing job and nowhere else, through model.SetNetworkStoredByteCount, since
// Quota keeps §20.7's five methods (CONTRACTS.md). A shard's run writes no count.
//
// A run that could not read or write everything returns its first error, so server/task's backoff reruns it.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func init() {
	evsJobs = append(evsJobs,
		&evsJob{name: evsservice.EvsNightlyTaskName, run: runEvsNightly, first: firstEvsNightly},
		&evsJob{name: evsservice.EvsNightlyCloseTaskName, run: runEvsNightlyClose},
	)
}

// The nightly start's chain, from the runtime's start.
func firstEvsNightly(host *sidecar.ServiceHost) *sidecar.TaskRequest {
	return evsservice.EvsNightlyStartTaskRequest(host.ClockOrReal().Now())
}

// Sets one network's stored count. A test's seam over model.SetNetworkStoredByteCount.
var evsNightlySetStoredByteCount = func(ctx context.Context, networkId sidecar.Id, storedByteCount int64) error {
	return model.SetNetworkStoredByteCount(ctx, server.Id(networkId), storedByteCount)
}

// A page of the networks holding a count. A test's seam over model.NetworkIdsWithStoredByteCount.
var evsNightlyNetworkIdsWithStoredByteCount = func(ctx context.Context, after *sidecar.Id, limit int) ([]sidecar.Id, error) {
	var modelAfter *server.Id
	if after != nil {
		afterId := server.Id(*after)
		modelAfter = &afterId
	}
	modelNetworkIds, err := model.NetworkIdsWithStoredByteCount(ctx, modelAfter, limit)
	if err != nil {
		return nil, err
	}
	networkIds := make([]sidecar.Id, 0, len(modelNetworkIds))
	for _, networkId := range modelNetworkIds {
		networkIds = append(networkIds, sidecar.Id(networkId))
	}
	return networkIds, nil
}

// The closing job's count store (evsservice.NightlyCountStore): network_quota_state through model, a raise turned
// into the call's error, and a build that does not link the table answering evsservice.ErrNightlyCountsUnavailable,
// so the job ends with nothing to write to.
type evsNightlyCounts struct{}

func (self evsNightlyCounts) SetStoredByteCount(ctx context.Context, networkId sidecar.Id, storedByteCount int64) (returnErr error) {
	server.HandleError(func() {
		returnErr = evsNightlySetStoredByteCount(ctx, networkId, storedByteCount)
	}, func(err error) {
		returnErr = err
	})
	return evsNightlyCountsError(returnErr)
}

func (self evsNightlyCounts) NetworkIdsWithStoredByteCount(ctx context.Context, after *sidecar.Id, limit int) (networkIds []sidecar.Id, returnErr error) {
	server.HandleError(func() {
		networkIds, returnErr = evsNightlyNetworkIdsWithStoredByteCount(ctx, after, limit)
	}, func(err error) {
		networkIds, returnErr = nil, err
	})
	return networkIds, evsNightlyCountsError(returnErr)
}

// model's refusal on a build without MG2, as the closing job reads it.
func evsNightlyCountsError(err error) error {
	if errors.Is(err, model.ErrNetworkQuotaStateUnavailable) {
		return fmt.Errorf("%w: %w", evsservice.ErrNightlyCountsUnavailable, err)
	}
	return err
}

// One run: the nightly start, or one shard's pass.
func runEvsNightly(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunNightlyPass(ctx, host, &evsservice.NightlyPassDeps{Log: evsBudgetLogger{}}, args)
	if result == nil {
		return err
	}
	if glog.V(1) {
		glog.Infof("[evsjobs]nightly pass of %s shard %q night %d: %d repos, %d networks counted, reported %t; orphans: %d candidates, %d deleted, %d replaced, %d held, %d failed, %d map parts; %d repos skipped\n",
			result.ClusterId, result.Shard, result.Night, result.RepoCount, len(result.NetworkBytes), result.Reported,
			result.Orphans.CandidateCount, result.Orphans.DeletedCount, result.Orphans.ReplacedCount, result.Orphans.HeldCount,
			result.Orphans.FailedCount, result.Orphans.MapPartDeletedCount, result.OrphanSkippedRepoCount)
	}
	return err
}

// One run of a night's closing job.
func runEvsNightlyClose(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunNightlyClose(ctx, host, &evsservice.NightlyCloseDeps{Counts: evsNightlyCounts{}, Log: evsBudgetLogger{}}, args)
	if result != nil && glog.V(1) {
		glog.Infof("[evsjobs]nightly close of night %d: %d shards, %d missing, waiting %t, unfinished %t, lost %t, counts unavailable %t; %d counts written, %d written zero\n",
			result.Night, result.ShardCount, len(result.MissingShardFields), result.Waiting, result.Unfinished, result.Lost,
			result.CountsUnavailable, result.SetCount, result.ZeroCount)
	}
	return err
}
