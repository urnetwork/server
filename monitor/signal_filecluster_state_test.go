package monitor

// SIGNALS.md §29.1's states on fixture admin info, the storage signals' shared fakes, and FS-50's file-cluster
// conditions at 02:00 reaching the rota's pager sink.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/pager"
)

// FS-50's hour: 02:00 on the pager tests' day.
var fileClusterTestNow = time.Date(2026, time.November, 7, 2, 0, 0, 0, time.UTC)

// A clock a test moves by hand.
type fileClusterTestClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *fileClusterTestClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *fileClusterTestClock) Advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

// The clusters' admin info as a test sets it.
type fakeFileClusterAdminSource struct {
	stateLock  sync.Mutex
	clusterIds []string
	kinds      map[string]string
	infos      map[string]*server.FileClusterAdminInfo
	errs       map[string]error
}

func newFakeFileClusterAdminSource() *fakeFileClusterAdminSource {
	return &fakeFileClusterAdminSource{
		clusterIds: []string{"meta-1", "seg-1"},
		kinds:      map[string]string{"meta-1": server.FileClusterKindMetadata, "seg-1": server.FileClusterKindSegment},
		infos: map[string]*server.FileClusterAdminInfo{
			"meta-1": fileClusterTestInfo("meta-1", 4, 2),
			"seg-1":  fileClusterTestInfo("seg-1", 4, 4),
		},
		errs: map[string]error{},
	}
}

func (self *fakeFileClusterAdminSource) ClusterIds() []string {
	return slices.Clone(self.clusterIds)
}

func (self *fakeFileClusterAdminSource) ClusterKind(clusterId string) string {
	return self.kinds[clusterId]
}

func (self *fakeFileClusterAdminSource) ReadAdminInfo(ctx context.Context, clusterId string) (*server.FileClusterAdminInfo, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.errs[clusterId]; err != nil {
		return nil, err
	}
	return self.infos[clusterId], nil
}

// The cluster's info, to change.
func (self *fakeFileClusterAdminSource) info(clusterId string) *server.FileClusterAdminInfo {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.infos[clusterId]
}

// One pool with one set over hostCount hosts of drivesPerHost drives each, every one online.
func fileClusterTestInfo(clusterId string, hostCount int, drivesPerHost int) *server.FileClusterAdminInfo {
	info := &server.FileClusterAdminInfo{PoolCount: 1}
	for hostIndex := range hostCount {
		endpoint := fmt.Sprintf("store-%d.example:59000", hostIndex+1)
		info.Hosts = append(info.Hosts, &server.FileClusterHostInfo{Endpoint: endpoint, State: "online"})
		for driveIndex := range drivesPerHost {
			info.Drives = append(info.Drives, &server.FileClusterDriveInfo{
				Endpoint:     fmt.Sprintf("http://%s/srv/file-cluster/%s/d%d", endpoint, clusterId, driveIndex+1),
				Path:         fmt.Sprintf("/srv/file-cluster/%s/d%d", clusterId, driveIndex+1),
				HostEndpoint: endpoint,
				State:        "ok",
				DiskIndex:    hostIndex*drivesPerHost + driveIndex,
			})
		}
	}
	return info
}

// The host's state in info.
func setFileClusterTestHost(info *server.FileClusterAdminInfo, hostIndex int, state string) {
	info.Hosts[hostIndex].State = state
}

// The drive's state and heal flag in info.
func setFileClusterTestDrive(info *server.FileClusterAdminInfo, diskIndex int, state string, healing bool) {
	for _, drive := range info.Drives {
		if drive.DiskIndex == diskIndex {
			drive.State = state
			drive.Healing = healing
		}
	}
}

// What the signals published.
type fakeSidecarStorageGauges struct {
	stateLock        sync.Mutex
	clusterSetStates map[string]map[string]string
	clusterRatings   map[string]float64
}

func newFakeSidecarStorageGauges() *fakeSidecarStorageGauges {
	return &fakeSidecarStorageGauges{clusterSetStates: map[string]map[string]string{}, clusterRatings: map[string]float64{}}
}

func (self *fakeSidecarStorageGauges) PublishSetStates(clusterId string, setStates map[string]string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.clusterSetStates[clusterId] = setStates
}

func (self *fakeSidecarStorageGauges) PublishDriveRatingDays(clusterId string, days float64, known bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if known {
		self.clusterRatings[clusterId] = days
	} else {
		delete(self.clusterRatings, clusterId)
	}
}

func (self *fakeSidecarStorageGauges) setState(clusterId string, set string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clusterSetStates[clusterId][set]
}

// Settings over the fakes, with the file clusters and the dump on.
func sidecarStorageTestSettings(source FileClusterAdminSource, gauges SidecarStorageGauges) *SidecarStorageSettings {
	settings := DefaultSidecarStorageSettings()
	settings.FileClusters = true
	settings.SidecarDump = true
	settings.AdminSource = source
	settings.Gauges = gauges
	return settings
}

func sidecarStorageTestEnv(settings *SidecarStorageSettings, now func() time.Time) *probeEnv {
	return &probeEnv{cfg: &monitorConfig{env: "synthetic", sidecarStorage: settings}, now: now}
}

// A query that answers samples whatever it is asked, or err.
func fixedSidecarStorageQuery(samples []*sidecarStorageSample, err error) sidecarStorageQuery {
	return func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
		return samples, err
	}
}

// One sample of check with labels as pairs.
func sidecarStorageTestSample(check string, value float64, labels ...string) *sidecarStorageSample {
	sample := &sidecarStorageSample{check: check, labels: map[string]string{}, value: value}
	for i := 0; i+1 < len(labels); i += 2 {
		sample.labels[labels[i]] = labels[i+1]
	}
	return sample
}

// The classes of the findings that are not healthy, sorted, each with its target.
func sidecarStorageTestRaised(findings []finding) []string {
	raised := []string{}
	for _, finding := range findings {
		if !finding.healthy {
			raised = append(raised, finding.class+" "+finding.target)
		}
	}
	slices.Sort(raised)
	return raised
}

func requireSidecarStorageRaised(t *testing.T, findings []finding, want ...string) {
	t.Helper()
	got := sidecarStorageTestRaised(findings)
	if want == nil {
		want = []string{}
	}
	slices.Sort(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raised %v, want %v", got, want)
	}
}

// The state probe's findings for meta-1 alone at now.
func checkFileClusterState(t *testing.T, probe *fileClusterStateProbe, source *fakeFileClusterAdminSource, gauges SidecarStorageGauges, now time.Time) []finding {
	t.Helper()
	source.clusterIds = []string{"meta-1"}
	findings, err := probe.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(source, gauges), func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

// While the fork's heal debt does not read, here with no services host to ask Mimir through, a set with every
// drive online reads unknown, never protected, and reports a visibility finding; its gauge reads unknown.
func TestFileClusterStateUnknownHealDebtNeverReadsProtected(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	findings := checkFileClusterState(t, newFileClusterStateProbe(), source, gauges, fileClusterTestNow)
	requireSidecarStorageRaised(t, findings, "filecluster-state-unobservable meta-1/0-0")
	for _, finding := range findings {
		if !finding.healthy && (!finding.isUnobservable() || finding.frame != "heal-debt") {
			t.Errorf("finding %+v", finding)
		}
	}
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateUnknown {
		t.Errorf("gauge %q", state)
	}
}

// A drive's rebuild alone leaves its set degraded, never at_risk.
func TestFileClusterStateRebuildAloneStaysDegraded(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	probe := newFileClusterStateProbe()
	setFileClusterTestDrive(source.info("meta-1"), 3, "ok", true)
	for minute := range 600 {
		findings := checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(time.Duration(minute)*time.Minute))
		requireSidecarStorageRaised(t, findings, "filecluster-set-degraded meta-1/0-0")
	}
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateDegraded {
		t.Errorf("gauge %q", state)
	}
}

// One host offline is degraded; past the 4 h RTO it is at_risk; it stays at_risk after the host returns,
// until a full heal ends.
func TestFileClusterStateHostOfflinePastTheRtoIsAtRisk(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	probe := newFileClusterStateProbe()
	setFileClusterTestHost(source.info("meta-1"), 1, "offline")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow), "filecluster-set-degraded meta-1/0-0")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(4*time.Hour-time.Second)), "filecluster-set-degraded meta-1/0-0")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(4*time.Hour)), "filecluster-set-at-risk meta-1/0-0")
	setFileClusterTestHost(source.info("meta-1"), 1, "online")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(5*time.Hour)), "filecluster-set-at-risk meta-1/0-0")
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateAtRisk {
		t.Errorf("gauge %q", state)
	}
}

// Degraded and a further drive or host failing is at_risk: two failing at once, or one after another whose
// heal has not been seen; the same host failing again is not a further failure.
func TestFileClusterStateFurtherFailureIsAtRisk(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	probe := newFileClusterStateProbe()
	info := source.info("meta-1")
	setFileClusterTestHost(info, 0, "offline")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow), "filecluster-set-degraded meta-1/0-0")
	setFileClusterTestHost(info, 0, "online")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(time.Minute)), "filecluster-set-degraded meta-1/0-0")
	setFileClusterTestHost(info, 0, "offline")
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(2*time.Minute)), "filecluster-set-degraded meta-1/0-0")
	// a drive on another host fails while the first outage's heal is unseen
	setFileClusterTestHost(info, 0, "online")
	setFileClusterTestDrive(info, 5, "offline", false)
	requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(3*time.Minute)), "filecluster-set-at-risk meta-1/0-0")

	// two at once on a fresh watcher
	fresh := newFakeFileClusterAdminSource()
	setFileClusterTestDrive(fresh.info("meta-1"), 0, "faulty", false)
	setFileClusterTestDrive(fresh.info("meta-1"), 7, "offline", false)
	requireSidecarStorageRaised(t, checkFileClusterState(t, newFileClusterStateProbe(), fresh, gauges, fileClusterTestNow), "filecluster-set-at-risk meta-1/0-0")
}

// With the fork's heal debt known (a fake): the queue alone degrades and drains to protected; an outage ends
// only on a full heal that began after its last failure; lost entries warn and hold the set until a full heal; a
// debt before a failure makes the failure a further one.
func TestFileClusterStateKnownHealDebtFollowsTheOwnersRule(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	probe := newFileClusterStateProbe()
	debt := &fileClusterHealDebt{known: true}
	probe.healDebt = func(ctx context.Context, env *probeEnv, clusterId string) (map[string]*fileClusterHealDebt, error) {
		clone := *debt
		return map[string]*fileClusterHealDebt{"0-0": &clone}, nil
	}
	now := fileClusterTestNow
	check := func(want ...string) {
		t.Helper()
		now = now.Add(time.Minute)
		requireSidecarStorageRaised(t, checkFileClusterState(t, probe, source, gauges, now), want...)
	}
	check()
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateProtected {
		t.Fatalf("gauge %q", state)
	}

	debt.depth = 3
	check("filecluster-set-degraded meta-1/0-0")
	debt.depth = 0
	check()

	info := source.info("meta-1")
	setFileClusterTestHost(info, 2, "offline")
	check("filecluster-set-degraded meta-1/0-0")
	setFileClusterTestHost(info, 2, "online")
	// the queue drained, but a host was offline: only a full heal after it ends the episode
	check("filecluster-set-degraded meta-1/0-0")
	// a full heal that began before the host's return healed part of the set without it
	debt.fullHealStartTime = now.Add(-30 * time.Minute)
	debt.fullHealEndTime = now
	check("filecluster-set-degraded meta-1/0-0")
	debt.fullHealStartTime = now
	debt.fullHealEndTime = now.Add(30 * time.Second)
	check()

	debt.lost = true
	check("filecluster-heal-debt-lost meta-1/0-0", "filecluster-set-degraded meta-1/0-0")
	// a failure now is a further one
	setFileClusterTestDrive(info, 1, "offline", false)
	check("filecluster-set-at-risk meta-1/0-0")
	setFileClusterTestDrive(info, 1, "ok", false)
	debt.lost = false
	debt.fullHealStartTime = now.Add(30 * time.Second)
	debt.fullHealEndTime = now.Add(time.Minute)
	check()
}

// A cluster whose admin info does not read is blind, and its remembered sets read unknown.
func TestFileClusterStateUnreadAdminInfoIsBlind(t *testing.T) {
	source := newFakeFileClusterAdminSource()
	gauges := newFakeSidecarStorageGauges()
	probe := newFileClusterStateProbe()
	setFileClusterTestHost(source.info("meta-1"), 0, "offline")
	checkFileClusterState(t, probe, source, gauges, fileClusterTestNow)
	source.errs["meta-1"] = errors.New("connection refused")
	findings := checkFileClusterState(t, probe, source, gauges, fileClusterTestNow.Add(time.Minute))
	requireSidecarStorageRaised(t, findings, "filecluster-state-unobservable meta-1")
	if !findings[0].isUnobservable() || findings[0].frame != "admin-info" {
		t.Errorf("finding %+v", findings[0])
	}
	if state := gauges.setState("meta-1", "0-0"); state != fileClusterStateUnknown {
		t.Errorf("gauge %q", state)
	}
}

// Without a file_clusters block every file-cluster and budget signal is configured off, which raises no
// incident; with one that did not read, each is blind. Without a sidecar_dump block the dump signal is off.
func TestSidecarStorageSignalsOffWithoutABlockAndBlindWithABadOne(t *testing.T) {
	probes := []probe{newFileClusterStateProbe(), fileClusterWritesProbe{}, fileClusterSettleProbe{}, fileClusterWearProbe{}, writeBudgetProbe{}}
	off := loadSidecarStorageSettings(nil, nil, func() (*server.FileClusters, error) {
		t.Fatal("loaded the vault file without a block")
		return nil, nil
	})
	broken := loadSidecarStorageSettings(&fileClustersMonitorYaml{Enabled: true}, nil, func() (*server.FileClusters, error) {
		return nil, errors.New("no file-clusters-monitor.yml")
	})
	if broken.FileClustersConfigError == "" || broken.AdminSource != nil {
		t.Fatalf("a vault file that did not load: %+v", broken)
	}
	for _, probe := range append(probes, sidecarDumpProbe{}) {
		findings, err := probe.check(t.Context(), sidecarStorageTestEnv(off, func() time.Time { return fileClusterTestNow }))
		if err != nil || len(findings) != 1 || !findings[0].configuredOff {
			t.Errorf("%s with no block: %+v %v", probe.id(), findings, err)
		}
	}
	for _, probe := range probes {
		findings, err := probe.check(t.Context(), sidecarStorageTestEnv(broken, func() time.Time { return fileClusterTestNow }))
		if err != nil || len(findings) != 1 || findings[0].configuredOff || !findings[0].isUnobservable() {
			t.Errorf("%s with a block that did not read: %+v %v", probe.id(), findings, err)
		}
	}
	dump := loadSidecarStorageSettings(nil, &sidecarDumpMonitorYaml{Enabled: true}, nil)
	if !dump.SidecarDump || dump.FileClusters {
		t.Errorf("the dump block alone: %+v", dump)
	}
}

// The generation check compares the settings without the process seams: two loads of one vault file are
// equal, and a cluster added is not.
func TestSidecarStorageSettingsCompareWithoutTheirSeams(t *testing.T) {
	load := func(clusterIds ...string) SignalSettings {
		settings := DefaultSidecarStorageSettings()
		settings.FileClusters = true
		settings.AdminSource = newFakeFileClusterAdminSource()
		settings.Gauges = newFakeSidecarStorageGauges()
		settings.fileClusterIds = clusterIds
		return SignalSettings{SidecarStorage: settings}
	}
	if !reflect.DeepEqual(comparableSignalSettings(load("meta-1", "seg-1")), comparableSignalSettings(load("meta-1", "seg-1"))) {
		t.Error("two loads of one file compare unequal")
	}
	if reflect.DeepEqual(comparableSignalSettings(load("meta-1", "seg-1")), comparableSignalSettings(load("meta-1", "meta-2", "seg-1"))) {
		t.Error("a cluster added compares equal")
	}
	if categories := settingsMismatchCategories(comparableSignalSettings(load("meta-1")), comparableSignalSettings(load("meta-2"))); !slices.Contains(categories, "sidecar_storage") {
		t.Errorf("categories %v", categories)
	}
}

// The prometheus gauges: each set 1 under its state and 0 under the others, a set gone loses its series,
// and rating days go when unknown.
func TestSidecarStorageGaugesPublishEachStateAndForgetGoneSets(t *testing.T) {
	registry := prometheus.NewRegistry()
	gauges := NewPrometheusSidecarStorageGauges(registry)
	gauges.PublishSetStates("meta-1", map[string]string{"0-0": fileClusterStateDegraded, "0-1": fileClusterStateUnknown})
	if again := NewPrometheusSidecarStorageGauges(registry); again == nil {
		t.Fatal("registering twice")
	}
	if count := testutil.CollectAndCount(registry, fileClusterSetStateMetricName); count != 8 {
		t.Fatalf("%d series", count)
	}
	value := func(set string, state string) float64 {
		return testutil.ToFloat64(gauges.(*prometheusSidecarStorageGauges).setStateGauge.WithLabelValues("meta-1", set, state))
	}
	if value("0-0", fileClusterStateDegraded) != 1 || value("0-0", fileClusterStateProtected) != 0 || value("0-1", fileClusterStateUnknown) != 1 {
		t.Error("states")
	}
	gauges.PublishSetStates("meta-1", map[string]string{"0-0": fileClusterStateAtRisk})
	if count := testutil.CollectAndCount(registry, fileClusterSetStateMetricName); count != 4 {
		t.Errorf("%d series after a set went", count)
	}
	gauges.PublishDriveRatingDays("meta-1", math.Inf(1), true)
	if count := testutil.CollectAndCount(registry, fileClusterDriveRatingDaysMetricName); count != 1 {
		t.Errorf("%d rating series", count)
	}
	gauges.PublishDriveRatingDays("meta-1", 0, false)
	if count := testutil.CollectAndCount(registry, fileClusterDriveRatingDaysMetricName); count != 0 {
		t.Errorf("%d rating series once unknown", count)
	}
}

// The alerts a run of signal hands the notifiers: its findings that are not healthy, observed at now.
func sidecarStorageTestAlerts(signal Signal, findings []finding, now func() time.Time) Alerts {
	alerts := Alerts{}
	for _, finding := range findings {
		if !finding.healthy {
			alerts = append(alerts, alertFromFinding(SignalSettings{Environment: "synthetic", Now: now}, signal.Number(), signal.Key(), signal.Name(), finding))
		}
	}
	return alerts
}

// FS-50's file-cluster and budget conditions at 02:00: a set at_risk, a cluster refusing writes, a key held
// past a minute and P0 past the refill each reach the rota's pager sink at once and page the second person
// at 30 minutes unacknowledged, not before.
func TestFileClusterPageConditionsAt0200ReachThePager(t *testing.T) {
	type condition struct {
		class    string
		signal   Signal
		findings func(harness *pagerHarness) []finding
	}
	conditions := []condition{
		{
			class: fileClusterStateClassAtRisk, signal: NewFileClusterStateSignal(),
			findings: func(harness *pagerHarness) []finding {
				source := newFakeFileClusterAdminSource()
				setFileClusterTestHost(source.info("meta-1"), 0, "offline")
				setFileClusterTestHost(source.info("meta-1"), 1, "offline")
				return checkFileClusterState(t, newFileClusterStateProbe(), source, nil, harness.clock.Now())
			},
		},
		{
			class: fileClusterWritesClassRefusing, signal: NewFileClusterWritesSignal(),
			findings: func(harness *pagerHarness) []finding {
				source := newFakeFileClusterAdminSource()
				for _, hostIndex := range []int{0, 1} {
					setFileClusterTestHost(source.info("meta-1"), hostIndex, "offline")
				}
				findings, err := fileClusterWritesProbe{}.check(t.Context(), sidecarStorageTestEnv(sidecarStorageTestSettings(source, nil), harness.clock.Now))
				if err != nil {
					t.Fatal(err)
				}
				return findings
			},
		},
		{
			class: fileClusterSettleClassKeyHeld, signal: NewFileClusterSettleSignal(),
			findings: func(harness *pagerHarness) []finding {
				settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
				return fileClusterSettleProbe{}.evaluate(settings, []string{"meta-1"}, []*sidecarStorageSample{
					sidecarStorageTestSample(fileClusterSettleCheckHeld, 95, "cluster", "meta-1", "host", "store-1"),
				})
			},
		},
		{
			class: writeBudgetClassP0PastRefill, signal: NewWriteBudgetSignal(),
			findings: func(harness *pagerHarness) []finding {
				settings := sidecarStorageTestSettings(newFakeFileClusterAdminSource(), nil)
				return writeBudgetProbe{}.evaluate(settings, []string{"meta-1"}, nil, []*sidecarStorageSample{
					sidecarStorageTestSample(writeBudgetCheckRatio, 1.4, "cluster", "meta-1"),
					sidecarStorageTestSample(writeBudgetCheckBucket, 0, "cluster", "meta-1"),
				})
			},
		},
	}
	for _, condition := range conditions {
		if _, paged := pageClassOf(condition.class); !paged {
			t.Fatalf("%s has no page class row", condition.class)
		}
		harness := newPagerHarness()
		notifier := NewPagerAlertNotifier(harness.settings())
		alerts := sidecarStorageTestAlerts(condition.signal, condition.findings(harness), harness.clock.Now)
		notifier.Handle(condition.signal, SignalExecution{Alerts: alerts})
		flushPager(t, notifier)
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
		notification := harness.sink.Notifications()[0]
		if notification.Page.Class != condition.class || notification.Page.BothAtOnce || !notification.Time.Equal(fileClusterTestNow) {
			t.Fatalf("%s paged %+v", condition.class, notification)
		}
		harness.clock.Advance(30*time.Minute - time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson)
		harness.clock.Advance(time.Second)
		harness.sink.Tick()
		requirePagedPeople(t, harness.sink, pager.FakeFirstPerson, pager.FakeSecondPerson)
		closePager(t, notifier)
	}
}
