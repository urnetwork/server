package taskworker

// §19.10's restore frame's job in taskworker (OPS-M3-03, QD-813): sidecar_restore_orphan_log, which the restore's ops
// step queues for each orphaned log for 7 d 10 min after the incident, and which then moves the log's repos to
// shredding under the incident, queues their shreds and deletes the log's proj/ and projrows/ objects once the last
// of them is shredded (sidecarrestore/orphan_logs.go). It runs on the process's evs host, built from the same
// file-clusters-taskworker.yml the ops step reads, which reaches repo/ on every metadata cluster and proj/ on the
// first, and issues its writes as P0 on the host's write budget; a process with no host retries it on server/task's
// backoff, as evs's jobs do.

import (
	"context"
	"errors"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecarrestore"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: sidecarrestore.OrphanLogJobName, run: runSidecarRestoreOrphanLog})
}

// One run of the job on host. Args that name no orphaned log are dropped, said once: a retry could never succeed,
// and the run touched nothing.
func runSidecarRestoreOrphanLog(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	err := sidecarrestore.RunOrphanLogJob(ctx, &sidecarrestore.OrphanLogDeps{
		Blobs:  host.Blobs,
		Tasks:  host.Tasks,
		Budget: host.Budget,
		Now:    host.ClockOrReal().Now,
	}, args)
	if errors.Is(err, sidecarrestore.ErrOrphanLogJobArgs) {
		glog.Errorf("[restore]%s with args that name no orphaned log, dropped: %v\n", sidecarrestore.OrphanLogJobName, err)
		return nil
	}
	return err
}
