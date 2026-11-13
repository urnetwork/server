package monitor

// SIGNALS.md §29.1's heal debt from the fork's series as a scrape labels them: a set's debt is known only when
// every host its cluster's admin info lists shows its queue and the full heal reads, the state probe reads it in
// one query, and the queues' sums are published as §19.6 names them.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// One host's fork series for a set of its cluster.
type fileClusterTestHostDebt struct {
	depth        float64
	dropped      float64
	unclean      float64
	lastLossUnix float64
}

// The fork's series for clusterId as the scrape labels them: every host of info shows hostDebt for set 0-0,
// overridden per host index, and the cluster's full heal from healStartUnix to healEndUnix.
func fileClusterTestDebtSamples(clusterId string, info *server.FileClusterAdminInfo, hostDebts map[int]fileClusterTestHostDebt, healStartUnix float64, healEndUnix float64) []*sidecarStorageSample {
	samples := []*sidecarStorageSample{}
	for hostIndex, host := range info.Hosts {
		debt := hostDebts[hostIndex]
		labels := []string{"cluster", clusterId, "server", host.Endpoint, "host", "store", "pool", "0", "set", "0"}
		samples = append(samples,
			sidecarStorageTestSample(fileClusterDebtCheckDepth, debt.depth, labels...),
			sidecarStorageTestSample(fileClusterDebtCheckDropped, debt.dropped, labels...),
			sidecarStorageTestSample(fileClusterDebtCheckUnclean, debt.unclean, labels...),
			sidecarStorageTestSample(fileClusterDebtCheckLastLoss, debt.lastLossUnix, labels...),
			sidecarStorageTestSample(fileClusterDebtCheckHealStart, healStartUnix, "cluster", clusterId, "server", host.Endpoint),
			sidecarStorageTestSample(fileClusterDebtCheckHealEnd, healEndUnix, "cluster", clusterId, "server", host.Endpoint),
		)
	}
	return samples
}

// samples without those of check from the host at hostIndex.
func fileClusterTestDebtSamplesWithout(samples []*sidecarStorageSample, info *server.FileClusterAdminInfo, hostIndex int, check string) []*sidecarStorageSample {
	kept := []*sidecarStorageSample{}
	for _, sample := range samples {
		if sample.check == check && sample.label("server") == info.Hosts[hostIndex].Endpoint {
			continue
		}
		kept = append(kept, sample)
	}
	return kept
}

// What the state signal published of the queues.
type fakeFileClusterHealDebtGauges struct {
	*fakeSidecarStorageGauges
	debtLock         sync.Mutex
	clusterSetQueues map[string]map[string]FileClusterQueueDebt
}

func newFakeFileClusterHealDebtGauges() *fakeFileClusterHealDebtGauges {
	return &fakeFileClusterHealDebtGauges{fakeSidecarStorageGauges: newFakeSidecarStorageGauges(), clusterSetQueues: map[string]map[string]FileClusterQueueDebt{}}
}

func (self *fakeFileClusterHealDebtGauges) PublishQueueDebts(clusterId string, setQueues map[string]FileClusterQueueDebt) {
	self.debtLock.Lock()
	defer self.debtLock.Unlock()
	self.clusterSetQueues[clusterId] = setQueues
}

func (self *fakeFileClusterHealDebtGauges) queue(clusterId string, set string) (FileClusterQueueDebt, bool) {
	self.debtLock.Lock()
	defer self.debtLock.Unlock()
	queue, ok := self.clusterSetQueues[clusterId][set]
	return queue, ok
}

// The query asks for each of the fork's series from the scrape's job alone, fresh.
func TestFileClusterHealDebtQueryReadsTheScrapesJob(t *testing.T) {
	query := fileClusterHealDebtQuery("synthetic", 3*time.Minute)
	for _, metric := range []string{
		fileClusterHostMrfDepthMetricName, fileClusterHostMrfDroppedMetricName, fileClusterHostUncleanStartsMetricName,
		fileClusterHostMrfLastLossMetricName, fileClusterFullHealStartMetricName, fileClusterFullHealEndMetricName,
	} {
		if !strings.Contains(query, metric+`{env="synthetic",job="file_cluster_minio"}`) {
			t.Errorf("the query does not read %s from the scrape's job: %s", metric, query)
		}
	}
	if !strings.Contains(query, "time() - 180") {
		t.Errorf("the query takes stale samples: %s", query)
	}
}

// Every host showing its queue and the full heal: the debt is known, the depth and counts are the hosts' sums,
// and nothing is lost while no host lost anything.
func TestFileClusterHealDebtsSumEveryHost(t *testing.T) {
	info := fileClusterTestInfo("meta-1", 4, 2)
	samples := fileClusterTestDebtSamples("meta-1", info, map[int]fileClusterTestHostDebt{
		0: {depth: 2},
		3: {depth: 1},
	}, 0, 0)
	debts, queues := fileClusterHealDebts("meta-1", info, samples)
	debt := debts["0-0"]
	if debt == nil || !debt.known || debt.depth != 3 || debt.lost || !debt.fullHealStartTime.IsZero() || !debt.fullHealEndTime.IsZero() {
		t.Fatalf("debt %+v", debt)
	}
	if queue := queues["0-0"]; queue.Depth != 3 || queue.DroppedCount != 0 || queue.UncleanStartCount != 0 {
		t.Fatalf("queue %+v", queue)
	}
	// another cluster's series answer for none of this one's hosts
	if debts, queues := fileClusterHealDebts("seg-1", fileClusterTestInfo("seg-1", 4, 4), samples); len(debts) != 0 || len(queues) != 0 {
		t.Fatalf("another cluster read this one's series: %v %v", debts, queues)
	}
}

// A host that says nothing may hold a queue: with any one of a host's four queue series missing, stale or not a
// count, or a host the admin info lists not in Mimir at all, the set's debt is unknown and its queue is not
// published. A series from a host the admin info does not list stands in for none.
func TestFileClusterHealDebtsNeedEveryHostsQueue(t *testing.T) {
	info := fileClusterTestInfo("meta-1", 4, 2)
	samples := fileClusterTestDebtSamples("meta-1", info, nil, 0, 0)
	for _, check := range []string{fileClusterDebtCheckDepth, fileClusterDebtCheckDropped, fileClusterDebtCheckUnclean, fileClusterDebtCheckLastLoss} {
		debts, queues := fileClusterHealDebts("meta-1", info, fileClusterTestDebtSamplesWithout(samples, info, 2, check))
		if len(debts) != 0 || len(queues) != 0 {
			t.Errorf("a host without its %s series: debts %v queues %v", check, debts, queues)
		}
	}

	// a fifth host the admin info lists, which Mimir does not show
	grown := fileClusterTestInfo("meta-1", 5, 2)
	if debts, queues := fileClusterHealDebts("meta-1", grown, samples); len(debts) != 0 || len(queues) != 0 {
		t.Errorf("a listed host with no series: debts %v queues %v", debts, queues)
	}

	// a stranger's series in the place of a listed host's
	stranger := fileClusterTestDebtSamplesWithout(samples, info, 1, fileClusterDebtCheckDepth)
	stranger = append(stranger, sidecarStorageTestSample(fileClusterDebtCheckDepth, 0, "cluster", "meta-1", "server", "stranger.example:59000", "pool", "0", "set", "0"))
	if debts, queues := fileClusterHealDebts("meta-1", info, stranger); len(debts) != 0 || len(queues) != 0 {
		t.Errorf("a host the admin info does not list stood in: debts %v queues %v", debts, queues)
	}

	// a depth that is no count
	broken := fileClusterTestDebtSamplesWithout(samples, info, 0, fileClusterDebtCheckDepth)
	broken = append(broken, sidecarStorageTestSample(fileClusterDebtCheckDepth, -1, "cluster", "meta-1", "server", info.Hosts[0].Endpoint, "pool", "0", "set", "0"))
	if debts, queues := fileClusterHealDebts("meta-1", info, broken); len(debts) != 0 || len(queues) != 0 {
		t.Errorf("a depth under zero: debts %v queues %v", debts, queues)
	}
}

// With every queue read and no host showing the cluster's full heal, the queue is published and the debt is
// unknown: whether a loss was healed since cannot be told.
func TestFileClusterHealDebtsNeedTheFullHeal(t *testing.T) {
	info := fileClusterTestInfo("meta-1", 4, 2)
	samples := fileClusterTestDebtSamples("meta-1", info, map[int]fileClusterTestHostDebt{1: {depth: 1}}, 0, 0)
	for hostIndex := range info.Hosts {
		samples = fileClusterTestDebtSamplesWithout(samples, info, hostIndex, fileClusterDebtCheckHealEnd)
	}
	debts, queues := fileClusterHealDebts("meta-1", info, samples)
	if len(debts) != 0 {
		t.Errorf("debts %v", debts)
	}
	if queue, ok := queues["0-0"]; !ok || queue.Depth != 1 {
		t.Errorf("queues %v", queues)
	}
}

// A loss on any host holds until a full heal begins after it: no full heal, one that began before the loss and
// one that began at it leave the set lost; one that began after clears it. The full heal is the newest any host
// read.
func TestFileClusterHealDebtsLostUntilAFullHealBeginsAfterTheLoss(t *testing.T) {
	info := fileClusterTestInfo("meta-1", 4, 2)
	lossUnix := float64(fileClusterTestNow.Unix())
	hostDebts := map[int]fileClusterTestHostDebt{
		1: {dropped: 2, lastLossUnix: lossUnix - 600},
		2: {unclean: 1, lastLossUnix: lossUnix},
	}
	lost := func(healStartUnix float64, healEndUnix float64) *fileClusterHealDebt {
		debts, queues := fileClusterHealDebts("meta-1", info, fileClusterTestDebtSamples("meta-1", info, hostDebts, healStartUnix, healEndUnix))
		if queue := queues["0-0"]; queue.DroppedCount != 2 || queue.UncleanStartCount != 1 {
			t.Fatalf("queue %+v", queue)
		}
		if debts["0-0"] == nil {
			t.Fatal("the debt is unknown")
		}
		return debts["0-0"]
	}
	if debt := lost(0, 0); !debt.lost {
		t.Error("a loss with no full heal reads as nothing lost")
	}
	// after the older host's loss, before the newer one's
	if debt := lost(lossUnix-300, lossUnix-200); !debt.lost {
		t.Error("a full heal that began before the last loss cleared it")
	}
	if debt := lost(lossUnix, lossUnix+60); !debt.lost {
		t.Error("a full heal that began at the loss cleared it")
	}
	debt := lost(lossUnix+1, lossUnix+90)
	if debt.lost {
		t.Error("a full heal that began after the last loss left it lost")
	}
	if !debt.fullHealStartTime.Equal(fileClusterTestNow.Add(time.Second)) || !debt.fullHealEndTime.Equal(fileClusterTestNow.Add(90*time.Second)) {
		t.Errorf("full heal %s..%s", debt.fullHealStartTime, debt.fullHealEndTime)
	}

	// one host still serves its last read of an older heal: the newest end wins, with its own start
	samples := fileClusterTestDebtSamples("meta-1", info, hostDebts, lossUnix+1, lossUnix+90)
	samples = fileClusterTestDebtSamplesWithout(samples, info, 0, fileClusterDebtCheckHealStart)
	samples = fileClusterTestDebtSamplesWithout(samples, info, 0, fileClusterDebtCheckHealEnd)
	samples = append(samples,
		sidecarStorageTestSample(fileClusterDebtCheckHealStart, lossUnix-300, "cluster", "meta-1", "server", info.Hosts[0].Endpoint),
		sidecarStorageTestSample(fileClusterDebtCheckHealEnd, lossUnix-200, "cluster", "meta-1", "server", info.Hosts[0].Endpoint))
	debts, _ := fileClusterHealDebts("meta-1", info, samples)
	if debts["0-0"] == nil || debts["0-0"].lost || !debts["0-0"].fullHealEndTime.Equal(fileClusterTestNow.Add(90*time.Second)) {
		t.Errorf("with one host on an older read: %+v", debts["0-0"])
	}
}

// The state probe reads the fork's series in one Mimir query: a set every host shows empty reads protected, a
// queued write degrades it until the queue drains, a lost entry warns until a full heal begins after it, a host
// gone silent leaves the set unknown and its queue unpublished, and a query that fails leaves every set unknown.
func TestFileClusterStateReadsTheForksHealDebtFromMimir(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeFileClusterHealDebtGauges()
	probe := newFileClusterStateProbe()
	info := source.info("meta-1")
	var samples []*sidecarStorageSample
	var queryErr error
	queryCount := 0
	probe.query = func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
		queryCount += 1
		if query != fileClusterHealDebtQuery("synthetic", DefaultSidecarStorageSettings().MetricFreshness) {
			t.Errorf("query %s", query)
		}
		return samples, queryErr
	}
	now := fileClusterTestNow
	check := func(want ...string) {
		t.Helper()
		now = now.Add(time.Minute)
		requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, now), want...)
	}

	samples = fileClusterTestDebtSamples("meta-1", info, nil, 0, 0)
	check()
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateProtected {
		t.Fatalf("gauge %q", state)
	}
	if queue, ok := gauges.queue("meta-1", "0-0"); !ok || queue.Depth != 0 {
		t.Fatalf("queue %+v %t", queue, ok)
	}
	if queryCount != 1 {
		t.Fatalf("%d queries for one run", queryCount)
	}

	samples = fileClusterTestDebtSamples("meta-1", info, map[int]fileClusterTestHostDebt{2: {depth: 4}}, 0, 0)
	check("filecluster-set-degraded meta-1/0-0")
	if queue, _ := gauges.queue("meta-1", "0-0"); queue.Depth != 4 {
		t.Fatalf("queue %+v", queue)
	}
	samples = fileClusterTestDebtSamples("meta-1", info, nil, 0, 0)
	check()

	// an entry lost with every drive online warns and holds the set until a full heal begins after it
	lossUnix := float64(now.Unix())
	lostDebts := map[int]fileClusterTestHostDebt{2: {dropped: 1, lastLossUnix: lossUnix}}
	samples = fileClusterTestDebtSamples("meta-1", info, lostDebts, 0, 0)
	check("filecluster-heal-debt-lost meta-1/0-0", "filecluster-set-degraded meta-1/0-0")
	samples = fileClusterTestDebtSamples("meta-1", info, lostDebts, lossUnix-3600, lossUnix-1800)
	check("filecluster-heal-debt-lost meta-1/0-0", "filecluster-set-degraded meta-1/0-0")
	samples = fileClusterTestDebtSamples("meta-1", info, lostDebts, lossUnix+30, lossUnix+60)
	check()
	if queue, _ := gauges.queue("meta-1", "0-0"); queue.DroppedCount != 1 {
		t.Fatalf("queue %+v", queue)
	}

	// a host gone silent: nothing says its queue is empty
	samples = fileClusterTestDebtSamplesWithout(samples, info, 3, fileClusterDebtCheckDepth)
	check("filecluster-state-unobservable meta-1/0-0")
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateUnknown {
		t.Fatalf("gauge %q", state)
	}
	if _, ok := gauges.queue("meta-1", "0-0"); ok {
		t.Fatal("a queue one host did not show is published")
	}

	// Mimir itself not reading
	samples = fileClusterTestDebtSamples("meta-1", info, nil, 0, 0)
	queryErr = errors.New("connection refused")
	check("filecluster-state-unobservable meta-1/0-0")
	if _, ok := gauges.queue("meta-1", "0-0"); ok {
		t.Fatal("a queue is published with Mimir not reading")
	}
	queryErr = nil
	check()
}

// An outage seen through the fork's series: the set stays degraded through the host's return and through a
// full heal that began while it was away, and clears on one that began after.
func TestFileClusterStateOutageEndsOnAFullHealBegunAfterTheReturn(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeFileClusterHealDebtGauges()
	probe := newFileClusterStateProbe()
	info := source.info("meta-1")
	healStartUnix, healEndUnix := 0.0, 0.0
	probe.query = func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
		samples := fileClusterTestDebtSamples("meta-1", info, nil, healStartUnix, healEndUnix)
		// a host that is down pushes nothing
		for hostIndex, host := range info.Hosts {
			if host.State != "online" {
				for _, check := range []string{fileClusterDebtCheckDepth, fileClusterDebtCheckHealStart, fileClusterDebtCheckHealEnd} {
					samples = fileClusterTestDebtSamplesWithout(samples, info, hostIndex, check)
				}
			}
		}
		return samples, nil
	}
	now := fileClusterTestNow
	check := func(want ...string) {
		t.Helper()
		now = now.Add(time.Minute)
		requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, now), want...)
	}
	check()
	setFileClusterTestHost(info, 1, "offline")
	check("filecluster-set-degraded meta-1/0-0")
	// a full heal begun while the host is away
	healStartUnix = float64(now.Add(30 * time.Second).Unix())
	check("filecluster-set-degraded meta-1/0-0")
	setFileClusterTestHost(info, 1, "online")
	healEndUnix = float64(now.Add(30 * time.Second).Unix())
	check("filecluster-set-degraded meta-1/0-0")
	check("filecluster-set-degraded meta-1/0-0")
	// one begun after the host is back
	healStartUnix = float64(now.Add(10 * time.Second).Unix())
	healEndUnix = float64(now.Add(50 * time.Second).Unix())
	check()
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateProtected {
		t.Fatalf("gauge %q", state)
	}
}

// The published series: a gauge and two counters per cluster and set, named as §19.6 names them, replaced on
// each publish and removed for a set or cluster no longer named.
func TestFileClusterQueueDebtSeriesAreNamedAsTheDesignNamesThem(t *testing.T) {
	registry := prometheus.NewRegistry()
	gauges := NewPrometheusSidecarStorageGauges(registry)
	debtGauges, ok := gauges.(FileClusterHealDebtGauges)
	if !ok {
		t.Fatal("the prometheus gauges publish no queue debt")
	}
	// a second gauge set of the process shares the one collector
	again := NewPrometheusSidecarStorageGauges(registry).(FileClusterHealDebtGauges)
	debtGauges.PublishQueueDebts("meta-1", map[string]FileClusterQueueDebt{
		"0-0": {Depth: 3, DroppedCount: 2, UncleanStartCount: 1},
		"0-1": {},
	})
	again.PublishQueueDebts("seg-1", map[string]FileClusterQueueDebt{"0-0": {Depth: 7}})

	names := []string{"urnetwork_filecluster_mrf_depth", "urnetwork_filecluster_mrf_dropped_total", "urnetwork_filecluster_unclean_starts_total"}
	const help = `# HELP urnetwork_filecluster_mrf_depth Partial writes of the erasure set its cluster's hosts have queued or are healing (§19.6).
# TYPE urnetwork_filecluster_mrf_depth gauge
# HELP urnetwork_filecluster_mrf_dropped_total Queued partial writes of the erasure set its cluster's hosts lost: never queued, or taken off unhealed.
# TYPE urnetwork_filecluster_mrf_dropped_total counter
# HELP urnetwork_filecluster_unclean_starts_total Starts of the cluster's hosts that followed a process which did not save its partial-write queue.
# TYPE urnetwork_filecluster_unclean_starts_total counter
`
	want := help + `urnetwork_filecluster_mrf_depth{cluster="meta-1",set="0-0"} 3
urnetwork_filecluster_mrf_depth{cluster="meta-1",set="0-1"} 0
urnetwork_filecluster_mrf_depth{cluster="seg-1",set="0-0"} 7
urnetwork_filecluster_mrf_dropped_total{cluster="meta-1",set="0-0"} 2
urnetwork_filecluster_mrf_dropped_total{cluster="meta-1",set="0-1"} 0
urnetwork_filecluster_mrf_dropped_total{cluster="seg-1",set="0-0"} 0
urnetwork_filecluster_unclean_starts_total{cluster="meta-1",set="0-0"} 1
urnetwork_filecluster_unclean_starts_total{cluster="meta-1",set="0-1"} 0
urnetwork_filecluster_unclean_starts_total{cluster="seg-1",set="0-0"} 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), names...); err != nil {
		t.Fatal(err)
	}

	// a set one host stopped showing loses its series, and a cluster with none loses them all
	debtGauges.PublishQueueDebts("meta-1", map[string]FileClusterQueueDebt{"0-1": {Depth: 1}})
	debtGauges.PublishQueueDebts("seg-1", nil)
	want = help + `urnetwork_filecluster_mrf_depth{cluster="meta-1",set="0-1"} 1
urnetwork_filecluster_mrf_dropped_total{cluster="meta-1",set="0-1"} 0
urnetwork_filecluster_unclean_starts_total{cluster="meta-1",set="0-1"} 0
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), names...); err != nil {
		t.Fatal(err)
	}
}
