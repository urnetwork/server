package taskworker

// §19.10's restore frame's job in taskworker (OPS-M3-03): sidecar_restore_orphan_log, which the restore's ops step
// queues for each orphaned log whose repos it marked deleted, and which deletes the log's proj/ and projrows/ objects
// once the last of those repos is shredded (sidecarrestore/orphan_logs.go). It runs on the process's evs host, built
// from the same file-clusters-taskworker.yml the ops step reads, which reaches repo/ on every metadata cluster and
// proj/ on the first; a process with no host retries it on server/task's backoff, as evs's jobs do.

import (
	"context"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/sidecarrestore"
)

func init() {
	evsJobs = append(evsJobs, &evsJob{name: sidecarrestore.OrphanLogJobName, run: runSidecarRestoreOrphanLog})
}

// One run of the job on host.
func runSidecarRestoreOrphanLog(ctx context.Context, host *sidecar.ServiceHost, args []byte) error {
	return sidecarrestore.RunOrphanLogJob(ctx, &sidecarrestore.OrphanLogDeps{
		Blobs: host.Blobs,
		Tasks: host.Tasks,
		Now:   host.ClockOrReal().Now,
	}, args)
}
