package monitor

// SIGNALS.md §29.7 on fixture admin info and Mimir answers: each cluster projected from its own slopes, the cohort
// limit, the earliest exhaustion named, D-OPS-17's marks, the first cluster's control plane apart, the hdd trigger,
// the gauges and the blind cases.

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// What the cohort signal published, beside the storage gauges.
type fakeFileClusterExhaustionGauges struct {
	*fakeSidecarStorageGauges
	stateLock sync.Mutex
	days      map[string]float64
}

func newFakeFileClusterExhaustionGauges() *fakeFileClusterExhaustionGauges {
	return &fakeFileClusterExhaustionGauges{fakeSidecarStorageGauges: newFakeSidecarStorageGauges(), days: map[string]float64{}}
}

func (self *fakeFileClusterExhaustionGauges) PublishExhaustionDays(clusterId string, resource string, days float64, known bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if known {
		self.days[clusterId+"/"+resource] = days
	} else {
		delete(self.days, clusterId+"/"+resource)
	}
}

func (self *fakeFileClusterExhaustionGauges) get(clusterId string, resource string) (float64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	days, ok := self.days[clusterId+"/"+resource]
	return days, ok
}

// Two metadata clusters and a segment cluster: meta-1, place_new's old cluster, holding its cohort, and meta-2,
// mostly empty. rawShare and inodeShare are each cluster's admin info now.
func fileClusterCohortTestSource(meta1Raw float64, meta1Inodes float64, meta2Raw float64) *fakeFileClusterAdminSource {
	source := newFakeFileClusterAdminSource()
	source.clusterIds = []string{"meta-1", "meta-2", "seg-1"}
	source.kinds["meta-2"] = server.FileClusterKindMetadata
	source.infos["meta-2"] = fileClusterTestInfo("meta-2", 4, 2)
	fill := func(info *server.FileClusterAdminInfo, raw float64, inodes float64) {
		info.RawCapacityByteCount = 1000000
		info.RawUsageByteCount = uint64(raw * 1000000)
		info.UsedInodeCount = uint64(inodes * 1000)
		info.FreeInodeCount = 1000 - info.UsedInodeCount
	}
	fill(source.infos["meta-1"], meta1Raw, meta1Inodes)
	fill(source.infos["meta-2"], meta2Raw, 0.01)
	return source
}

// The filesystems of a cluster's drives, two per host on four hosts as fileClusterTestInfo lists them: rawGain and
// inodeGain are the shares the cluster grew by over the window.
func fileClusterCohortTestFilesystems(clusterId string, rawGain float64, inodeGain float64) []*sidecarStorageSample {
	samples := []*sidecarStorageSample{}
	for host := 1; host <= 4; host += 1 {
		for drive := 1; drive <= 2; drive += 1 {
			labels := []string{"host", "store-" + string(rune('0'+host)), "device", "/dev/nvme" + string(rune('0'+drive)) + "n1",
				"mountpoint", "/srv/file-cluster/" + clusterId + "/d" + string(rune('0'+drive))}
			samples = append(samples,
				sidecarStorageTestSample(fileClusterCohortCheckSize, 1000, labels...),
				sidecarStorageTestSample(fileClusterCohortCheckAvail, 500, labels...),
				sidecarStorageTestSample(fileClusterCohortCheckAvailThen, 500+rawGain*1000, labels...),
				sidecarStorageTestSample(fileClusterCohortCheckFiles, 1000, labels...),
				sidecarStorageTestSample(fileClusterCohortCheckFilesFree, 600, labels...),
				sidecarStorageTestSample(fileClusterCohortCheckFilesFreeThen, 600+inodeGain*1000, labels...),
			)
		}
	}
	return samples
}

// The drive paths each metadata cluster's admin info lists.
func fileClusterCohortTestPaths() map[string]string {
	paths := map[string]string{}
	for _, clusterId := range []string{"meta-1", "meta-2"} {
		for drive := 1; drive <= 2; drive += 1 {
			paths["/srv/file-cluster/"+clusterId+"/d"+string(rune('0'+drive))] = clusterId
		}
	}
	return paths
}

func fileClusterCohortTestInputs(source *fakeFileClusterAdminSource) []*fileClusterCohortInput {
	inputs := []*fileClusterCohortInput{}
	for _, clusterId := range source.clusterIds {
		inputs = append(inputs, &fileClusterCohortInput{clusterId: clusterId, kind: source.kinds[clusterId], info: source.infos[clusterId]})
	}
	return inputs
}

func evaluateFileClusterCohort(source *fakeFileClusterAdminSource, samples []*sidecarStorageSample, gauges SidecarStorageGauges) []finding {
	settings := sidecarStorageTestSettings(source, gauges)
	return fileClusterCohortProbe{}.evaluate(settings, fileClusterCohortTestInputs(source), fileClusterCohortTestPaths(), samples)
}

// The steady samples: meta-1 growing 3% raw and 1.5% inodes a month at 0.3 DWPD rising 0.03, meta-2 flat, the first
// cluster's control plane growing, the segment cluster's drive operations flat and measured.
func fileClusterCohortTestSamples() []*sidecarStorageSample {
	samples := append(fileClusterCohortTestFilesystems("meta-1", 0.03, 0.015), fileClusterCohortTestFilesystems("meta-2", 0, 0)...)
	return append(samples,
		sidecarStorageTestSample(fileClusterCohortCheckDwpd, 0.30, "cluster", "meta-1", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpdThen, 0.27, "cluster", "meta-1", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpd, 0.05, "cluster", "meta-2", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpdThen, 0.05, "cluster", "meta-2", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckControl, 3.0e9, "cluster", "meta-1"),
		sidecarStorageTestSample(fileClusterCohortCheckControlThen, 2.7e9, "cluster", "meta-1"),
		sidecarStorageTestSample(fileClusterCohortCheckHddOps, 100, "cluster", "seg-1"),
		sidecarStorageTestSample(fileClusterCohortCheckHddOpsThen, 100, "cluster", "seg-1"),
		sidecarStorageTestSample(fileClusterCohortCheckHddCapacity, 1000, "cluster", "seg-1"),
	)
}

// Each cluster projected from its own slopes: meta-1's raw reaches 80% from 30% at 0.1% a day in 500 days, its
// inodes 70% from 30% at 0.05% a day in 800, its wear 0.75 from 0.30 at 0.001 a day in 450, its earliest; meta-2,
// flat, never; nothing raised under the lines and outside the 182-day lead times.
func TestFileClusterCohortProjectsEachClusterFromItsOwnSlopes(t *testing.T) {
	gauges := newFakeFileClusterExhaustionGauges()
	source := fileClusterCohortTestSource(0.30, 0.30, 0.02)
	findings := evaluateFileClusterCohort(source, fileClusterCohortTestSamples(), gauges)
	requireSidecarStorageRaised(t, findings)
	want := map[string]float64{"meta-1/raw": 500, "meta-1/inodes": 800, "meta-1/wear": 450}
	for key, days := range want {
		parts := strings.SplitN(key, "/", 2)
		if got, ok := gauges.get(parts[0], parts[1]); !ok || math.Abs(got-days) > 0.01 {
			t.Errorf("%s: %v %v, want %v", key, got, ok, days)
		}
	}
	for _, resource := range []string{"raw", "inodes", "wear"} {
		if got, ok := gauges.get("meta-2", resource); !ok || !math.IsInf(got, 1) {
			t.Errorf("meta-2 %s: %v %v, want +Inf", resource, got, ok)
		}
	}
	if _, ok := gauges.get("meta-1", "copy"); ok {
		t.Error("copy load, which no series carries, was projected")
	}
	if got, ok := gauges.get("seg-1", "hdd_ops"); !ok || !math.IsInf(got, 1) {
		t.Errorf("seg-1 hdd_ops: %v %v", got, ok)
	}
}

// At 80% raw or 70% inodes the cluster stands at its cohort limit, where evs answers 507; the other cluster, mostly
// empty, raises nothing, since the estate's free space lifts no cohort.
func TestFileClusterCohortLimitAtEightyRawOrSeventyInodes(t *testing.T) {
	// at 79% raw, the line is 10 days off at 0.1% a day: the exhaustion page, and the marks long passed
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.79, 0.40, 0.02), fileClusterCohortTestSamples(), nil),
		"filecluster-cohort-exhaustion meta-1", "filecluster-cohort-trigger meta-1")
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.80, 0.40, 0.02), fileClusterCohortTestSamples(), nil),
		"filecluster-cohort-exhaustion meta-1", "filecluster-cohort-limit meta-1", "filecluster-cohort-trigger meta-1")
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.40, 0.70, 0.02), fileClusterCohortTestSamples(), nil),
		"filecluster-cohort-exhaustion meta-1", "filecluster-cohort-limit meta-1", "filecluster-cohort-trigger meta-1")
}

// A cohort whose own growth reaches its line inside the move tool's lead time raises the exhaustion page, naming
// the estate's earliest; the first cluster's control plane shows apart.
func TestFileClusterCohortExhaustionInsideTheLeadTimeNamesTheEarliest(t *testing.T) {
	samples := append(fileClusterCohortTestFilesystems("meta-1", 0.3, 0.015), fileClusterCohortTestFilesystems("meta-2", 0.03, 0)...)
	samples = append(samples,
		sidecarStorageTestSample(fileClusterCohortCheckDwpd, 0.30, "cluster", "meta-1", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpdThen, 0.30, "cluster", "meta-1", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpd, 0.05, "cluster", "meta-2", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckDwpdThen, 0.05, "cluster", "meta-2", "window", "7d"),
		sidecarStorageTestSample(fileClusterCohortCheckControl, 3.0e9, "cluster", "meta-1"),
		sidecarStorageTestSample(fileClusterCohortCheckControlThen, 2.7e9, "cluster", "meta-1"),
	)
	// meta-1: 30% raw growing 1% a day reaches 80% in 50 days; meta-2: 2% growing 0.1% a day in 780
	findings := evaluateFileClusterCohort(fileClusterCohortTestSource(0.30, 0.40, 0.02), samples, nil)
	requireSidecarStorageRaised(t, findings, "filecluster-cohort-exhaustion meta-1", "filecluster-cohort-trigger meta-1")
	for _, finding := range findings {
		if finding.healthy || finding.class != fileClusterCohortClassExhaustion {
			continue
		}
		if !strings.Contains(finding.observed, "earliest: meta-1 raw in 50 days") {
			t.Errorf("the earliest is not named: %s", finding.observed)
		}
		if !strings.Contains(finding.observed, "control_plane_bytes=3000000000 per_day=10000000") {
			t.Errorf("the control plane is not shown apart: %s", finding.observed)
		}
	}
}

// D-OPS-17's marks: 50% raw or 40% inodes reached, or a mark projected inside the next cluster's lead time.
func TestFileClusterCohortTriggerAtTheMarksOrTheirProjection(t *testing.T) {
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.50, 0.10, 0.02), fileClusterCohortTestSamples(), nil),
		"filecluster-cohort-trigger meta-1")
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.20, 0.40, 0.02), fileClusterCohortTestSamples(), nil),
		"filecluster-cohort-trigger meta-1")
	// 30% raw at 0.1% a day reaches 50% in 200 days, outside 182; wear at 0.30 rising 0.001 a day reaches 0.5 in 200
	samples := fileClusterCohortTestSamples()
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.30, 0.10, 0.02), samples, nil))
	// at 0.33 raw it reaches 50% in 170 days, inside
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.33, 0.10, 0.02), samples, nil),
		"filecluster-cohort-trigger meta-1")
}

// A segment cluster's drive operations trending to half its measured capacity inside the placement's lead time
// raise the hdd trigger; with no measured capacity nothing is projected.
func TestFileClusterCohortHddTrigger(t *testing.T) {
	samples := fileClusterCohortTestSamples()
	for _, sample := range samples {
		if sample.check == fileClusterCohortCheckHddOpsThen {
			// 100 a second, up 90 in 30 days, 3 a day: half of 1,000 in 133 days
			sample.value = 10
		}
	}
	gauges := newFakeFileClusterExhaustionGauges()
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.30, 0.10, 0.02), samples, gauges),
		"filecluster-hdd-trigger seg-1")
	if got, ok := gauges.get("seg-1", "hdd_ops"); !ok || math.Abs(got-400.0/3) > 0.01 {
		t.Errorf("hdd_ops days %v %v", got, ok)
	}
	unmeasured := []*sidecarStorageSample{}
	for _, sample := range samples {
		if sample.check != fileClusterCohortCheckHddCapacity {
			unmeasured = append(unmeasured, sample)
		}
	}
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.30, 0.10, 0.02), unmeasured, gauges))
	if _, ok := gauges.get("seg-1", "hdd_ops"); ok {
		t.Error("a cluster with no measured capacity was projected")
	}
}

// No 30-day reading: nothing projected and no gauge, the line still read from admin info; no filesystem of a
// cluster's drives, or no drive writes, reads blind; Mimir and admin info failing read blind.
func TestFileClusterCohortBlindAndUnprojected(t *testing.T) {
	gauges := newFakeFileClusterExhaustionGauges()
	samples := []*sidecarStorageSample{}
	for _, sample := range fileClusterCohortTestSamples() {
		switch sample.check {
		case fileClusterCohortCheckAvailThen, fileClusterCohortCheckFilesFreeThen, fileClusterCohortCheckDwpdThen:
		default:
			samples = append(samples, sample)
		}
	}
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.60, 0.10, 0.02), samples, gauges),
		"filecluster-cohort-trigger meta-1")
	if _, ok := gauges.get("meta-1", "raw"); ok {
		t.Error("a cluster with no 30-day reading was projected")
	}
	// past its line a resource is exhausted whatever its slope: 0 days
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.81, 0.10, 0.02), samples, gauges),
		"filecluster-cohort-exhaustion meta-1", "filecluster-cohort-limit meta-1", "filecluster-cohort-trigger meta-1")
	if got, ok := gauges.get("meta-1", "raw"); !ok || got != 0 {
		t.Errorf("a resource past its line reads %v %v, want 0", got, ok)
	}

	noFilesystems := []*sidecarStorageSample{}
	for _, sample := range fileClusterCohortTestSamples() {
		if sample.label("mountpoint") == "" || !strings.Contains(sample.label("mountpoint"), "meta-2") {
			if sample.check == fileClusterCohortCheckDwpd && sample.label("cluster") == "meta-2" {
				continue
			}
			noFilesystems = append(noFilesystems, sample)
		}
	}
	requireSidecarStorageRaised(t, evaluateFileClusterCohort(fileClusterCohortTestSource(0.30, 0.10, 0.02), noFilesystems, nil),
		"filecluster-cohort-unobservable meta-2", "filecluster-cohort-unobservable meta-2")

	source := fileClusterCohortTestSource(0.30, 0.10, 0.02)
	source.errs["meta-2"] = errors.New("admin info refused")
	probe := fileClusterCohortProbe{query: fixedSidecarStorageQuery(fileClusterCohortTestSamples(), nil)}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(source, nil), func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-cohort-unobservable meta-2")

	probe = fileClusterCohortProbe{query: fixedSidecarStorageQuery(nil, errors.New("mimir down"))}
	findings, err = probe.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(fileClusterCohortTestSource(0.3, 0.1, 0.02), nil), func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	requireSidecarStorageRaised(t, findings, "filecluster-cohort-unobservable mimir")
}

// Without its block the signal reports itself off, raising nothing.
func TestFileClusterCohortConfiguredOff(t *testing.T) {
	settings := DefaultSidecarStorageSettings()
	findings, err := fileClusterCohortProbe{}.check(t.Context(), sidecarStorageTestEnv(settings, func() time.Time { return fileClusterTestNow }))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || !findings[0].configuredOff {
		t.Fatalf("findings %+v", findings)
	}
}
