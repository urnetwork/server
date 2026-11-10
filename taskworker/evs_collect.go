package taskworker

// EVSB-M2-14's marker collection among evs's jobs (evs_jobs.go; §11.7's collect row), a sidecar job on the process's
// evs host. evs.collect has no chain of its own: the nightly pass queues one run per ref whose head the fold has
// passed by a base range since its last window, once-keyed by the ref, and each run opens at most one window on that
// ref (evsservice.MarkerCollector). A run that could not finish returns its error, so server/task's backoff reruns
// it; a window it opened and did not close lapses by itself, and the next run reads every marker again.

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: evsservice.EvsCollectTaskName, run: runEvsCollect})
}

// One ref's collection.
func runEvsCollect(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	result, err := evsservice.RunMarkerCollection(ctx, host, &evsservice.MarkerCollectorDeps{Log: evsBudgetLogger{}}, args)
	if result != nil && glog.V(1) {
		glog.Infof("[evsjobs]marker collection: %d versions read, %d removals, %d markers deleted, %d left, done %d, opened %t, closed %t, skipped %q\n",
			result.ScannedVersionCount, result.RemovalCount, result.MarkerDeletedCount, result.MarkerLeftCount, result.Done,
			result.Opened, result.Closed, result.Skipped)
	}
	return err
}
