package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	proxyRuntimeFreshness            = 90 * time.Second
	proxyRuntimeHeapWarnBytes        = float64(3 << 30)
	proxyRuntimeObjectWarnCount      = float64(20_000_000)
	proxyRuntimeResidualWarnBytes    = float64(2 << 30)
	proxyRuntimePeerAllowancePerPeer = float64(48 << 10)
)

// Signal proxy-runtime implements SIGNALS.md §14.7b. It joins the newest
// scrape-fresh Proxy process identity to Go-runtime and owner gauges so a
// multi-gigabyte application live set is not conflated with host cache,
// returned message buffers, or the known WireGuard registered-peer shape.
func NewProxyRuntimeSignal() Signal {
	return &signalAdapter{
		number: "14.7b", key: "proxy-runtime", name: "Proxy runtime live-set attribution",
		probe: proxyRuntimeProbe{},
	}
}

type proxyRuntimeProbe struct{}

func (proxyRuntimeProbe) id() string             { return "runtime/proxy-live-set" }
func (proxyRuntimeProbe) tier() string           { return tierWarn }
func (proxyRuntimeProbe) cadence() time.Duration { return time.Minute }

const (
	proxyRuntimeMetricRSS uint16 = 1 << iota
	proxyRuntimeMetricStart
	proxyRuntimeMetricBuild
	proxyRuntimeMetricHeap
	proxyRuntimeMetricObjects
	proxyRuntimeMetricGoroutines
	proxyRuntimeMetricPeers
	proxyRuntimeMetricDevices
	proxyRuntimeMetricDeviceTracked
	proxyRuntimeMetricPoolRetained
	proxyRuntimeMetricNextGC
	proxyRuntimeMetricGOGC
	proxyRuntimeMetricStack
	proxyRuntimeMetricLastGC
	proxyRuntimeMetricAll = proxyRuntimeMetricRSS |
		proxyRuntimeMetricStart |
		proxyRuntimeMetricBuild |
		proxyRuntimeMetricHeap |
		proxyRuntimeMetricObjects |
		proxyRuntimeMetricGoroutines |
		proxyRuntimeMetricPeers |
		proxyRuntimeMetricDevices |
		proxyRuntimeMetricDeviceTracked |
		proxyRuntimeMetricPoolRetained |
		proxyRuntimeMetricNextGC |
		proxyRuntimeMetricGOGC |
		proxyRuntimeMetricStack |
		proxyRuntimeMetricLastGC
)

var proxyRuntimeMetricNames = []string{
	"process_resident_memory_bytes",
	"process_start_time_seconds",
	"urnetwork_build_info",
	"go_memstats_heap_alloc_bytes",
	"go_memstats_heap_objects",
	"go_goroutines",
	"urnetwork_proxy_wg_peers",
	"urnetwork_proxy_devices_live",
	"urnetwork_proxy_device_memory_tracked_used_bytes",
	"urnetwork_message_pool_retained_bytes",
	"go_memstats_next_gc_bytes",
	"go_gc_gogc_percent",
	"go_memstats_stack_inuse_bytes",
	"go_memstats_last_gc_time_seconds",
}

type proxyRuntimeMetrics struct {
	host          string
	block         string
	instance      string
	version       string
	rss           float64
	start         float64
	heap          float64
	objects       float64
	goroutines    float64
	peers         float64
	devices       float64
	deviceTracked float64
	poolRetained  float64
	nextGC        float64
	gogc          float64
	stack         float64
	lastGC        float64
	availableMask uint16
}

func proxyRuntimeQuery(environment string) string {
	parts := make([]string, 0, len(proxyRuntimeMetricNames))
	for _, metricName := range proxyRuntimeMetricNames {
		series := fmt.Sprintf(
			`label_replace(%s{env=%s,job="proxy"},"monitor_metric",%s,"job",".*")`,
			metricName,
			strconv.Quote(environment),
			strconv.Quote(metricName),
		)
		parts = append(parts, fmt.Sprintf(
			`(%s and on(monitor_metric,env,host,block,instance) (timestamp(%s) >= time() - %d))`,
			series,
			series,
			int64(proxyRuntimeFreshness/time.Second),
		))
	}
	return strings.Join(parts, " or ")
}

func (proxyRuntimeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("proxy runtime: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(proxyRuntimeQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("proxy runtime: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("proxy runtime: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"proxy runtime: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*proxyRuntimeMetrics{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("proxy runtime: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > proxyRuntimeFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("proxy runtime: invalid %s value %v", metricName, value)
		}
		host := series.Metric["host"]
		if host == "" {
			continue
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		key := host + "\x00" + block + "\x00" + instance
		process := processes[key]
		if process == nil {
			process = &proxyRuntimeMetrics{host: host, block: block, instance: instance}
			processes[key] = process
		}
		switch metricName {
		case "process_resident_memory_bytes":
			process.rss = value
			process.availableMask |= proxyRuntimeMetricRSS
		case "process_start_time_seconds":
			process.start = value
			process.availableMask |= proxyRuntimeMetricStart
		case "urnetwork_build_info":
			if value > 0 && series.Metric["version"] != "" {
				process.version = series.Metric["version"]
				process.availableMask |= proxyRuntimeMetricBuild
			}
		case "go_memstats_heap_alloc_bytes":
			process.heap = value
			process.availableMask |= proxyRuntimeMetricHeap
		case "go_memstats_heap_objects":
			process.objects = value
			process.availableMask |= proxyRuntimeMetricObjects
		case "go_goroutines":
			process.goroutines = value
			process.availableMask |= proxyRuntimeMetricGoroutines
		case "urnetwork_proxy_wg_peers":
			process.peers = value
			process.availableMask |= proxyRuntimeMetricPeers
		case "urnetwork_proxy_devices_live":
			process.devices = value
			process.availableMask |= proxyRuntimeMetricDevices
		case "urnetwork_proxy_device_memory_tracked_used_bytes":
			process.deviceTracked = value
			process.availableMask |= proxyRuntimeMetricDeviceTracked
		case "urnetwork_message_pool_retained_bytes":
			process.poolRetained = value
			process.availableMask |= proxyRuntimeMetricPoolRetained
		case "go_memstats_next_gc_bytes":
			process.nextGC = value
			process.availableMask |= proxyRuntimeMetricNextGC
		case "go_gc_gogc_percent":
			process.gogc = value
			process.availableMask |= proxyRuntimeMetricGOGC
		case "go_memstats_stack_inuse_bytes":
			process.stack = value
			process.availableMask |= proxyRuntimeMetricStack
		case "go_memstats_last_gc_time_seconds":
			process.lastGC = value
			process.availableMask |= proxyRuntimeMetricLastGC
		}
	}

	current, err := newestProxyRuntimeProcesses(processes)
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, fmt.Errorf("proxy runtime: Mimir returned no actual-scrape-fresh proxy process samples")
	}
	sort.Slice(current, func(i, j int) bool {
		return proxyRuntimeLabel(current[i]) < proxyRuntimeLabel(current[j])
	})

	missing := []string{}
	high := []string{}
	for _, process := range current {
		if process.availableMask&proxyRuntimeMetricAll != proxyRuntimeMetricAll {
			missing = append(missing, fmt.Sprintf(
				"%s[%s]",
				proxyRuntimeLabel(process),
				strings.Join(proxyRuntimeMissingMetrics(process.availableMask), ","),
			))
			continue
		}
		peerAllowance := process.peers * proxyRuntimePeerAllowancePerPeer
		knownAllowance := process.poolRetained + process.deviceTracked + peerAllowance
		residual := max(0, process.heap-knownAllowance)
		if process.heap >= proxyRuntimeHeapWarnBytes &&
			process.objects >= proxyRuntimeObjectWarnCount &&
			residual >= proxyRuntimeResidualWarnBytes {
			high = append(high, proxyRuntimeObservation(process, peerAllowance, residual))
		}
	}

	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-live-set", tier: tierWarn,
			class: "proxy-runtime-unobservable", target: "proxy-fleet", frame: metricHost.name, sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities lack the complete runtime memory attribution metric set",
				len(missing), len(current),
			),
			mechanism: "The query proved the selected process identity through fresh RSS and process-start metrics, but one or more Go-runtime or owner gauges are absent on that exact identity. A missing metric is unknown ownership, not a zero-byte pool, zero hosted devices, or an empty WireGuard peer table.",
			baseline:  "Every newest actual-scrape-fresh proxy identity exports RSS, process start, build version, HeapAlloc, heap objects, goroutines, WireGuard peers, hosted devices, tracked DeviceLocal bytes, returned message-pool bytes, next-GC goal, GOGC, stack-in-use bytes, and last-GC time on the same labels.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d complete_runtime_identities=%d missing_identities=%d missing=%s metrics_gateway=%s",
				len(current), len(current)-len(missing), len(missing), strings.Join(missing, ";"), metricHost.name,
			),
			evidence: fmt.Sprintf("Every metric family is filtered by its source timestamp at no more than %.0f seconds old before the exact host/block/instance join; newest process start suppresses a draining generation.", proxyRuntimeFreshness.Seconds()),
			context:  "This is attribution loss, not proof of a memory leak. Host process count, MemAvailable, OOM, and UDP loss remain owned by §14.7; message-pool capacity invariants remain owned by §14.7a.",
			action:   "Provenance-check the selected Proxy image and metrics collector, then deploy the missing identity-free gauges through the ordinary host-serialized rollout. Do not restart solely to erase the high-RSS generation or treat an absent owner gauge as zero.",
			verify:   "Every newest fresh identity exports all fourteen metrics for two consecutive scrapes, after which the live-set discriminator can be evaluated without changing its process generation.",
			playbook: "SIGNALS.md §14.7b",
		})
	}
	if len(high) > 0 {
		findings = append(findings, finding{
			probeId: "runtime/proxy-live-set", tier: tierWarn,
			class: "proxy-runtime-live-set", target: "proxy-fleet", frame: metricHost.name, sustain: 2,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh proxy identities retain a multi-gigabyte Go heap not explained by tracked owners or the production-shaped WireGuard peer allowance",
				len(high), len(current),
			),
			mechanism: "HeapAlloc and heap-object count prove process-owned allocated Go objects rather than filesystem cache or shared image pages. NextGC, GOGC, stack roots, and the last collection time distinguish a reachable post-collection floor from a temporary pre-GC allocation wave. Returned message buffers and tracked DeviceLocal bytes are subtracted directly. The remaining allowance charges 48 KiB per registered WireGuard peer—above the measured 44.6 KiB marginal RSS of endpoint-seeded, server-initiated handshaking peers—so a residual above 2 GiB cannot be attributed to the known peer lifecycle by that control. Legacy Proxy startup also called the process-global server warmup, eagerly constructing API-only SearchLocal indexes and their per-alias rune histograms before the first metrics sample; the selected build version identifies whether that software path can still be present.",
			baseline:  "For each newest proxy identity, HeapAlloc is below 3 GiB or heap objects are below 20 million, or less than 2 GiB remains after returned-pool bytes, tracked DeviceLocal bytes, and a conservative 48 KiB-per-peer allowance.",
			observed: fmt.Sprintf(
				"current_proxy_identities=%d high_live_set_identities=%d heap_limit_bytes=%.0f object_limit=%.0f residual_limit_bytes=%.0f peer_allowance_bytes_per_peer=%.0f high=%s metrics_gateway=%s",
				len(current), len(high), proxyRuntimeHeapWarnBytes, proxyRuntimeObjectWarnCount,
				proxyRuntimeResidualWarnBytes, proxyRuntimePeerAllowancePerPeer,
				strings.Join(high, ";"), metricHost.name,
			),
			evidence: "The newest process generation and its build version are selected independently for every host/block from actual-scrape-fresh Mimir series. NextGC and GOGC expose the collector's heap goal after its prior live-heap mark instead of inferring reachability from one HeapAlloc point. Corrected isolated controls measured exactly two durable goroutines per up peer, 44.6 KiB marginal RSS for endpoint-seeded server-initiated handshaking peers, and 904.2 MiB RSS/557.5 MiB heap at 20,000 such peers. Applying the server's 8 GiB logical message-pool ceiling did not materially change the up-peer result, so neither peer lifecycle nor empty pool capacity reproduces this multi-gigabyte heap floor. A live legacy process already held 3.89 GiB heap and 29.7 million objects 22 seconds after start with zero hosted devices, matching eager startup retention rather than caller-lock growth.",
			context:  "This is a software attribution and efficiency alert, not yet a claim that one cache leaks monotonically. The 2026-09-01 fleet showed both positive and negative thirty-minute heap/object deltas around the large floor. Software can lower the floor, but it cannot create RAM or additional hard client slots: §14.7 still requires capable hardware when an optimized fleet or serialized old/candidate pair cannot fit with reserve.",
			action:   "Provenance-check the selected build. If it predates the Proxy startup fix, deploy the Proxy artifact that skips the global API/model warmup, retains only ProxyId in durable WireGuard TUN factories, and bounds the caller-lock cache. If a current artifact still crosses the band, capture an aggregate heap allocation profile or add identity-free owner gauges on that exact generation and separate full-sync payloads, shared NetworkSpace, hosted DeviceLocal structure, and active HTTP/SOCKS flows. Preserve pool/device metrics as negative controls. Do not force a production GC, restart away the evidence, raise GOGC, lower a cgroup below the measured live set, or shrink WireGuard queues without packet-ordering and slow-peer isolation tests.",
			verify:   "Deploy the Proxy root-cause artifact through the host-serialized rollout. For 15 minutes across multiple GC cycles, every newest identity reports that exact build, preserves peer/device counts and simultaneous WireGuard plus HTTP/SOCKS acceptance, and has a materially lower startup heap/object floor with conservative residual below 2 GiB; RSS and host reserve improve with no new OOM or adjacent UDP receive drops.",
			playbook: "SIGNALS.md §14.7b",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("runtime/proxy-live-set", tierWarn, "proxy-runtime-live-set", "proxy-fleet")}, nil
	}
	return findings, nil
}

func newestProxyRuntimeProcesses(processes map[string]*proxyRuntimeMetrics) ([]*proxyRuntimeMetrics, error) {
	newest := map[string]*proxyRuntimeMetrics{}
	for _, process := range processes {
		if process.availableMask&proxyRuntimeMetricRSS == 0 || process.rss <= 0 {
			continue
		}
		if process.availableMask&proxyRuntimeMetricStart == 0 {
			return nil, fmt.Errorf(
				"proxy runtime: fresh RSS identity %s omitted process_start_time_seconds",
				proxyRuntimeLabel(process),
			)
		}
		slot := process.host + "\x00" + process.block
		if process.block == "" {
			slot += "\x00" + process.instance
		}
		previous := newest[slot]
		if previous == nil || process.start > previous.start ||
			(process.start == previous.start && process.instance > previous.instance) {
			newest[slot] = process
		}
	}
	current := make([]*proxyRuntimeMetrics, 0, len(newest))
	for _, process := range newest {
		current = append(current, process)
	}
	return current, nil
}

func proxyRuntimeLabel(process *proxyRuntimeMetrics) string {
	label := process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

func proxyRuntimeMissingMetrics(mask uint16) []string {
	metrics := []struct {
		bit  uint16
		name string
	}{
		{proxyRuntimeMetricHeap, "heap-alloc"},
		{proxyRuntimeMetricBuild, "build-info"},
		{proxyRuntimeMetricObjects, "heap-objects"},
		{proxyRuntimeMetricGoroutines, "goroutines"},
		{proxyRuntimeMetricPeers, "wg-peers"},
		{proxyRuntimeMetricDevices, "devices-live"},
		{proxyRuntimeMetricDeviceTracked, "device-tracked"},
		{proxyRuntimeMetricPoolRetained, "pool-retained"},
		{proxyRuntimeMetricNextGC, "next-gc"},
		{proxyRuntimeMetricGOGC, "gogc"},
		{proxyRuntimeMetricStack, "stack-inuse"},
		{proxyRuntimeMetricLastGC, "last-gc"},
	}
	missing := []string{}
	for _, metric := range metrics {
		if mask&metric.bit == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func proxyRuntimeObservation(process *proxyRuntimeMetrics, peerAllowance, residual float64) string {
	return fmt.Sprintf(
		"%s(build_version=%q heap_bytes=%.0f heap_objects=%.0f rss_bytes=%.0f goroutines=%.0f wg_peers=%.0f devices_live=%.0f pool_retained_bytes=%.0f device_tracked_bytes=%.0f peer_allowance_bytes=%.0f residual_bytes=%.0f next_gc_bytes=%.0f gogc_percent=%.0f stack_inuse_bytes=%.0f last_gc_time=%.0f start_time=%.0f)",
		proxyRuntimeLabel(process),
		process.version,
		process.heap,
		process.objects,
		process.rss,
		process.goroutines,
		process.peers,
		process.devices,
		process.poolRetained,
		process.deviceTracked,
		peerAllowance,
		residual,
		process.nextGC,
		process.gogc,
		process.stack,
		process.lastGC,
		process.start,
	)
}
