package taskworker

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the sweep's one-off passes with no chain of its own: the turn-on's start queues one scan per
// metadata cluster, each scan of an empty cluster ends clean, and args of no run's shape fail.
func TestEvsJobsBindTheSweepPasses(t *testing.T) {
	var sweep *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsSweepTaskName {
			sweep = job
		}
	}
	if sweep == nil || sweep.first != nil {
		t.Fatal("no evs.sweep job, or one with a chain of its own")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	if err := sweep.run(t.Context(), host, evsservice.EvsSweepTurnOnTaskRequest().Args); err != nil {
		t.Fatal(err)
	}
	scans := 0
	for _, request := range fake.Tasks.AllRequests() {
		if request.Name != evsservice.EvsSweepTaskName || !strings.HasPrefix(string(request.Args), "scan\n") {
			continue
		}
		scans += 1
		if err := sweep.run(t.Context(), host, request.Args); err != nil {
			t.Fatalf("a scan of an empty cluster: %v", err)
		}
	}
	if scans != len(host.Blobs.MetadataClusterIds()) {
		t.Fatalf("%d scans for %d clusters", scans, len(host.Blobs.MetadataClusterIds()))
	}
	if err := sweep.run(t.Context(), host, []byte("no run")); !errors.Is(err, evsservice.ErrEvsSweepArgs) {
		t.Fatalf("args of no shape: %v", err)
	}
}
