package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// §28.4 over synthetic series: 30 one-minute evaluations, the longest hold, of
// the four gossip replicas of Main.

const gossipMeshTestPoints = 30

var gossipMeshTestHosts = []string{"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"}

// Each part by host, one value per evaluation; a nil map leaves the part out.
type gossipMeshTestSeries struct {
	peers  map[string][]float64
	fds    map[string][]float64
	high   map[string][]float64
	heap   map[string][]float64
	totals map[string][]float64
}

// A gossip build that exports its links: four replicas under 80% of 1024,
// with heaps under a gigabyte, and their links steady for three hours.
func gossipMeshTestHealthy() gossipMeshTestSeries {
	series := gossipMeshTestSeries{
		peers: map[string][]float64{}, fds: map[string][]float64{}, high: map[string][]float64{}, heap: map[string][]float64{},
		totals: map[string][]float64{
			"peers_total": stormRepeat(2850, gossipMeshTestPoints),
			"peers_peak":  stormRepeat(2900, gossipMeshTestPoints),
			"fds_total":   stormRepeat(3050, gossipMeshTestPoints),
			"fds_peak":    stormRepeat(3100, gossipMeshTestPoints),
		},
	}
	for index, host := range gossipMeshTestHosts {
		peers := []float64{700, 600, 800, 750}[index]
		series.peers[host] = stormRepeat(peers, gossipMeshTestPoints)
		series.fds[host] = stormRepeat(peers+50, gossipMeshTestPoints)
		series.high[host] = stormRepeat(1024, gossipMeshTestPoints)
		series.heap[host] = stormRepeat(0.4*1024*1024*1024, gossipMeshTestPoints)
	}
	return series
}

// The same replicas on a gossip build that predates the link gauges.
func gossipMeshTestOldBuild() gossipMeshTestSeries {
	series := gossipMeshTestHealthy()
	series.peers = map[string][]float64{}
	series.high = map[string][]float64{}
	delete(series.totals, "peers_total")
	delete(series.totals, "peers_peak")
	return series
}

func gossipMeshTestResponse(t *testing.T, series gossipMeshTestSeries) string {
	t.Helper()
	synthetic := []stormSyntheticSeries{}
	for part, byHost := range map[string]map[string][]float64{"peers": series.peers, "fds": series.fds, "high": series.high, "heap": series.heap} {
		for host, values := range byHost {
			synthetic = append(synthetic, stormSyntheticSeries{part: part, labels: map[string]string{"host": host}, values: values})
		}
	}
	for part, values := range series.totals {
		synthetic = append(synthetic, stormSyntheticSeries{part: part, labels: map[string]string{}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(gossipMeshTestPoints), time.Minute, synthetic)
}

// Values that change at a step: before for the first count, after for the
// rest.
func gossipMeshTestStep(before float64, after float64, count int) []float64 {
	values := stormRepeat(after, gossipMeshTestPoints)
	for index := range count {
		values[index] = before
	}
	return values
}

func TestGossipMeshJudgesCapacityLinksAndHeap(t *testing.T) {
	probe := gossipMeshProbe{}
	const (
		edge3Saturated = "gossip-mesh-saturated main/gossip-mesh/by-us-fmt-5-edge-3"
		edge0Saturated = "gossip-mesh-saturated main/gossip-mesh/by-us-fmt-5-edge-0"
		edge3Heap      = "gossip-heap-high main/gossip-heap/by-us-fmt-5-edge-3"
		edge0Heap      = "gossip-heap-high main/gossip-heap/by-us-fmt-5-edge-0"
		dropped        = "gossip-mesh-links-dropped main/gossip-mesh"
		visibility     = "gossip-mesh-unobservable main/gossip-mesh"
	)

	// edge-3 a few dozen links under its watermark for half an hour
	near := gossipMeshTestHealthy()
	near.peers["by-us-fmt-5-edge-3"] = stormRepeat(960, gossipMeshTestPoints)

	// 2026-11-11 before 10:35Z on the old build: 3,900 open fds against
	// 1024, the trim loop, for the last ten minutes only
	trimming := gossipMeshTestOldBuild()
	trimming.fds["by-us-fmt-5-edge-3"] = gossipMeshTestStep(700, 3894, gossipMeshTestPoints-10)

	// edge-3's heap on 2026-11-11: 5.4 GB
	heap := gossipMeshTestHealthy()
	heap.heap["by-us-fmt-5-edge-3"] = stormRepeat(5.4e9, gossipMeshTestPoints)

	// 10:35Z: the fleet left, links 3,296 of 15,576 for twenty minutes
	departed := gossipMeshTestOldBuild()
	departed.totals["fds_total"] = gossipMeshTestStep(15576, 3296, 10)
	departed.totals["fds_peak"] = stormRepeat(15576, gossipMeshTestPoints)

	// a replica restarted three minutes ago: its links back within two
	restarted := gossipMeshTestHealthy()
	restarted.totals["peers_total"] = gossipMeshTestStep(2850, 2850, 0)
	restarted.totals["peers_total"][gossipMeshTestPoints-3] = 2140
	restarted.totals["peers_total"][gossipMeshTestPoints-2] = 1100

	// edge-4's replica exports nothing
	missing := gossipMeshTestHealthy()
	delete(missing.peers, "by-us-fmt-5-edge-4")
	delete(missing.fds, "by-us-fmt-5-edge-4")
	delete(missing.high, "by-us-fmt-5-edge-4")
	delete(missing.heap, "by-us-fmt-5-edge-4")

	for _, test := range []struct {
		name   string
		series gossipMeshTestSeries
		want   map[string]string
	}{
		{name: "healthy", series: gossipMeshTestHealthy(), want: map[string]string{
			edge3Saturated: "healthy", edge0Saturated: "healthy", edge3Heap: "healthy", dropped: "healthy", visibility: "healthy",
		}},
		{name: "the old build, from its open fds", series: gossipMeshTestOldBuild(), want: map[string]string{
			edge3Saturated: "healthy", edge3Heap: "healthy", dropped: "healthy", visibility: "healthy",
		}},
		{name: "a replica near its watermark", series: near, want: map[string]string{
			edge3Saturated: "warn", edge0Saturated: "healthy", visibility: "healthy",
		}},
		{name: "a replica trimming", series: trimming, want: map[string]string{
			edge3Saturated: "warn", edge0Saturated: "healthy",
		}},
		{name: "a replica's heap", series: heap, want: map[string]string{
			edge3Heap: "warn", edge0Heap: "healthy", edge3Saturated: "healthy",
		}},
		{name: "the fleet left", series: departed, want: map[string]string{
			dropped: "warn", edge3Saturated: "healthy",
		}},
		{name: "a replica restarted", series: restarted, want: map[string]string{
			dropped: "hold",
		}},
		{name: "a replica exports nothing", series: missing, want: map[string]string{
			"gossip-mesh-saturated main/gossip-mesh/by-us-fmt-5-edge-4": "hold",
			edge3Saturated: "healthy",
			visibility:     "unobservable",
		}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, gossipMeshTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// The alerts say where the links were read from and against which watermark,
// and the query reads the series §28.4 names.
func TestGossipMeshAlertsNameTheirSources(t *testing.T) {
	settings := DefaultGossipMeshSettings()
	if settings.DefaultHighWater != 1024 || settings.NearShare != 0.9 || settings.NearHold != 30*time.Minute || settings.OverHold != 10*time.Minute ||
		settings.DropShare != 0.5 || settings.DropWindow != 3*time.Hour || settings.DropHold != 15*time.Minute ||
		settings.HeapMaxBytes != 4*1024*1024*1024 || settings.HeapHold != 30*time.Minute {
		t.Fatalf("§28.4 policy changed: %+v", settings)
	}
	query := gossipMeshQuery("main", settings)
	for _, want := range []string{
		`max by (host) (urnetwork_gossip_peers{env="main",service="gossip"})`,
		`max by (host) (process_open_fds{env="main",service="gossip"})`,
		`max by (host) (urnetwork_gossip_conn_manager_watermark{env="main",service="gossip",level="high"})`,
		`max by (host) (go_memstats_heap_inuse_bytes{env="main",service="gossip"})`,
		`max_over_time(sum(max by (host) (urnetwork_gossip_peers{env="main",service="gossip"}))[3h:1m])`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	trimming := gossipMeshTestOldBuild()
	trimming.fds["by-us-fmt-5-edge-3"] = stormRepeat(3894, gossipMeshTestPoints)
	trimming.heap["by-us-fmt-5-edge-3"] = stormRepeat(5.4e9, gossipMeshTestPoints)
	trimming.totals["fds_total"] = stormRepeat(3296, gossipMeshTestPoints)
	trimming.totals["fds_peak"] = stormRepeat(15576, gossipMeshTestPoints)
	alerts := runStormSynthetic(t, NewGossipMeshSignal(), stormSyntheticNow, gossipMeshTestResponse(t, trimming), nil)
	found := map[string]bool{}
	for _, alert := range alerts {
		if alert.SignalNumber != "28.4" || alert.Severity != SeverityWarn {
			continue
		}
		switch alert.Class {
		case "gossip-mesh-saturated":
			found[alert.Class] = alert.Target == "main/gossip-mesh/by-us-fmt-5-edge-3" &&
				strings.Contains(alert.Observed, "links=3894 links_source=open_fds high_water=1024 share=3.80 trimming=yes")
		case "gossip-heap-high":
			found[alert.Class] = strings.Contains(alert.Observed, "heap_inuse_bytes=5400000000")
		case "gossip-mesh-links-dropped":
			found[alert.Class] = strings.Contains(alert.Observed, "links=3296 peak_links=15576 share=0.21 links_source=open_fds")
		}
	}
	for _, class := range []string{"gossip-mesh-saturated", "gossip-heap-high", "gossip-mesh-links-dropped"} {
		if !found[class] {
			t.Fatalf("no %s alert with its sources in %+v", class, alerts)
		}
	}

	// a replica that exports its own watermark is judged against it
	raised := gossipMeshTestHealthy()
	raised.peers["by-us-fmt-5-edge-3"] = stormRepeat(1500, gossipMeshTestPoints)
	raised.high["by-us-fmt-5-edge-3"] = stormRepeat(4096, gossipMeshTestPoints)
	states := stormFindingStates(t, gossipMeshProbe{}, stormSyntheticNow, gossipMeshTestResponse(t, raised), nil)
	requireStormStates(t, "a raised watermark", states, map[string]string{"gossip-mesh-saturated main/gossip-mesh/by-us-fmt-5-edge-3": "healthy"})
}

func TestGossipMeshRejectsInvalidPolicyWithoutReading(t *testing.T) {
	signal := NewGossipMeshSignalWithSettings(&GossipMeshSettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "gossip-mesh-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
