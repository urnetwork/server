// The mesh capacity gauges (mesh_metrics.go, monitor/SIGNALS.md §28.4).
//
// Nothing here touches the database: the operator and the member are real
// nodes on loopback, built through the constructors the service uses.

package gossip

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	connectgossip "github.com/urnetwork/connect/v2026/gossip"
)

// What one gather of the collector exports: each series by its name and its
// level label, if any.
func gatherGossipMesh(t *testing.T, collector prometheus.Collector) map[string]float64 {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			key := family.GetName()
			for _, label := range metric.GetLabel() {
				key += "/" + label.GetName() + "=" + label.GetValue()
			}
			values[key] = metric.GetGauge().GetValue()
		}
	}
	return values
}

// The operator node exports the links it holds and the watermarks it trims
// at, the ones run.go builds it with, and exports nothing once its run ends:
// a missing series is unknown to a reader, never zero links.
func TestGossipOperatorExportsItsLinksAndWatermarks(t *testing.T) {
	config := newTestConfig(t)

	operator := newTestOperator(t, config, 0)
	closed := false
	defer func() {
		if !closed {
			operator.close()
		}
	}()

	values := gatherGossipMesh(t, gossipMeshMetrics)
	if values["urnetwork_gossip_conn_manager_watermark/level=low"] != connManagerLowWater ||
		values["urnetwork_gossip_conn_manager_watermark/level=high"] != connManagerHighWater {
		t.Fatalf("watermarks = %v, want %d/%d", values, connManagerLowWater, connManagerHighWater)
	}
	if peers, ok := values["urnetwork_gossip_peers"]; !ok || peers != 0 {
		t.Fatalf("a replica no member dialed exports peers %v (%t), want 0", peers, ok)
	}
	if _, ok := values["urnetwork_gossip_mesh_peers"]; !ok {
		t.Fatalf("no mesh peer series: %v", values)
	}

	member := newTestMember(t, config, operator.addrs(t))
	defer member.close()
	waitForNodeStatus(t, operator.node, "a member linked", func(status connectgossip.NodeStatus) bool {
		return 1 <= status.PeerCount
	})
	values = gatherGossipMesh(t, gossipMeshMetrics)
	if values["urnetwork_gossip_peers"] != 1 {
		t.Fatalf("one member linked, but peers = %v", values)
	}

	operator.close()
	closed = true
	if values := gatherGossipMesh(t, gossipMeshMetrics); len(values) != 0 {
		t.Fatalf("a replica whose run ended still exports %v", values)
	}
}

// A later node replaces an earlier one, and an observation reads the node's
// status at every gather rather than once.
func TestGossipMeshCollectorReadsTheCurrentNode(t *testing.T) {
	collector := &gossipMeshCollector{}
	if values := gatherGossipMesh(t, collector); len(values) != 0 {
		t.Fatalf("no node yet, but the collector exports %v", values)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := connectgossip.NodeStatus{PeerCount: 1007, MeshPeerCount: 31}
	collector.observe(ctx, func() connectgossip.NodeStatus { return status }, 512, 1024)
	values := gatherGossipMesh(t, collector)
	if values["urnetwork_gossip_peers"] != 1007 || values["urnetwork_gossip_mesh_peers"] != 31 {
		t.Fatalf("values = %v", values)
	}
	status = connectgossip.NodeStatus{PeerCount: 1030, MeshPeerCount: 30}
	if values := gatherGossipMesh(t, collector); values["urnetwork_gossip_peers"] != 1030 {
		t.Fatalf("the gather did not read the status again: %v", values)
	}

	nextCtx, nextCancel := context.WithCancel(context.Background())
	defer nextCancel()
	collector.observe(nextCtx, func() connectgossip.NodeStatus { return connectgossip.NodeStatus{PeerCount: 3} }, 8, 16)
	cancel()
	values = gatherGossipMesh(t, collector)
	if values["urnetwork_gossip_peers"] != 3 || values["urnetwork_gossip_conn_manager_watermark/level=high"] != 16 {
		t.Fatalf("the later node did not replace the earlier one: %v", values)
	}
	nextCancel()
	if values := gatherGossipMesh(t, collector); len(values) != 0 {
		t.Fatalf("an ended run still exports %v", values)
	}
}
