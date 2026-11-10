package taskworker

// EVS1-M2-17's path-epoch copy among evs's jobs (evs_jobs.go): evs.epoch_copy, which evs's step 8 queues after a
// PATH_EPOCH commits, its args the repo, ref and epoch (evsservice.RunPathEpochCopy). A run makes at most the
// settings' CopyRunMoveCount moves and queues the rest as its own successor, so a 1M-entry ref's copy is many runs,
// each within the job's time. Each run's moves left feed §19.8's urnetwork_evs_path_epoch_moves_left{cluster}: this
// process's sum over the refs whose copies it ran last, which a ref's finished copy leaves at zero, so the series
// summed over processes is the cluster's copy liability.

import (
	"context"
	"sync"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

// §19.8's series name.
const evsPathEpochMovesLeftMetricName = "urnetwork_evs_path_epoch_moves_left"

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsEpochCopyTaskName, run: runEvsEpochCopy})
}

// This process's copy liability: each ref's moves left as its last run here left them, by cluster.
type evsCopyLiability struct {
	declareOnce sync.Once
	gauge       sidecar.MetricsGauge

	stateLock       sync.Mutex
	clusterRefMoves map[string]map[string]int64
}

// The process's, which every copy run reports to.
var evsCopyLiabilities = &evsCopyLiability{clusterRefMoves: map[string]map[string]int64{}}

// Keeps a ref's moves left on cluster and sets the cluster's series to the process's sum. metrics declares the series
// once a process; nil keeps the figure without a series.
func (self *evsCopyLiability) report(metrics sidecar.Metrics, clusterId string, refKey string, movesLeft int64) int64 {
	if metrics != nil {
		self.declareOnce.Do(func() {
			self.gauge = metrics.Gauge(evsPathEpochMovesLeftMetricName, "Path-epoch copy moves left, this process's refs.", "cluster")
		})
	}
	total := func() int64 {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		refMoves := self.clusterRefMoves[clusterId]
		if refMoves == nil {
			refMoves = map[string]int64{}
			self.clusterRefMoves[clusterId] = refMoves
		}
		if movesLeft <= 0 {
			delete(refMoves, refKey)
		} else {
			refMoves[refKey] = movesLeft
		}
		total := int64(0)
		for _, moves := range refMoves {
			total += moves
		}
		return total
	}()
	if self.gauge != nil {
		self.gauge.Set(float64(total), clusterId)
	}
	return total
}

// One copy run. Args that name no copy are dropped, said once: a retry could never succeed.
func runEvsEpochCopy(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	repoId, refId, _, err := evsservice.ParseEvsEpochCopyArgs(args)
	if err != nil {
		glog.Errorf("[evsjobs]%s with args that name no copy: dropped\n", evsservice.EvsEpochCopyTaskName)
		return nil
	}
	result, err := evsservice.RunPathEpochCopy(ctx, host, nil, args)
	// a run that failed before it counted its moves leaves the ref's last figure standing
	if result != nil && result.ClusterId != "" && (err == nil || 0 < result.MovesLeft) {
		evsCopyLiabilities.report(host.Metrics, result.ClusterId, sidecar.EvsKeyId(repoId)+":"+sidecar.EvsKeyId(refId), result.MovesLeft)
	}
	if err != nil {
		return err
	}
	glog.V(1).Infof("[evsjobs]copy moved %d, done %d, marked %d, missed %d, left %d, dropped %t\n",
		result.MovedCount, result.DoneCount, result.MarkedCount, result.MissedCount, result.MovesLeft, result.SourcesDropped)
	return nil
}
