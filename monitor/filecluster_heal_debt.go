package monitor

// The heal debt filecluster-state reads (SIGNALS.md §29.1; §19.6, g0-recheck-1-s11-degraded-write-debt and
// evsb-m3-01-file-cluster-signal-sources, both decided by the owner). The minio fork exports, on each host's node
// metrics, per pool and set: its partial-write queue's depth, the entries the queue lost, the host's unclean
// starts and when the set last lost anything there; and the cluster's last full heal, its start and end, which
// every host reads from one record. xops scrapes each cluster's hosts into Mimir as job="file_cluster_minio" with
// cluster="<cluster id>", and MinIO labels every series with its host, server="<host:port>", the name the
// cluster's admin info lists the host under.
//
// A set's debt is known only when every host admin info lists shows all four of its queue series fresh, and some
// host shows the cluster's full heal: a host that says nothing may hold a queue, so its silence is never a depth
// of zero. Then:
//
//   - depth is the hosts' sum;
//   - lost is a loss on any host that no full heal began after: a heal that began before the loss may have passed
//     the object by;
//   - the full heal is the newest any host read. The fork records one only when a heal of the whole cluster ran
//     to its end and left every item on every drive of its set, so one that ran with a drive offline is none.
//
// This monitor publishes the sums as urnetwork_filecluster_mrf_depth{cluster,set},
// urnetwork_filecluster_mrf_dropped_total{cluster,set} and urnetwork_filecluster_unclean_starts_total{cluster,set},
// and removes a set's series while a host of its cluster is silent.

import (
	"math"
	"strconv"
	"time"

	"github.com/urnetwork/server"
)

// The fork's series (minio's cmd/settle-metrics.go).
const (
	fileClusterHostMrfDepthMetricName      = "urnetwork_filecluster_host_mrf_depth"
	fileClusterHostMrfDroppedMetricName    = "urnetwork_filecluster_host_mrf_dropped_total"
	fileClusterHostUncleanStartsMetricName = "urnetwork_filecluster_host_unclean_starts_total"
	fileClusterHostMrfLastLossMetricName   = "urnetwork_filecluster_host_mrf_last_loss_seconds"
	fileClusterFullHealStartMetricName     = "urnetwork_filecluster_full_heal_start_seconds"
	fileClusterFullHealEndMetricName       = "urnetwork_filecluster_full_heal_end_seconds"
)

// The series this monitor publishes from them (§19.6).
const (
	fileClusterMrfDepthMetricName      = "urnetwork_filecluster_mrf_depth"
	fileClusterMrfDroppedMetricName    = "urnetwork_filecluster_mrf_dropped_total"
	fileClusterUncleanStartsMetricName = "urnetwork_filecluster_unclean_starts_total"
)

// The query's partitions.
const (
	fileClusterDebtCheckDepth     = "mrf_depth"
	fileClusterDebtCheckDropped   = "mrf_dropped"
	fileClusterDebtCheckUnclean   = "unclean_starts"
	fileClusterDebtCheckLastLoss  = "mrf_last_loss"
	fileClusterDebtCheckHealStart = "full_heal_start"
	fileClusterDebtCheckHealEnd   = "full_heal_end"
)

// The partitions a host shows for each set's queue: a host counts for a set only with every one of them.
var fileClusterDebtQueueChecks = []string{
	fileClusterDebtCheckDepth, fileClusterDebtCheckDropped, fileClusterDebtCheckUnclean, fileClusterDebtCheckLastLoss,
}

// The label MinIO names a series' host with, and the fork's labels for a set.
const (
	fileClusterMinioServerLabel = "server"
	fileClusterMinioPoolLabel   = "pool"
	fileClusterMinioSetLabel    = "set"
)

// A set's partial-write queue summed over its cluster's hosts.
type FileClusterQueueDebt struct {
	// partial writes queued or healing
	Depth float64
	// entries the hosts' queues lost
	DroppedCount float64
	// the hosts' starts that followed a process which did not save its queue
	UncleanStartCount float64
}

// Where the state signal publishes the queues' sums: a gauge set that also meets this publishes them.
// Implementations are safe for concurrent use.
type FileClusterHealDebtGauges interface {
	// Replaces the cluster's urnetwork_filecluster_mrf_depth, urnetwork_filecluster_mrf_dropped_total and
	// urnetwork_filecluster_unclean_starts_total series: a set not named loses its series, so a queue some host
	// did not show is never published as empty.
	PublishQueueDebts(clusterId string, setQueues map[string]FileClusterQueueDebt)
}

// The one query: each partition's fresh samples.
func fileClusterHealDebtQuery(environment string, freshness time.Duration) string {
	minio := func(metric string, check string) string {
		return sidecarStorageTag(sidecarStorageFresh(sidecarStorageSelector(environment, metric, "job="+strconv.Quote(fileClusterMinioJob)), freshness), check)
	}
	return sidecarStorageJoin(
		minio(fileClusterHostMrfDepthMetricName, fileClusterDebtCheckDepth),
		minio(fileClusterHostMrfDroppedMetricName, fileClusterDebtCheckDropped),
		minio(fileClusterHostUncleanStartsMetricName, fileClusterDebtCheckUnclean),
		minio(fileClusterHostMrfLastLossMetricName, fileClusterDebtCheckLastLoss),
		minio(fileClusterFullHealStartMetricName, fileClusterDebtCheckHealStart),
		minio(fileClusterFullHealEndMetricName, fileClusterDebtCheckHealEnd),
	)
}

// The time a series gives in Unix seconds, zero for 0.
func fileClusterSeriesTime(seconds float64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.UnixMilli(int64(math.Round(seconds * 1000)))
}

// Every set's heal debt and queue in one cluster, from one query's samples (see the file header). A set whose
// debt is unknown is in neither map, or only in the queues when the full heal alone did not read.
func fileClusterHealDebts(clusterId string, info *server.FileClusterAdminInfo, samples []*sidecarStorageSample) (map[string]*fileClusterHealDebt, map[string]FileClusterQueueDebt) {
	hostListed := map[string]bool{}
	for _, host := range info.Hosts {
		hostListed[host.Endpoint] = true
	}
	// by set, then host, then partition
	setHostQueues := map[string]map[string]map[string]float64{}
	type hostHeal struct {
		start    float64
		hasStart bool
		end      float64
		hasEnd   bool
	}
	hostHeals := map[string]*hostHeal{}
	for _, sample := range samples {
		host := sample.label(fileClusterMinioServerLabel)
		// a series of a host the cluster's admin info does not list answers for none of its hosts
		if sample.label("cluster") != clusterId || !hostListed[host] || !sidecarStorageCount(sample.value) {
			continue
		}
		switch sample.check {
		case fileClusterDebtCheckDepth, fileClusterDebtCheckDropped, fileClusterDebtCheckUnclean, fileClusterDebtCheckLastLoss:
			pool, err := strconv.Atoi(sample.label(fileClusterMinioPoolLabel))
			if err != nil {
				continue
			}
			setIndex, err := strconv.Atoi(sample.label(fileClusterMinioSetLabel))
			if err != nil {
				continue
			}
			set := fileClusterSetName(pool, setIndex)
			hostQueues, ok := setHostQueues[set]
			if !ok {
				hostQueues = map[string]map[string]float64{}
				setHostQueues[set] = hostQueues
			}
			queue, ok := hostQueues[host]
			if !ok {
				queue = map[string]float64{}
				hostQueues[host] = queue
			}
			queue[sample.check] = sample.value
		case fileClusterDebtCheckHealStart, fileClusterDebtCheckHealEnd:
			heal, ok := hostHeals[host]
			if !ok {
				heal = &hostHeal{}
				hostHeals[host] = heal
			}
			if sample.check == fileClusterDebtCheckHealStart {
				heal.start, heal.hasStart = sample.value, true
			} else {
				heal.end, heal.hasEnd = sample.value, true
			}
		}
	}

	// the newest full heal any host read, with that host's start of it
	healKnown := false
	var healStart, healEnd float64
	for _, heal := range hostHeals {
		if !heal.hasStart || !heal.hasEnd {
			continue
		}
		if !healKnown || healEnd < heal.end {
			healStart, healEnd = heal.start, heal.end
		}
		healKnown = true
	}

	debts := map[string]*fileClusterHealDebt{}
	queues := map[string]FileClusterQueueDebt{}
	for _, drive := range info.Drives {
		set := fileClusterSetName(drive.PoolIndex, drive.SetIndex)
		if _, done := queues[set]; done {
			continue
		}
		queue := FileClusterQueueDebt{}
		lastLoss := 0.0
		complete := 0 < len(info.Hosts)
		for _, host := range info.Hosts {
			hostQueue := setHostQueues[set][host.Endpoint]
			if len(hostQueue) != len(fileClusterDebtQueueChecks) {
				complete = false
				break
			}
			queue.Depth += hostQueue[fileClusterDebtCheckDepth]
			queue.DroppedCount += hostQueue[fileClusterDebtCheckDropped]
			queue.UncleanStartCount += hostQueue[fileClusterDebtCheckUnclean]
			lastLoss = max(lastLoss, hostQueue[fileClusterDebtCheckLastLoss])
		}
		if !complete {
			continue
		}
		queues[set] = queue
		if !healKnown {
			continue
		}
		debts[set] = &fileClusterHealDebt{
			known: true,
			depth: queue.Depth,
			// a loss no full heal began after: one that began at or before it may have passed the object by
			lost:              0 < lastLoss && (healStart == 0 || healStart <= lastLoss),
			fullHealStartTime: fileClusterSeriesTime(healStart),
			fullHealEndTime:   fileClusterSeriesTime(healEnd),
		}
	}
	return debts, queues
}
