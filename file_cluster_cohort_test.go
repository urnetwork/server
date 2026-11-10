package server

// FS-119's composed path on T2 (EVSB-M3-02; §19.9's cohorts row, D-OPS-22): two metadata clusters, each read
// through FileClusterCounters from its storage hosts' exporters and its admin info, each with its own budget loop,
// and one evs process's write budget over both. The first cluster's cohort grows to 80% raw while place_new names
// the second, mostly empty: the first's loop sets its at_limit flag, the process's ClusterLimit reads it from the
// first's allowance alone, so a network with repos on both clusters is held to the storage limit only on the
// first, and the second's P1 admissions go on. The route's 507s and passes are evsservice's cluster_limit_test.go,
// and the earliest exhaustion monitor's signal_filecluster_cohort_test.go; the p99 and shedding clauses under
// load, and the run on T3's stores, are the review phase's.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"
)

// Each cluster's admin info as a test sets it, behind the counters' seam.
type fileClusterCohortTestInfos struct {
	stateLock      sync.Mutex
	clusterIdInfos map[string]*FileClusterAdminInfo
}

func (self *fileClusterCohortTestInfos) get(clusterId string) *FileClusterAdminInfo {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	info, ok := self.clusterIdInfos[clusterId]
	if !ok {
		return nil
	}
	copied := *info
	return &copied
}

// Sets the cluster's raw use, in percent of its capacity.
func (self *fileClusterCohortTestInfos) setRawPercent(clusterId string, percent uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	info := self.clusterIdInfos[clusterId]
	info.RawCapacityByteCount = 100 * 1024 * 1024
	info.RawUsageByteCount = percent * 1024 * 1024
	info.UsedInodeCount = 1000
	info.FreeInodeCount = 99000
}

// The counters as the loop reads them, as taskworker's evsDriveCounterSource adapts them, since server's root links
// no engine outside its tests.
type fileClusterCohortTestSource struct {
	counters *FileClusterCounters
}

func (self *fileClusterCohortTestSource) ReadDriveCounters(ctx context.Context, clusterId string) (*evsservice.DriveCounterReading, error) {
	reading, err := self.counters.ReadDriveCounters(ctx, clusterId)
	if err != nil {
		return nil, err
	}
	converted := &evsservice.DriveCounterReading{
		UsageKnown:           true,
		RawUsageByteCount:    reading.RawUsageByteCount,
		RawCapacityByteCount: reading.RawCapacityByteCount,
		UsedInodeCount:       reading.UsedInodeCount,
		FreeInodeCount:       reading.FreeInodeCount,
	}
	for _, drive := range reading.Drives {
		converted.Drives = append(converted.Drives, &evsservice.DriveCounter{
			DriveId:          drive.DriveId,
			WrittenByteCount: drive.WrittenByteCount,
			WritesCompleted:  drive.WritesCompleted,
			ReadsCompleted:   drive.ReadsCompleted,
			Healing:          drive.Healing,
			Missing:          drive.Missing,
		})
	}
	return converted, nil
}

// One cluster's hosts and drives as admin info lists them, each host's exporter a test server.
func newFileClusterCohortTestCluster(t *testing.T, clusterId string, hostUrls map[string]string) *FileClusterAdminInfo {
	t.Helper()
	hostNames := []string{clusterId + "-store-a.example", clusterId + "-store-b.example"}
	for _, hostName := range hostNames {
		text := fileClusterExporterText(2, 0)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/metrics" {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			fmt.Fprint(writer, text)
		}))
		t.Cleanup(server.Close)
		hostUrls[hostName] = server.URL + "/metrics"
	}
	return fileClusterCountersAdminInfo(hostNames, -1)
}

func TestFs119CohortLimitHoldsOnlyTheFullCluster(t *testing.T) {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC))
	hostSettings := sidecartest.DefaultFakeHostSettings()
	hostSettings.Blobs.MetadataClusterIds = []string{"meta-1", "meta-2"}
	host := sidecartest.NewFakeHost(ctx, clock, nil, nil, hostSettings)
	sidecartest.RegisterEvsTwins(host.Redis)
	// place_new names the second cluster: new repos go there, and the first keeps its cohort
	host.Blobs.SetNewRepoClusterId("meta-2")

	hostUrls := map[string]string{}
	infos := &fileClusterCohortTestInfos{clusterIdInfos: map[string]*FileClusterAdminInfo{
		"meta-1": newFileClusterCohortTestCluster(t, "meta-1", hostUrls),
		"meta-2": newFileClusterCohortTestCluster(t, "meta-2", hostUrls),
	}}
	infos.setRawPercent("meta-1", 79)
	infos.setRawPercent("meta-2", 3)
	counters := newFileClusterCounters(DefaultFileClusterCountersSettings(), &http.Client{}, infos.get, func(hostName string) string {
		return hostUrls[hostName]
	})

	loopSettings := evsservice.DefaultBudgetLoopSettings()
	loops := map[string]*evsservice.BudgetLoop{}
	for _, clusterId := range []string{"meta-1", "meta-2"} {
		loop, err := evsservice.NewBudgetLoop(host.ServiceHost(), clusterId, &fileClusterCohortTestSource{counters: counters}, nil, loopSettings)
		if err != nil {
			t.Fatal(err)
		}
		loops[clusterId] = loop
	}
	stepLoops := func() {
		t.Helper()
		for clusterId, loop := range loops {
			if allowance := loop.Step(ctx); allowance == nil {
				t.Fatalf("%s's loop published nothing", clusterId)
			}
		}
	}
	stepLoops()

	// one evs process's budget over both clusters, its first tick at once; it then parks on its accounts' timer
	start := clock.Now()
	budget, err := evsservice.NewWriteBudget(ctx, host.ServiceHost(), []string{"meta-1", "meta-2"}, loopSettings.Budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(budget.Close)
	awaitTimerAt := func(at time.Time) {
		t.Helper()
		for {
			pending, added := clock.WatchTimers()
			for _, pendingAt := range pending {
				if pendingAt.Equal(at) {
					return
				}
			}
			select {
			case <-added:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	awaitTimerAt(start.Add(loopSettings.Budget.Interval))
	limit, err := evsservice.NewClusterLimit(budget, nil)
	if err != nil {
		t.Fatal(err)
	}
	if limit.Full("meta-1") || limit.Full("meta-2") {
		t.Fatal("a cluster under 80% raw reads at its cohort limit")
	}

	// the first cluster's cohort reaches 80% raw: its loop sets at_limit, and the process reads it at its next tick,
	// which has read every allowance by the time it sets its next timer
	infos.setRawPercent("meta-1", 80)
	clock.Advance(time.Second)
	stepLoops()
	clock.AdvanceTo(start.Add(loopSettings.Budget.Interval))
	awaitTimerAt(start.Add(2 * loopSettings.Budget.Interval))
	if !limit.Full("meta-1") {
		t.Fatal("the first cluster at 80% raw does not read at its cohort limit")
	}
	if limit.Full("meta-2") {
		t.Fatal("the second cluster, mostly empty, reads at its cohort limit: the estate's flag, not the cluster's")
	}

	// a network with repos on both: the storage limit's classification holds on the first alone
	payload := &evsservice.QuotaPush{NetworkId: connect.NewId(), NewSegmentCount: 1}
	if !(limit.Full("meta-1") && evsservice.QuotaRefusesAtLimit(payload)) {
		t.Fatal("a payload push to the full cluster passes")
	}
	if limit.Full("meta-2") && evsservice.QuotaRefusesAtLimit(payload) {
		t.Fatal("a payload push to the cluster under its limit is refused")
	}
	deletion := &evsservice.QuotaPush{NetworkId: payload.NetworkId}
	if evsservice.QuotaRefusesAtLimit(deletion) {
		t.Fatal("a deletion on the full cluster is refused")
	}

	// the second cluster's P1 admissions go on, as the first's cohort limit sheds nothing there
	grant, err := budget.AdmitP1(ctx, "meta-2", payload.NetworkId, sidecar.WriteCost{ImmediateByteCount: 1024})
	if err != nil {
		t.Fatalf("a P1 write on the second cluster: %v", err)
	}
	grant.Close()

	// admin info unread: the loop keeps the last flag, and so does the process
	infos.stateLock.Lock()
	delete(infos.clusterIdInfos, "meta-1")
	infos.stateLock.Unlock()
	clock.Advance(loopSettings.Budget.Interval)
	if allowance := loops["meta-1"].Step(ctx); allowance != nil && !allowance.AtLimit {
		t.Fatal("an unread admin info dropped the first cluster's flag")
	}
}
