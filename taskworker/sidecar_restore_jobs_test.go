package taskworker

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server/sidecarrestore"
)

// evs's registry binds the restore frame's orphan log job, which drops args naming no orphaned log rather than retry
// them forever, touching nothing and queueing nothing.
func TestEvsJobsBindTheOrphanLogJobDroppingArgsThatNameNoLog(t *testing.T) {
	var job *evsJob
	for _, candidate := range evsJobs {
		if candidate.name == sidecarrestore.OrphanLogJobName {
			job = candidate
		}
	}
	if job == nil || job.first != nil {
		t.Fatalf("job %v", job)
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	for _, args := range [][]byte{[]byte("no args"), []byte(`{"incident_id":null}`)} {
		if err := job.run(t.Context(), fake.ServiceHost(), args); err != nil {
			t.Fatalf("args %q: %v", args, err)
		}
	}
	if requests := fake.Tasks.AllRequests(); len(requests) != 0 {
		t.Fatalf("queued %v", requests)
	}
	if dump := fake.Blobs.Dump(); len(dump) != 0 {
		t.Fatalf("wrote %d objects", len(dump))
	}
}
