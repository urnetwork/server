package monitor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	mimirIndexMarker          = "monitor-signal-11.18-mimir-index"
	mimirGatewaySyncMaxAge    = 30 * time.Minute
	mimirBucketIndexMaxAge    = 35 * time.Minute
	mimirIndexFutureTolerance = 30 * time.Second
)

// Signal mimir-index implements SIGNALS.md §11.18. It reads each bundled
// Mimir child's own metrics so a healthy Grafana front or a bounded
// single-generation bucket-index warning cannot hide a missed store-gateway
// sync or a compactor index that has stopped advancing.
func NewMimirIndexSignal() Signal {
	return &signalAdapter{
		number: "11.18", key: "mimir-index", name: "Mimir bucket-index and store-gateway freshness",
		probe: mimirIndexProbe{},
	}
}

type mimirIndexProbe struct{}

func (mimirIndexProbe) id() string             { return "observability/mimir-index" }
func (mimirIndexProbe) tier() string           { return tierWarn }
func (mimirIndexProbe) cadence() time.Duration { return time.Minute }

type mimirIndexInstance struct {
	version           string
	processStart      float64
	gatewayLastSync   float64
	gatewaySyncCount  int64
	tenantsDiscovered int64
	tenantsSynced     int64
	metricsReady      bool
	indexUpdates      map[string]float64
}

type mimirIndexHostSample struct {
	instances []mimirIndexInstance
	count     int
	countSeen bool
}

type mimirIndexHostResult struct {
	host   *host
	sample mimirIndexHostSample
	err    error
}

func (mimirIndexProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("mimir index: no services hosts in inventory")
	}

	command := "# " + mimirIndexMarker + "\n" + mimirIndexScript
	results := make(chan mimirIndexHostResult, len(hosts))
	semaphore := make(chan struct{}, 4)
	var wait sync.WaitGroup
	for _, configuredHost := range hosts {
		target := configuredHost
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- mimirIndexHostResult{host: target, err: ctx.Err()}
				return
			}

			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- mimirIndexHostResult{host: target, err: err}
				return
			}
			sample, err := parseMimirIndexHostSample(output)
			results <- mimirIndexHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]mimirIndexHostResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	now := env.now().UTC()
	findings := []finding{}
	newestIndex := map[string]float64{}
	oldestReadyStart := float64(0)
	discoveredTenants := int64(0)
	aggregateObservable := true
	for _, result := range ordered {
		if result.err != nil {
			aggregateObservable = false
			findings = append(findings, cannotObserveFinding(result.host.name+"/mimir-index", result.err))
			continue
		}
		if result.sample.count == 0 {
			aggregateObservable = false
			findings = append(findings, mimirChildMissingFinding(result.host.name))
			continue
		}

		hostReady := false
		for instanceIndex, instance := range result.sample.instances {
			target := result.host.name
			frame := mimirInstanceFrame(instanceIndex, instance)
			if !instance.metricsReady {
				findings = append(findings, mimirMetricsUnavailableFinding(target, frame, instance.version))
				continue
			}
			hostReady = true
			if instance.processStart > 0 && (oldestReadyStart == 0 || instance.processStart < oldestReadyStart) {
				oldestReadyStart = instance.processStart
			}
			if instance.tenantsDiscovered > discoveredTenants {
				discoveredTenants = instance.tenantsDiscovered
			}
			findings = append(findings, evaluateMimirGatewaySync(now, target, frame, instance))
			findings = append(findings, evaluateMimirGatewayTenants(target, frame, instance))
			for tenant, updatedAt := range instance.indexUpdates {
				if newestIndex[tenant] < updatedAt {
					newestIndex[tenant] = updatedAt
				}
			}
		}
		if !hostReady {
			aggregateObservable = false
		}
	}

	// A shared writer timestamp may exist only on the current compactor owner.
	// Never emit either a broken or healthy fleet aggregate while an entire
	// host is unobservable: doing so could invent a missing writer or resolve a
	// real stale-writer ticket merely because its owner was the unreadable host.
	if aggregateObservable {
		findings = append(findings, evaluateMimirBucketIndexes(now, newestIndex, oldestReadyStart, discoveredTenants)...)
	}
	return findings, nil
}

func mimirInstanceFrame(index int, instance mimirIndexInstance) string {
	if instance.processStart > 0 {
		return fmt.Sprintf("process_start=%.0f", instance.processStart)
	}
	return fmt.Sprintf("instance=%d", index+1)
}

func mimirChildMissingFinding(host string) finding {
	return finding{
		probeId: "observability/mimir-index", tier: tierWarn,
		class: "mimir-child-missing", target: host, sustain: 2,
		symptom:   fmt.Sprintf("%s has no locally reachable Grafana Mimir child", host),
		mechanism: "The Grafana bundle front is a supervisor and reverse proxy; its process or systemd unit can exist while the Mimir child is absent or crash-looping. The probe enumerated loopback listeners and found no endpoint whose build-info application is Grafana Mimir.",
		baseline:  "Every active services host has at least one locally reachable Mimir child throughout steady state; an overlap may temporarily have two.",
		observed:  "mimir_instances=0",
		context:   "This is a per-host child failure, not evidence that the shared object store or every Mimir replica is down. During a valid rollout, the old ready generation remains until the replacement child passes readiness.",
		action:    "Inspect the host's warp Grafana unit, parent /status response, and bounded child journal. Fix the first Mimir startup or supervision error in the image/config; do not add a load-balancer target for an unready generation or drain the healthy predecessor.",
		verify:    "The host exposes a Mimir build-info endpoint and fresh child metrics, the Grafana parent /status is ready, and the finding remains clear for two consecutive probes.",
		playbook:  "SIGNALS.md §11.18 and §11.9",
	}
}

func mimirMetricsUnavailableFinding(host, frame, version string) finding {
	return finding{
		probeId: "observability/mimir-index", tier: tierWarn,
		class: "mimir-index-unobservable", target: host, frame: frame, sustain: 2,
		symptom:   fmt.Sprintf("%s has a Mimir child whose internal metrics endpoint is unreadable", host),
		mechanism: "Build-info identified a live Mimir HTTP endpoint, but its local /metrics request did not complete. Readiness alone cannot prove bucket-index or store-gateway freshness without those counters.",
		baseline:  "Every identified Mimir child returns bounded, parseable internal metrics over loopback.",
		observed:  fmt.Sprintf("metrics_ready=false version=%s frame=%s", firstNonempty(version, "unknown"), frame),
		context:   "This is observation loss for the exact Mimir process. Sibling replicas do not make the unreadable process healthy, and a public Grafana query exercises a different route.",
		action:    "Check the child readiness and local HTTP listener, then inspect the exact process journal for stalls or restarts. Restore metrics visibility before interpreting missing freshness counters as zero.",
		verify:    "The same process or its ready replacement returns /metrics and supplies process start, gateway sync, tenant, and bucket-index observations on two consecutive probes.",
		playbook:  "SIGNALS.md §11.18",
	}
}

func evaluateMimirGatewaySync(now time.Time, host, frame string, instance mimirIndexInstance) finding {
	target := host
	base := finding{probeId: "observability/mimir-index", tier: tierWarn, class: "mimir-store-gateway-stale", target: target, frame: frame}
	uptime := time.Duration(0)
	if instance.processStart > 0 {
		uptime = now.Sub(time.Unix(int64(instance.processStart), 0))
	}
	if instance.gatewayLastSync <= 0 {
		if uptime > 0 && uptime <= mimirGatewaySyncMaxAge {
			base.healthy = true
			return base
		}
		base.sustain = 2
		base.symptom = fmt.Sprintf("%s Mimir store-gateway has no successful bucket sync timestamp", host)
		base.mechanism = "The store-gateway has been alive beyond its normal 15-minute sync interval but has not recorded one successful blocks sync, so its loaded block view cannot be trusted."
		base.baseline = "Every established Mimir process records a successful store-gateway sync at least every 30 minutes."
		base.observed = fmt.Sprintf("last_successful_sync=missing process_uptime=%s sync_count=%d version=%s", uptime.Round(time.Second), instance.gatewaySyncCount, firstNonempty(instance.version, "unknown"))
		base.action = "Inspect this process's store-gateway sync errors, object-store reachability, and ring ownership. Repair the failed dependency or configuration; do not hide it by increasing Mimir's staleness tolerance."
		base.verify = "The exact host records two successful periodic syncs, tenants_discovered equals tenants_synced, and no query returns err-mimir-store-consistency-check-failed."
		base.playbook = "SIGNALS.md §11.18"
		return base
	}

	syncedAt := time.Unix(int64(instance.gatewayLastSync), 0).UTC()
	age := now.Sub(syncedAt)
	if age <= mimirGatewaySyncMaxAge && age >= -mimirIndexFutureTolerance {
		base.healthy = true
		return base
	}
	base.sustain = 2
	base.symptom = fmt.Sprintf("%s Mimir store-gateway last synced %s ago", host, age.Round(time.Second))
	base.mechanism = "Store-gateways independently refresh their bucket view on a jittered 15-minute cadence. More than 30 minutes without a successful sync means at least one complete cadence was missed and can leave this gateway multiple bucket-index generations behind queriers."
	base.baseline = "last successful store-gateway sync age <= 30m and not more than 30s in the future"
	base.observed = fmt.Sprintf("last_successful_sync=%s age=%s sync_count=%d version=%s", syncedAt.Format(time.RFC3339), age.Round(time.Second), instance.gatewaySyncCount, firstNonempty(instance.version, "unknown"))
	base.evidence = "Mimir's own cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds metric from the exact host/process."
	base.context = "A single-generation query/gateway version difference is expected phase skew and remains below this threshold. A future timestamp instead indicates host clock skew and must be corrected before freshness can be evaluated."
	base.action = "Inspect the exact process's store-gateway sync errors, object-store operations, and ring state. Restore successful syncs; do not merely suppress the warning, extend max_stale_period, or restart every replica together."
	base.verify = "The exact process records two successful periodic syncs less than 30 minutes apart, tenant discovery is fully synced, and no Mimir store-consistency error occurs."
	base.playbook = "SIGNALS.md §11.18"
	return base
}

func evaluateMimirGatewayTenants(host, frame string, instance mimirIndexInstance) finding {
	base := finding{probeId: "observability/mimir-index", tier: tierWarn, class: "mimir-store-gateway-tenants", target: host, frame: frame}
	if instance.tenantsDiscovered == instance.tenantsSynced {
		base.healthy = true
		return base
	}
	base.sustain = 2
	base.symptom = fmt.Sprintf("%s Mimir store-gateway synced %d of %d discovered tenants", host, instance.tenantsSynced, instance.tenantsDiscovered)
	base.mechanism = "The gateway discovered tenant bucket indexes but did not complete loading all of them. Queries routed to this instance can require consistency retries and eventually fail if no replica owns the omitted blocks."
	base.baseline = "cortex_bucket_stores_tenants_synced equals cortex_bucket_stores_tenants_discovered on every ready Mimir process"
	base.observed = fmt.Sprintf("tenants_discovered=%d tenants_synced=%d version=%s", instance.tenantsDiscovered, instance.tenantsSynced, firstNonempty(instance.version, "unknown"))
	base.evidence = "Paired tenant gauges from one exact Mimir child; zero discovered and zero synced is valid for an empty environment."
	base.context = "Do not infer a compactor failure solely from this gateway-local mismatch. The separate shared writer signal evaluates bucket-index updates across the fleet."
	base.action = "Inspect store-gateway metadata fetch, index-header, object-store, and ring errors for this process, then repair the first failed load. Do not drain healthy gateway replicas until coverage is restored."
	base.verify = "Discovered and synced tenant counts match on two consecutive probes and representative historical queries complete without consistency retries or errors."
	base.playbook = "SIGNALS.md §11.18"
	return base
}

func evaluateMimirBucketIndexes(now time.Time, newest map[string]float64, oldestReadyStart float64, discovered int64) []finding {
	if len(newest) == 0 {
		uptime := time.Duration(0)
		if oldestReadyStart > 0 {
			uptime = now.Sub(time.Unix(int64(oldestReadyStart), 0))
		}
		if discovered == 0 || (uptime > 0 && uptime <= mimirBucketIndexMaxAge) {
			return []finding{healthyFinding("observability/mimir-index", tierWarn, "mimir-bucket-index-stale", "mimir-bucket-index")}
		}
		return []finding{{
			probeId: "observability/mimir-index", tier: tierWarn,
			class: "mimir-bucket-index-stale", target: "mimir-bucket-index", sustain: 2,
			symptom:   "No established Mimir compactor exports a successful bucket-index update",
			mechanism: "At least one tenant is discovered and the fleet has been running longer than the 35-minute writer threshold, but no compactor owns a successful bucket-index update metric. Queriers cannot prove their shared long-term-storage view is advancing.",
			baseline:  "At least one compactor reports a successful per-tenant bucket-index update within 35 minutes.",
			observed:  fmt.Sprintf("index_updates=0 discovered_tenants=%d oldest_ready_process_uptime=%s", discovered, uptime.Round(time.Second)),
			action:    "Inspect compactor ring ownership, cleanup errors, and object-store writes on every Mimir replica. Restore one healthy owner; do not raise the querier max-stale period as a substitute for index production.",
			verify:    "A compactor emits a fresh cortex_bucket_index_last_successful_update_timestamp_seconds value and it advances again on the next cleanup cadence.",
			playbook:  "SIGNALS.md §11.18",
		}}
	}

	findings := make([]finding, 0, len(newest))
	tenants := make([]string, 0, len(newest))
	for tenant := range newest {
		tenants = append(tenants, tenant)
	}
	sort.Strings(tenants)
	for _, tenant := range tenants {
		target := "mimir-bucket-index/" + safeMimirTenant(tenant)
		updatedAt := time.Unix(int64(newest[tenant]), 0).UTC()
		age := now.Sub(updatedAt)
		if age <= mimirBucketIndexMaxAge && age >= -mimirIndexFutureTolerance {
			findings = append(findings, healthyFinding("observability/mimir-index", tierWarn, "mimir-bucket-index-stale", target))
			continue
		}
		findings = append(findings, finding{
			probeId: "observability/mimir-index", tier: tierWarn,
			class: "mimir-bucket-index-stale", target: target, sustain: 2,
			symptom:   fmt.Sprintf("Mimir bucket index %s was last updated %s ago", safeMimirTenant(tenant), age.Round(time.Second)),
			mechanism: "The compactor is responsible for writing the shared per-tenant bucket index on a jittered 15-minute cleanup cadence. An age over 35 minutes means two expected updates plus buffer were missed; at the default one-hour max-stale period, queriers will next fail rather than return partial long-term results.",
			baseline:  "newest successful bucket-index update age <= 35m and not more than 30s in the future",
			observed:  fmt.Sprintf("last_successful_update=%s age=%s tenant=%s", updatedAt.Format(time.RFC3339), age.Round(time.Second), safeMimirTenant(tenant)),
			evidence:  "Fleet maximum of cortex_bucket_index_last_successful_update_timestamp_seconds for this privacy-safe tenant identity.",
			context:   "This is the shared writer signal, distinct from one store-gateway's local sync lag. A future value indicates clock skew and is also outside the valid freshness band.",
			action:    "Inspect the compactor owner, cleanup failure counters, ring state, and object-store writes. Restore index updates before changing querier tolerances; do not restart every compactor simultaneously.",
			verify:    "The bucket-index timestamp advances on two cleanup cadences, every gateway sync remains under 30 minutes old, and no err-mimir-bucket-index-too-old or store-consistency error occurs.",
			playbook:  "SIGNALS.md §11.18",
		})
	}
	return findings
}

func safeMimirTenant(tenant string) string {
	if tenant == "" || tenant == "anonymous" {
		return "anonymous"
	}
	digest := sha256.Sum256([]byte(tenant))
	return fmt.Sprintf("tenant-%x", digest[:4])
}

func parseMimirIndexHostSample(output string) (mimirIndexHostSample, error) {
	sample := mimirIndexHostSample{}
	current := -1
	for lineNumber, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "instance_begin":
			if len(fields) != 2 || current >= 0 {
				return sample, fmt.Errorf("mimir index line %d: invalid instance_begin", lineNumber+1)
			}
			sample.instances = append(sample.instances, mimirIndexInstance{indexUpdates: map[string]float64{}})
			current = len(sample.instances) - 1
		case "instance_end":
			if len(fields) != 1 || current < 0 {
				return sample, fmt.Errorf("mimir index line %d: unexpected instance_end", lineNumber+1)
			}
			current = -1
		case "mimir_count":
			if len(fields) != 2 || sample.countSeen {
				return sample, fmt.Errorf("mimir index line %d: invalid mimir_count", lineNumber+1)
			}
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return sample, fmt.Errorf("mimir index line %d: invalid mimir_count %q", lineNumber+1, fields[1])
			}
			sample.count = count
			sample.countSeen = true
		default:
			if current < 0 {
				return sample, fmt.Errorf("mimir index line %d: field outside instance: %q", lineNumber+1, fields[0])
			}
			instance := &sample.instances[current]
			switch fields[0] {
			case "metrics_ready":
				if len(fields) != 2 || (fields[1] != "0" && fields[1] != "1") {
					return sample, fmt.Errorf("mimir index line %d: invalid metrics_ready", lineNumber+1)
				}
				instance.metricsReady = fields[1] == "1"
			case "version":
				if len(fields) != 2 {
					return sample, fmt.Errorf("mimir index line %d: invalid version", lineNumber+1)
				}
				instance.version = fields[1]
			case "process_start", "gateway_last_sync":
				if len(fields) != 2 {
					return sample, fmt.Errorf("mimir index line %d: invalid %s", lineNumber+1, fields[0])
				}
				value, err := strconv.ParseFloat(fields[1], 64)
				if err != nil || value < 0 {
					return sample, fmt.Errorf("mimir index line %d: invalid %s %q", lineNumber+1, fields[0], fields[1])
				}
				if fields[0] == "process_start" {
					instance.processStart = value
				} else {
					instance.gatewayLastSync = value
				}
			case "gateway_sync_count", "tenants_discovered", "tenants_synced":
				if len(fields) != 2 {
					return sample, fmt.Errorf("mimir index line %d: invalid %s", lineNumber+1, fields[0])
				}
				value, err := strconv.ParseInt(fields[1], 10, 64)
				if err != nil || value < 0 {
					return sample, fmt.Errorf("mimir index line %d: invalid %s %q", lineNumber+1, fields[0], fields[1])
				}
				switch fields[0] {
				case "gateway_sync_count":
					instance.gatewaySyncCount = value
				case "tenants_discovered":
					instance.tenantsDiscovered = value
				case "tenants_synced":
					instance.tenantsSynced = value
				}
			case "index_update":
				if len(fields) != 3 || fields[1] == "" {
					return sample, fmt.Errorf("mimir index line %d: invalid index_update", lineNumber+1)
				}
				value, err := strconv.ParseFloat(fields[2], 64)
				if err != nil || value < 0 {
					return sample, fmt.Errorf("mimir index line %d: invalid index_update %q", lineNumber+1, fields[2])
				}
				instance.indexUpdates[fields[1]] = value
			default:
				return sample, fmt.Errorf("mimir index line %d: unknown field %q", lineNumber+1, fields[0])
			}
		}
	}
	if current >= 0 {
		return sample, fmt.Errorf("mimir index: unterminated instance")
	}
	if !sample.countSeen {
		return sample, fmt.Errorf("mimir index: missing mimir_count")
	}
	if sample.count != len(sample.instances) {
		return sample, fmt.Errorf("mimir index: count=%d instances=%d", sample.count, len(sample.instances))
	}
	return sample, nil
}

const mimirIndexScript = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'mimir probe prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
mimir_count=0
ports=$(ss -ltnH 2>/dev/null | awk '$4 ~ /^127[.]0[.]0[.]1:[0-9]+$/ {sub(/.*:/, "", $4); print $4}' | sort -n -u)
for port in $ports; do
  build_info=$(curl -fsS --max-time 2 "http://127.0.0.1:${port}/api/v1/status/buildinfo" 2>/dev/null || true)
  case "$build_info" in
    *'"application":"Grafana Mimir"'*) ;;
    *) continue ;;
  esac

  mimir_count=$((mimir_count+1))
  printf 'instance_begin %s\n' "$port"
  metrics=$(curl -fsS --max-time 10 "http://127.0.0.1:${port}/metrics" 2>/dev/null)
  metrics_status=$?
  if [ "$metrics_status" -ne 0 ]; then
    printf 'metrics_ready 0\ninstance_end\n'
    continue
  fi
  printf 'metrics_ready 1\n'
  printf '%s\n' "$metrics" | awk '
    /^cortex_build_info[{]/ {
      if (match($1, /version="[^"]+"/)) {
        value=substr($1, RSTART+9, RLENGTH-10)
        print "version", value
      }
    }
    /^process_start_time_seconds / {print "process_start", $2}
    /^cortex_bucket_stores_blocks_last_successful_sync_timestamp_seconds[ {]/ {print "gateway_last_sync", $2}
    /^cortex_bucket_stores_blocks_sync_seconds_count[ {]/ {print "gateway_sync_count", $2}
    /^cortex_bucket_stores_tenants_discovered[ {]/ {print "tenants_discovered", $2}
    /^cortex_bucket_stores_tenants_synced[ {]/ {print "tenants_synced", $2}
    /^cortex_bucket_index_last_successful_update_timestamp_seconds[{]/ {
      if (match($1, /user="[^"]+"/)) {
        user=substr($1, RSTART+6, RLENGTH-7)
        print "index_update", user, $2
      }
    }
  '
  printf 'instance_end\n'
done
printf 'mimir_count %s\n' "$mimir_count"
`
