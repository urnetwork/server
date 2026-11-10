package taskworker

import (
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the copy, which drops args naming no copy rather than retry them forever.
func TestEvsJobsBindTheEpochCopy(t *testing.T) {
	var copyJob *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsEpochCopyTaskName {
			copyJob = job
		}
	}
	if copyJob == nil || copyJob.first != nil {
		t.Fatalf("copy job %v", copyJob)
	}
	fake := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil)
	if err := copyJob.run(t.Context(), fake.ServiceHost(), []byte("no copy")); err != nil {
		t.Fatalf("args naming no copy: %v", err)
	}
}

// §19.8's copy liability: the process's sum over refs by cluster, a finished ref dropping out.
func TestEvsCopyLiabilitySumsRefsByCluster(t *testing.T) {
	liability := &evsCopyLiability{clusterRefMoves: map[string]map[string]int64{}}
	metrics := sidecartest.NewFakeMetrics(nil)
	if total := liability.report(metrics, "meta-1", "repo:a", 10); total != 10 {
		t.Fatalf("%d", total)
	}
	if total := liability.report(metrics, "meta-1", "repo:b", 5); total != 15 {
		t.Fatalf("%d", total)
	}
	if total := liability.report(metrics, "meta-2", "repo:c", 7); total != 7 {
		t.Fatalf("%d", total)
	}
	if total := liability.report(metrics, "meta-1", "repo:a", 0); total != 5 {
		t.Fatalf("a finished ref stays: %d", total)
	}
}
