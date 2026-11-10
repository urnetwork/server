package taskworker

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// evs's registry binds the hourly evacuation: its chain starts at each runtime's start, the start queues itself an
// hour on and one run per metadata cluster, and a cluster's run on an empty cluster ends clean.
func TestEvsJobsBindTheHourlyEvacuation(t *testing.T) {
	var evacuate *evsJob
	for _, job := range evsJobs {
		if job.name == evsservice.EvsEvacuateTaskName {
			evacuate = job
		}
	}
	if evacuate == nil || evacuate.first == nil {
		t.Fatal("no evs.evacuate job with a chain")
	}
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	sidecartest.RegisterEvsTwins(fake.Redis)
	host := fake.ServiceHost()
	if request := evacuate.first(host); request.OnceKey != evsservice.EvsEvacuateTaskName || len(request.Args) != 0 {
		t.Fatalf("the chain starts under %q with args %q", request.OnceKey, request.Args)
	}
	if err := evacuate.run(t.Context(), host, nil); err != nil {
		t.Fatal(err)
	}
	clusterIds := host.Blobs.MetadataClusterIds()
	nextStart := false
	clusterRuns := map[string]bool{}
	for _, request := range fake.Tasks.AllRequests() {
		if request.Name != evsservice.EvsEvacuateTaskName {
			continue
		}
		if len(request.Args) == 0 {
			nextStart = nextStart || request.RunAt.Equal(clock.Now().Add(time.Hour))
			continue
		}
		clusterRuns[string(request.Args)] = true
	}
	if !nextStart || len(clusterRuns) != len(clusterIds) {
		t.Fatalf("next start %t, cluster runs %v of %v", nextStart, clusterRuns, clusterIds)
	}
	for _, clusterId := range clusterIds {
		if !clusterRuns[clusterId] {
			t.Fatalf("no run queued for %s", clusterId)
		}
		if err := evacuate.run(t.Context(), host, []byte(clusterId)); err != nil {
			t.Fatalf("%s: %v", clusterId, err)
		}
	}
	if err := evacuate.run(t.Context(), host, []byte("no-such-cluster")); err == nil {
		t.Fatal("a run for a cluster this process does not hold ended clean")
	}
}

// A cluster's run's usage producer reports a staged delta under producer taskworker through the host's Quota, sent by
// the time the run's finish returns; a host with no Quota has none, and the evacuator alarms instead.
func TestEvsEvacuationReportsStagedDeltasUnderTaskworker(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	fake := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil)
	host := fake.ServiceHost()
	usage, finish, err := evsEvacuateUsage(t.Context(), host)
	if err != nil || usage == nil {
		t.Fatalf("no producer over a host with a Quota: %v", err)
	}
	networkId := connect.Id(sidecar.Id{0x5e})
	if err := usage.AddStoredDelta(networkId, -700); err != nil {
		t.Fatal(err)
	}
	finish()
	batches := fake.Quota.Batches()
	if len(batches) != 1 || batches[0].Producer != sidecar.QuotaProducerTaskworker || len(batches[0].Events) != 1 {
		t.Fatalf("batches %+v", batches)
	}
	if event := batches[0].Events[0]; event.NetworkId != networkId || event.StoredByteDelta != -700 || event.TransferByteCount != 0 {
		t.Fatalf("event %+v", event)
	}

	host.Quota = nil
	usage, finish, err = evsEvacuateUsage(t.Context(), host)
	if err != nil || usage != nil {
		t.Fatalf("a producer with no Quota: %v", err)
	}
	finish()
}
