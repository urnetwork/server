package gossip

// What the operator's gossip node holds, for the mesh capacity signal
// (monitor/SIGNALS.md §28.4; factury/research/gossip-drop-2026-11-11.md §2).
//
// The connection manager of a replica keeps at most its high watermark of
// links. Above it, every 10 seconds, it closes the links older than its
// 60-second grace period down to the low watermark, and the members it closed
// redial at once. So a replica that more members want a link from than its
// high watermark loops: on 2026-11-11 about 13-15k members wanted links from
// four replicas of 512/1024, and each replica closed the excess at about 67
// seconds, about 10k websocket closures a minute, until a 12.5k-device fleet
// went offline at 10:35Z. Nothing exported the link count, so the loop read
// as ordinary churn and the drop that ended it read as a gossip outage.
//
// These gauges export the links the node holds and the watermarks it trims
// at, so a replica nearing its high watermark is visible before it starts the
// loop. The values are read from the node when the stats pusher collects, a
// cheap read of its status value (connect gossip Node.Status). Only the
// watermark carries a label, its level, so the series count is fixed. A
// replica exports nothing before its node exists or after its run ended,
// which a reader takes as unknown rather than as zero links.

import (
	"context"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	connectgossip "github.com/urnetwork/connect/v2026/gossip"
)

var (
	gossipPeersDesc = prometheus.NewDesc(
		"urnetwork_gossip_peers",
		"Peers the operator's gossip node holds a connection to: every member and extender linked to this replica",
		nil, nil,
	)
	gossipMeshPeersDesc = prometheus.NewDesc(
		"urnetwork_gossip_mesh_peers",
		"Peers in the operator gossip node's topic mesh, which is what carries the extender records it publishes",
		nil, nil,
	)
	gossipConnManagerWatermarkDesc = prometheus.NewDesc(
		"urnetwork_gossip_conn_manager_watermark",
		"The operator gossip node's connection manager watermarks: above high it closes links past their 60 s grace period down to low, and the members it closed redial",
		[]string{"level"}, nil,
	)
)

// The node a replica runs, while its run lasts.
type gossipMeshObservation struct {
	ctx       context.Context
	status    func() connectgossip.NodeStatus
	lowWater  int
	highWater int
}

type gossipMeshCollector struct {
	stateLock   sync.Mutex
	observation *gossipMeshObservation
}

var gossipMeshMetrics = &gossipMeshCollector{}

func init() {
	prometheus.MustRegister(gossipMeshMetrics)
}

// observe exports the node's links and the watermarks it was built with until
// ctx ends. A later node replaces an earlier one: a process runs one replica.
func (self *gossipMeshCollector) observe(
	ctx context.Context,
	status func() connectgossip.NodeStatus,
	lowWater int,
	highWater int,
) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.observation = &gossipMeshObservation{
		ctx:       ctx,
		status:    status,
		lowWater:  lowWater,
		highWater: highWater,
	}
}

// The observation while its run lasts, else nil.
func (self *gossipMeshCollector) current() *gossipMeshObservation {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.observation == nil || self.observation.ctx.Err() != nil {
		return nil
	}
	return self.observation
}

func (self *gossipMeshCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- gossipPeersDesc
	ch <- gossipMeshPeersDesc
	ch <- gossipConnManagerWatermarkDesc
}

func (self *gossipMeshCollector) Collect(ch chan<- prometheus.Metric) {
	observation := self.current()
	if observation == nil {
		return
	}
	status := observation.status()
	ch <- prometheus.MustNewConstMetric(gossipPeersDesc, prometheus.GaugeValue, float64(status.PeerCount))
	ch <- prometheus.MustNewConstMetric(gossipMeshPeersDesc, prometheus.GaugeValue, float64(status.MeshPeerCount))
	ch <- prometheus.MustNewConstMetric(gossipConnManagerWatermarkDesc, prometheus.GaugeValue, float64(observation.lowWater), "low")
	ch <- prometheus.MustNewConstMetric(gossipConnManagerWatermarkDesc, prometheus.GaugeValue, float64(observation.highWater), "high")
}
