package monitor

// The sidecar's storage signals, SIGNALS.md §29 (EVSB-M3-01): §19.6's file clusters, §11.13's write budget and
// D-OPS-18's sidecar dump, as §19.8 pages and warns on them (D-OPS-9, owner decision 143). What they share:
//
//   - monitor.yml's file_clusters and sidecar_dump blocks (config.go). Without its block a signal reports
//     itself configured off, which raises no incident: today's production has no file cluster. With a block
//     that does not read it reports itself blind, which holds its other incidents open.
//   - the clusters' admin info, read through vault's file-clusters-monitor.yml, whose MinIO user holds admin
//     reads alone (OPS-M2-04), on server.FileClusters with NoChecks: each signal reads it at its own cadence.
//   - one bounded instant query of the loopback Mimir through a services host, for the series the budget loop,
//     the processes, the storage hosts and the dumps push. Mimir's lag suits these signals; the budget loop
//     itself never reads Mimir.
//   - the gauges this monitor owns, §19.6's urnetwork_filecluster_set_state and §19.8's
//     urnetwork_filecluster_drive_rating_days on SidecarStorageGauges, urnetwork_filecluster_exhaustion_days on
//     FileClusterExhaustionGauges, and §19.6's urnetwork_filecluster_mrf_depth, urnetwork_filecluster_mrf_dropped_total
//     and urnetwork_filecluster_unclean_starts_total on FileClusterHealDebtGauges, which by default register in the
//     process's prometheus registry; the continuous CLI pushes that registry under warp (server/grafana.go).
//
// No series, finding or page names a network or repo (D-OPS-8, I-13): clusters, sets, hosts, devices and
// processes only.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
)

// The clusters' admin info as monitor reads it: *server.FileClusters with NoChecks meets it.
type FileClusterAdminSource interface {
	// Every cluster, the first first.
	ClusterIds() []string
	// server.FileClusterKindMetadata or server.FileClusterKindSegment.
	ClusterKind(clusterId string) string
	// One signed read of the cluster's admin info now.
	ReadAdminInfo(ctx context.Context, clusterId string) (*server.FileClusterAdminInfo, error)
}

// Where the storage signals publish the gauges this monitor owns. Implementations are safe for concurrent
// use.
type SidecarStorageGauges interface {
	// Replaces the cluster's urnetwork_filecluster_set_state series: each set 1 under its state and 0 under
	// the others; a set no longer named loses its series.
	PublishSetStates(clusterId string, setStates map[string]string)
	// Sets the cluster's urnetwork_filecluster_drive_rating_days, or with known false removes it.
	PublishDriveRatingDays(clusterId string, days float64, known bool)
}

// urnetwork_filecluster_set_state's state label: §19.6's three, and unknown while an input it needs is
// missing, so a series never claims a state no input showed.
const (
	fileClusterStateProtected = "protected"
	fileClusterStateDegraded  = "degraded"
	fileClusterStateAtRisk    = "at_risk"
	fileClusterStateUnknown   = "unknown"
)

var fileClusterStates = []string{fileClusterStateProtected, fileClusterStateDegraded, fileClusterStateAtRisk, fileClusterStateUnknown}

// The gauges' names (§19.6, §19.8).
const (
	fileClusterSetStateMetricName        = "urnetwork_filecluster_set_state"
	fileClusterDriveRatingDaysMetricName = "urnetwork_filecluster_drive_rating_days"
	fileClusterExhaustionDaysMetricName  = "urnetwork_filecluster_exhaustion_days"
)

// Where the cohort signal publishes §19.8's urnetwork_filecluster_exhaustion_days (SIGNALS.md §29.7): a gauge set
// that also meets this publishes them. Implementations are safe for concurrent use.
type FileClusterExhaustionGauges interface {
	// Sets the cluster's days until resource is exhausted at its 30-day slope, +Inf while it does not grow, or
	// with known false removes the series.
	PublishExhaustionDays(clusterId string, resource string, days float64, known bool)
}

// The storage signals' settings: monitor.yml's file_clusters and sidecar_dump blocks (config.go) and every
// line §19.6 and §19.8 draw. DefaultSidecarStorageSettings is the only source of a default.
type SidecarStorageSettings struct {
	// monitor.yml has a file_clusters block: the file-cluster and write-budget signals run. false reports
	// them configured off.
	FileClusters bool
	// Why that block, or vault's file-clusters-monitor.yml, did not read. Set, those signals report
	// themselves blind. Never holds a credential.
	FileClustersConfigError string
	// The clusters' admin info: nil unless the block read. A process seam: the generation check compares
	// fileClusterIds in its place.
	AdminSource FileClusterAdminSource
	// The clusters the vault file named when it loaded.
	fileClusterIds []string
	// monitor.yml has a sidecar_dump block: the dump signal runs. false reports it configured off.
	SidecarDump bool
	// Where the gauges go. nil publishes nothing. A process seam, which the generation check ignores.
	Gauges SidecarStorageGauges

	// How old a Mimir sample may be and still count: 3 min, several missed 15 s pushes. A stale series is a
	// missing one.
	MetricFreshness time.Duration
	// The most of one Mimir answer read, past which the query fails and its signal reads blind: 1 MiB, ten
	// times a large estate's answer.
	MimirResponseMaxByteCount int
	// How long one Mimir query may take: 15 s, as the minio-storage signal's, inside every cadence here.
	MimirQueryTimeout time.Duration
	// §19.6's RTO: a host offline longer than this puts every set it carries at_risk. 4 h.
	HostOfflineAtRisk time.Duration
	// Consecutive failing runs before a page class opens: 2, a minute at the 30 s signals' cadence,
	// riding out one lost read.
	PageSustain int
	// Consecutive failing runs before a warn opens: 2.
	WarnSustain int
	// §19.8's held-key page: a key held longer than this by a write that cannot settle. 1 min.
	HeldKeyPage time.Duration
	// §19.8's clock warn: an offset past this, or an unknown one. 25 ms, half of §19.6's 50 ms bound.
	ClockWarnOffset time.Duration
	// §19.8's drive-write lines on the busiest drive: its last 24 h past 0.8 DWPD warns, its 7-day mean
	// over 0.5 warns and over 0.75 pages in business hours.
	DriveDwpdDayWarn      float64
	DriveDwpdWeekWarn     float64
	DriveDwpdWeekBusiness float64
	// SMART available spare under this percent warns: 10.
	DriveSpareWarnPercent float64
	// The span SMART's percentage used is taken at: 30 d.
	RatingWindow time.Duration
	// A drive whose rated endurance at that rate lasts less than this pages in business hours: five
	// years of service.
	ServiceLife time.Duration
	// A metadata drive's bytes, which turn forgiven excess into drive-writes: the budget's DriveByteCount.
	DriveByteCount float64
	// The drive-writes five years at the refill leave under the drives' rating, which forgiven excess is
	// shown against: ~420 (§19.6's wear row).
	ForgivenDriveWritesHeadroom float64
	// The write budget's bucket depth D in drive bytes: the budget's refill over its Depth.
	BucketDepthByteCount float64
	// The bucket under this share of D warns: 0.25.
	BucketLowShare float64
	// The bucket at zero this long, P0's excess forgiven throughout, warns: 5 min.
	BucketEmptyWindow time.Duration
	// P0's warn line, a share of the refill, and how long P0 must stay past it: D-TEST-11 sets the line at
	// four times the hourly share its run measures, at most half the refill, so half until that run sets
	// it; an hour.
	P0WarnLineShare  float64
	P0WarnLineWindow time.Duration
	// The span shed P1 work is counted over: 5 min.
	ShedWindow time.Duration
	// HDD admission refusals past this share of a day's uploads warn: 0.1% over 24 h.
	HddRefusalShare  float64
	HddRefusalWindow time.Duration
	// No dump run ended in this long is a night skipped: 25 h, a day and the run's 20 min past 02:30.
	DumpRunMaxAge time.Duration
	// The newest verified off-site generation older than this warns: 26 h.
	DumpOffsiteMaxAge time.Duration

	// §19.9's cohorts row (SIGNALS.md §29.7): a cluster's exhaustion is the first date its own growth reaches 80%
	// raw, 70% inodes or a 0.75 DWPD 7-day mean on its busiest drive, and its cohort limit, where evs answers 507
	// CLUSTER_FULL, is 80% raw or 70% inodes. Shares of capacity, and DWPD.
	CohortRawLimit   float64
	CohortInodeLimit float64
	CohortWearLimit  float64
	// D-OPS-17's marks, which order the next metadata cluster when reached or projected inside its lead time
	// (§19.9's triggers row, owner decision 136): 50% raw, 40% inodes, a 0.5 DWPD 7-day mean.
	TriggerRawShare   float64
	TriggerInodeShare float64
	TriggerWearDwpd   float64
	// §19.9's hdd trigger: a segment cluster's 30-day trend of drive operations reaching this share of its
	// measured capacity inside its lead time (g0-recheck-0-hdd-trigger): 0.5.
	HddTriggerShare float64
	// The span each slope is taken over: 30 d.
	CohortTrendWindow time.Duration
	// The lead times projections are held to: the move tool's (§19.9's cohorts row), the next metadata cluster's
	// (§19.9's triggers row) and the segment placement's (hdd_trigger_lead_days), each 26 weeks until §23.7 sets
	// its own (g0-recheck-0-hdd-trigger #3).
	MoveToolLeadTime    time.Duration
	NextClusterLeadTime time.Duration
	HddTriggerLeadTime  time.Duration
}

// §19.6's and §19.8's lines, and the write budget's own defaults for its drive and depth.
func DefaultSidecarStorageSettings() *SidecarStorageSettings {
	budget := evsservice.DefaultWriteBudgetSettings()
	refillBytesPerSecond := budget.RefillDwpd * float64(budget.DriveByteCount) / (24 * time.Hour).Seconds()
	return &SidecarStorageSettings{
		MetricFreshness:             3 * time.Minute,
		MimirResponseMaxByteCount:   1024 * 1024,
		MimirQueryTimeout:           15 * time.Second,
		HostOfflineAtRisk:           4 * time.Hour,
		PageSustain:                 2,
		WarnSustain:                 2,
		HeldKeyPage:                 1 * time.Minute,
		ClockWarnOffset:             25 * time.Millisecond,
		DriveDwpdDayWarn:            0.8,
		DriveDwpdWeekWarn:           0.5,
		DriveDwpdWeekBusiness:       0.75,
		DriveSpareWarnPercent:       10,
		RatingWindow:                30 * 24 * time.Hour,
		ServiceLife:                 5 * 365 * 24 * time.Hour,
		DriveByteCount:              float64(budget.DriveByteCount),
		ForgivenDriveWritesHeadroom: 420,
		BucketDepthByteCount:        refillBytesPerSecond * budget.Depth.Seconds(),
		BucketLowShare:              0.25,
		BucketEmptyWindow:           5 * time.Minute,
		P0WarnLineShare:             0.5,
		P0WarnLineWindow:            1 * time.Hour,
		ShedWindow:                  5 * time.Minute,
		HddRefusalShare:             0.001,
		HddRefusalWindow:            24 * time.Hour,
		DumpRunMaxAge:               25 * time.Hour,
		DumpOffsiteMaxAge:           26 * time.Hour,
		CohortRawLimit:              0.8,
		CohortInodeLimit:            0.7,
		CohortWearLimit:             0.75,
		TriggerRawShare:             0.5,
		TriggerInodeShare:           0.4,
		TriggerWearDwpd:             0.5,
		HddTriggerShare:             0.5,
		CohortTrendWindow:           30 * 24 * time.Hour,
		MoveToolLeadTime:            182 * 24 * time.Hour,
		NextClusterLeadTime:         182 * 24 * time.Hour,
		HddTriggerLeadTime:          182 * 24 * time.Hour,
	}
}

// A copy whose source and gauges are shared, as they are one process's.
func cloneSidecarStorageSettings(settings *SidecarStorageSettings) *SidecarStorageSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.fileClusterIds = slices.Clone(settings.fileClusterIds)
	return &clone
}

// The settings as the generation check compares them: without the process seams.
func comparableSidecarStorageSettings(settings *SidecarStorageSettings) *SidecarStorageSettings {
	clone := cloneSidecarStorageSettings(settings)
	if clone != nil {
		clone.AdminSource = nil
		clone.Gauges = nil
	}
	return clone
}

// The finding a file-cluster signal returns in place of its checks: configured off without a block, blind
// with one that does not read. ok is false when the signal may run.
func fileClusterSignalGate(settings *SidecarStorageSettings, probeId string, class string) (finding, bool) {
	switch {
	case settings == nil || !settings.FileClusters:
		return finding{
			probeId: probeId, tier: tierWarn, class: class, target: "file-clusters",
			sustain: 1, configuredOff: true,
			symptom: "monitor.yml has no file_clusters block",
		}, true
	case settings.FileClustersConfigError != "" || settings.AdminSource == nil:
		return finding{
			probeId: probeId, tier: tierWarn, class: class, target: "file-clusters",
			sustain: 1, unobservable: true,
			symptom:  "the file clusters are asked for and their settings do not read",
			observed: settings.FileClustersConfigError,
		}, true
	}
	return finding{}, false
}

// One sample of a storage signal's tagged instant query.
type sidecarStorageSample struct {
	// the partition the query tagged it with
	check  string
	labels map[string]string
	value  float64
}

// The label, "" when absent.
func (self *sidecarStorageSample) label(name string) string {
	return self.labels[name]
}

// Runs one instant query through a services host's loopback Mimir and returns its samples. An error means
// the source could not be read, and the caller reports itself blind.
type sidecarStorageQuery func(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error)

// The production query: curl to the loopback Mimir on the first services host that answers, the
// minio-storage signal's path.
func sidecarStorageMimirQuery(ctx context.Context, env *probeEnv, query string) ([]*sidecarStorageSample, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, errors.New("no services host in inventory for the loopback Mimir query")
	}
	settings := env.cfg.sidecarStorage
	limit := settings.MimirResponseMaxByteCount
	command := "curl -fsS --max-time " + strconv.FormatInt(int64(settings.MimirQueryTimeout/time.Second), 10) +
		" --max-filesize " + strconv.Itoa(limit) +
		" --data-urlencode " + shellSingleQuote("query="+query) +
		" --data-urlencode " + shellSingleQuote("time="+strconv.FormatInt(env.now().Unix(), 10)) +
		" 'http://127.0.0.1:3100/prometheus/api/v1/query'"
	output, _, err := mimirShellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command, limit)
	if err != nil {
		return nil, err
	}
	return parseSidecarStorageSamples(output)
}

// The samples of a Mimir instant-query answer, each with its monitor_check partition.
func parseSidecarStorageSamples(output string) ([]*sidecarStorageSample, error) {
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("Mimir status=%q result_type=%q", response.Status, response.Data.ResultType)
	}
	samples := []*sidecarStorageSample{}
	for _, series := range response.Data.Result {
		_, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, err
		}
		labels := map[string]string{}
		for name, labelValue := range series.Metric {
			if name != "monitor_check" {
				labels[name] = labelValue
			}
		}
		samples = append(samples, &sidecarStorageSample{check: series.Metric["monitor_check"], labels: labels, value: value})
	}
	return samples, nil
}

// A selector of metric in environment, with further matchers.
func sidecarStorageSelector(environment string, metric string, matchers ...string) string {
	return metric + "{" + strings.Join(append([]string{"env=" + strconv.Quote(environment)}, matchers...), ",") + "}"
}

// expression's samples tagged with check.
func sidecarStorageTag(expression string, check string) string {
	return "label_replace(" + expression + `,"monitor_check",` + strconv.Quote(check) + `,"__name__",".*")`
}

// selector's samples whose source sample is no older than freshness: an instant query's own timestamp is
// no proof a source is current.
func sidecarStorageFresh(selector string, freshness time.Duration) string {
	return "(" + selector + " and (timestamp(" + selector + ") >= time() - " + strconv.FormatInt(int64(freshness/time.Second), 10) + "))"
}

// A PromQL range of span, in whole seconds.
func sidecarStorageRange(span time.Duration) string {
	return "[" + strconv.FormatInt(int64(span/time.Second), 10) + "s]"
}

// The queries joined into one.
func sidecarStorageJoin(expressions ...string) string {
	return strings.Join(expressions, " or ")
}

// A finite value at or above zero.
func sidecarStorageCount(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && 0 <= value
}

// The cluster ids of kind, in the source's order.
func fileClusterIdsOfKind(source FileClusterAdminSource, kind string) []string {
	clusterIds := []string{}
	for _, clusterId := range source.ClusterIds() {
		if source.ClusterKind(clusterId) == kind {
			clusterIds = append(clusterIds, clusterId)
		}
	}
	return clusterIds
}

// The gauges in a prometheus registry (see the file header). Safe for concurrent use.
type prometheusSidecarStorageGauges struct {
	setStateGauge       *prometheus.GaugeVec
	ratingDaysGauge     *prometheus.GaugeVec
	exhaustionDaysGauge *prometheus.GaugeVec
	queueDebts          *fileClusterQueueDebtCollector

	stateLock sync.Mutex
	// the sets each cluster's last publish named
	clusterSets map[string][]string
}

// The gauges registered in registerer; ones it already holds are taken as they are, so every signal and
// every monitor of a process share one series set.
func NewPrometheusSidecarStorageGauges(registerer prometheus.Registerer) SidecarStorageGauges {
	register := func(gauge *prometheus.GaugeVec) *prometheus.GaugeVec {
		err := registerer.Register(gauge)
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*prometheus.GaugeVec); ok {
				return existing
			}
		}
		return gauge
	}
	queueDebts := newFileClusterQueueDebtCollector()
	if err := registerer.Register(queueDebts); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*fileClusterQueueDebtCollector); ok {
				queueDebts = existing
			}
		}
	}
	return &prometheusSidecarStorageGauges{
		queueDebts: queueDebts,
		setStateGauge: register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: fileClusterSetStateMetricName,
			Help: "§19.6's protection state of each erasure set: 1 under its state, 0 under the others; unknown while an input is missing.",
		}, []string{"cluster", "set", "state"})),
		ratingDaysGauge: register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: fileClusterDriveRatingDaysMetricName,
			Help: "Days until the cluster's first drive's SMART percentage used reaches 100 at its 30-day rate.",
		}, []string{"cluster"})),
		exhaustionDaysGauge: register(prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: fileClusterExhaustionDaysMetricName,
			Help: "Days until the cluster's own growth reaches its cohort line for the resource at its 30-day slope (§19.9's cohorts row).",
		}, []string{"cluster", "resource"})),
		clusterSets: map[string][]string{},
	}
}

func (self *prometheusSidecarStorageGauges) PublishSetStates(clusterId string, setStates map[string]string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, set := range self.clusterSets[clusterId] {
		if _, ok := setStates[set]; !ok {
			for _, state := range fileClusterStates {
				self.setStateGauge.DeleteLabelValues(clusterId, set, state)
			}
		}
	}
	sets := []string{}
	for set, current := range setStates {
		sets = append(sets, set)
		for _, state := range fileClusterStates {
			value := 0.0
			if state == current {
				value = 1
			}
			self.setStateGauge.WithLabelValues(clusterId, set, state).Set(value)
		}
	}
	slices.Sort(sets)
	self.clusterSets[clusterId] = sets
}

func (self *prometheusSidecarStorageGauges) PublishQueueDebts(clusterId string, setQueues map[string]FileClusterQueueDebt) {
	self.queueDebts.publish(clusterId, setQueues)
}

// The queues' sums as a collector: a gauge and two counters whose values are the hosts' own, summed (see
// filecluster_heal_debt.go). Safe for concurrent use.
type fileClusterQueueDebtCollector struct {
	depthDescription   *prometheus.Desc
	droppedDescription *prometheus.Desc
	uncleanDescription *prometheus.Desc

	stateLock sync.Mutex
	// by cluster, then set
	clusterSetQueues map[string]map[string]FileClusterQueueDebt
}

func newFileClusterQueueDebtCollector() *fileClusterQueueDebtCollector {
	labels := []string{"cluster", "set"}
	return &fileClusterQueueDebtCollector{
		depthDescription: prometheus.NewDesc(fileClusterMrfDepthMetricName,
			"Partial writes of the erasure set its cluster's hosts have queued or are healing (§19.6).", labels, nil),
		droppedDescription: prometheus.NewDesc(fileClusterMrfDroppedMetricName,
			"Queued partial writes of the erasure set its cluster's hosts lost: never queued, or taken off unhealed.", labels, nil),
		uncleanDescription: prometheus.NewDesc(fileClusterUncleanStartsMetricName,
			"Starts of the cluster's hosts that followed a process which did not save its partial-write queue.", labels, nil),
		clusterSetQueues: map[string]map[string]FileClusterQueueDebt{},
	}
}

func (self *fileClusterQueueDebtCollector) publish(clusterId string, setQueues map[string]FileClusterQueueDebt) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(setQueues) == 0 {
		delete(self.clusterSetQueues, clusterId)
		return
	}
	queues := map[string]FileClusterQueueDebt{}
	for set, queue := range setQueues {
		queues[set] = queue
	}
	self.clusterSetQueues[clusterId] = queues
}

func (self *fileClusterQueueDebtCollector) Describe(descriptions chan<- *prometheus.Desc) {
	descriptions <- self.depthDescription
	descriptions <- self.droppedDescription
	descriptions <- self.uncleanDescription
}

func (self *fileClusterQueueDebtCollector) Collect(metrics chan<- prometheus.Metric) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for clusterId, setQueues := range self.clusterSetQueues {
		for set, queue := range setQueues {
			metrics <- prometheus.MustNewConstMetric(self.depthDescription, prometheus.GaugeValue, queue.Depth, clusterId, set)
			metrics <- prometheus.MustNewConstMetric(self.droppedDescription, prometheus.CounterValue, queue.DroppedCount, clusterId, set)
			metrics <- prometheus.MustNewConstMetric(self.uncleanDescription, prometheus.CounterValue, queue.UncleanStartCount, clusterId, set)
		}
	}
}

func (self *prometheusSidecarStorageGauges) PublishExhaustionDays(clusterId string, resource string, days float64, known bool) {
	if !known {
		self.exhaustionDaysGauge.DeleteLabelValues(clusterId, resource)
		return
	}
	self.exhaustionDaysGauge.WithLabelValues(clusterId, resource).Set(days)
}

func (self *prometheusSidecarStorageGauges) PublishDriveRatingDays(clusterId string, days float64, known bool) {
	if !known {
		self.ratingDaysGauge.DeleteLabelValues(clusterId)
		return
	}
	self.ratingDaysGauge.WithLabelValues(clusterId).Set(days)
}

// The process's gauges in its default registry, built once.
var defaultSidecarStorageGauges = sync.OnceValue(func() SidecarStorageGauges {
	return NewPrometheusSidecarStorageGauges(prometheus.DefaultRegisterer)
})
