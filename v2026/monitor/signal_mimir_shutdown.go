package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const mimirShutdownMarker = "monitor-signal-11.21-mimir-shutdown"

// Signal mimir-shutdown implements SIGNALS.md §11.21. It reads only the exact
// shutdown and recent-store settings needed to evaluate replacement
// continuity from each bundled Mimir child. The rest of the rendered
// configuration, which can contain credentials, never leaves the host.
func NewMimirShutdownSignal() Signal {
	return &signalAdapter{
		number: "11.21", key: "mimir-shutdown", name: "Mimir shutdown durability configuration",
		probe: mimirShutdownProbe{},
	}
}

type mimirShutdownProbe struct{}

func (mimirShutdownProbe) id() string             { return "observability/mimir-shutdown" }
func (mimirShutdownProbe) tier() string           { return tierWarn }
func (mimirShutdownProbe) cadence() time.Duration { return 5 * time.Minute }

type mimirShutdownInstance struct {
	port                     int
	flush                    bool
	flushSeen                bool
	queryStoreAfter          time.Duration
	queryStoreAfterSeen      bool
	queryIngestersWithin     time.Duration
	queryIngestersWithinSeen bool
	ignoreBlocksWithin       time.Duration
	ignoreBlocksWithinSeen   bool
	bucketSyncInterval       time.Duration
	bucketSyncIntervalSeen   bool
	compactorCleanupInterval time.Duration
	compactorCleanupSeen     bool
}

type mimirShutdownHostSample struct {
	instances []mimirShutdownInstance
	count     int
	countSeen bool
}

type mimirShutdownHostResult struct {
	host   *host
	sample mimirShutdownHostSample
	err    error
}

func (mimirShutdownProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("mimir shutdown: no services hosts in inventory")
	}

	command := "# " + mimirShutdownMarker + "\n" + mimirShutdownScript
	results := make(chan mimirShutdownHostResult, len(hosts))
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
				results <- mimirShutdownHostResult{host: target, err: ctx.Err()}
				return
			}

			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- mimirShutdownHostResult{host: target, err: err}
				return
			}
			sample, err := parseMimirShutdownHostSample(output)
			results <- mimirShutdownHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]mimirShutdownHostResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered)+1)
	complete := true
	observableHosts := 0
	instanceCount := 0
	disabled := []string{}
	replacementUnverified := []string{}
	noncompactedRisk := []string{}
	for _, result := range ordered {
		target := result.host.name
		if result.err != nil {
			complete = false
			findings = append(findings, cannotObserveFinding(target+"/mimir-shutdown", result.err))
			continue
		}
		if result.sample.count == 0 {
			complete = false
			findings = append(findings, mimirShutdownChildMissingFinding(target))
			continue
		}

		observableHosts++
		instanceCount += result.sample.count
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn, "mimir-shutdown-child-missing", target,
		))
		for _, instance := range result.sample.instances {
			identity := fmt.Sprintf("%s:%d", target, instance.port)
			if !instance.flush {
				disabled = append(disabled, identity+"=false")
			}
			if instance.queryStoreAfter <= 0 || instance.ignoreBlocksWithin <= 0 {
				noncompactedRisk = append(noncompactedRisk, mimirShutdownInstanceDetail(identity, instance))
				continue
			}
			replacementUnverified = append(
				replacementUnverified,
				mimirShutdownInstanceDetail(identity, instance),
			)
		}
	}

	if len(disabled) > 0 {
		findings = append(findings, mimirShutdownFlushDisabledFinding(
			len(hosts), observableHosts, instanceCount, disabled,
		))
	} else if complete {
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn,
			"mimir-shutdown-flush-disabled", "mimir-fleet",
		))
	}
	if len(noncompactedRisk) > 0 {
		findings = append(findings, mimirNoncompactedQueryRiskFinding(
			instanceCount, noncompactedRisk,
		))
	} else if complete {
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn,
			"mimir-noncompacted-query-risk", "mimir-fleet",
		))
	}
	if len(replacementUnverified) > 0 {
		findings = append(findings, mimirReplacementContinuityUnverifiedFinding(
			instanceCount, replacementUnverified,
		))
	} else if complete {
		findings = append(findings, healthyFinding(
			"observability/mimir-shutdown", tierWarn,
			"mimir-replacement-continuity-unverified", "mimir-fleet",
		))
	}
	return findings, nil
}

func mimirShutdownInstanceDetail(identity string, instance mimirShutdownInstance) string {
	return fmt.Sprintf(
		"%s{flush=%t,query_store_after=%s,query_ingesters_within=%s,ignore_blocks_within=%s,bucket_sync_interval=%s,compactor_cleanup_interval=%s}",
		identity, instance.flush, instance.queryStoreAfter, instance.queryIngestersWithin,
		instance.ignoreBlocksWithin, instance.bucketSyncInterval,
		instance.compactorCleanupInterval,
	)
}

func mimirShutdownChildMissingFinding(host string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-shutdown-child-missing", target: host, sustain: 2,
		symptom:   fmt.Sprintf("%s has no locally observable Mimir shutdown configuration", host),
		mechanism: "The Grafana bundle parent can remain alive while its Mimir child is absent, starting, or no longer exposes a loopback configuration endpoint. Without the exact child setting, this host cannot prove that its unshipped TSDB head survives a clean rollout.",
		baseline:  "Every active services host has at least one locally reachable Mimir child whose rendered configuration contains exactly one value for each of the six allowlisted shutdown/recent-store fields; a rollout may temporarily expose two generations.",
		observed:  "mimir_instances=0",
		context:   "This is host-local observation loss, not proof that every replicated Mimir process is down. The probe emits only the six allowlisted non-secret Boolean/duration fields and never returns the full rendered configuration because it can contain credentials.",
		action:    "Inspect the host's Grafana unit, parent status, and bounded child journal. Restore the Mimir child or its loopback configuration endpoint before claiming shutdown durability from a sibling replica.",
		verify:    "The host exposes at least one Mimir configuration with flush_blocks_on_shutdown=true for two consecutive probes.",
		playbook:  "SIGNALS.md §11.21 and §11.2",
	}
}

func mimirShutdownFlushDisabledFinding(hostCount, observableHosts, instanceCount int, disabled []string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-shutdown-flush-disabled", target: "mimir-fleet", sustain: 1,
		symptom: fmt.Sprintf(
			"%d of %d directly observed Mimir process(es) will not flush their partial TSDB head on clean shutdown",
			len(disabled), instanceCount,
		),
		mechanism: "Mimir has not yet uploaded its current incomplete TSDB head to object storage. With flush_blocks_on_shutdown=false and the Grafana data directory intentionally ephemeral, removing a cleanly stopped container discards that unshipped head instead of reusing it, producing bounded holes in otherwise independent metric series.",
		baseline:  "Every active Mimir process on every enabled services host renders blocks_storage.tsdb.flush_blocks_on_shutdown: true and retains the Grafana parent's 120-second Mimir child stop allowance inside Warpctl's 3,600-second container drain.",
		observed: fmt.Sprintf(
			"configured_hosts=%d observable_hosts=%d mimir_instances=%d disabled_instances=%d details=%s",
			hostCount, observableHosts, instanceCount, len(disabled), strings.Join(disabled, ";"),
		),
		evidence: "Each value comes from the exact process's loopback /config endpoint. The remote filter emits only the six allowlisted non-secret Boolean/duration fields; rendered credentials and unrelated configuration never leave the host.",
		context:  "This is a software deployment gap, not a Grafana panel-query defect and not a hardware-capacity alert. Historical fixed Mimir gaps are unrecoverable and clear only when they age out of the dashboard window. Enabling this setting protects shutdown durability, but does not by itself remove the positive recent-store query blind zone classified by §11.20.",
		action:   "Build and deploy Grafana from an intentional local Warp checkout containing commit 7176ccd, after §8.13 can read the exact Warpctl identity. Keep each generation's TSDB directory private and ephemeral, retain the 120-second Mimir child stop allowance inside Warpctl's 3,600-second container drain, and do not zero-fill the dashboard or shared-mount one TSDB directory into overlapping containers. The generated unit's separate 60-second timeout stops only the Warpctl controller and does not truncate a normal container drain. The first rollout still begins with old children configured false; explicitly flushing those old ingesters is an operator-controlled production mutation if preserving their current partial heads is required. Treat replacement-read continuity as the separate operator decision in this signal.",
		verify:   "Require every exact loopback Mimir config to report flush_blocks_on_shutdown=true on consecutive probes. Separately resolve mimir-replacement-continuity-unverified through an approved lifecycle design; do not use a clean flush alone as proof that a replacement has no temporary query gap.",
		playbook: "SIGNALS.md §11.21, §11.20, and §8.13",
	}
}

func mimirReplacementContinuityUnverifiedFinding(instanceCount int, details []string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-replacement-continuity-unverified", target: "mimir-fleet", sustain: 1,
		symptom: fmt.Sprintf(
			"%d of %d observed Mimir process(es) do not prove a complete replacement-read handoff",
			len(details), instanceCount,
		),
		mechanism: "Positive recent-store horizons deliberately keep replicated non-compacted blocks out of ordinary store queries. A clean flush protects object-storage durability, but replacing every ephemeral ingester at once still removes the only query path to its recent head until the compacted store boundary advances. The exact current config has no verified parent lifecycle marker proving that an old ingester remains read-only and queryable for the full store/discovery horizon.",
		baseline:  "Every exact Mimir child keeps positive ordered compacted-store horizons, and the selected architecture supplies separately observable proof that the old generation stays read-only and queryable for at least max(query_store_after, 2*bucket sync, 2*compactor cleanup). Zero horizons are a separate non-compacted-query risk, not a healthy shortcut.",
		observed:  fmt.Sprintf("mimir_instances=%d unverified_instances=%d details=%s", instanceCount, len(details), strings.Join(details, ";")),
		evidence:  "Each duration and Boolean comes from an exact child loopback /config endpoint. No object-store credential, raw config, metric label, or response body leaves the host. This version deliberately does not infer a parent lifecycle contract from a listening child.",
		context:   "This is a replacement-read continuity decision, not proof of permanent loss in an existing historical gap. Section 11.20 uses repeated absolute-window observations to distinguish a moving recent-store boundary from a fixed loss. Mimir 3.1.1's classic downscale procedure is correctness-preserving but a 12-hour old/new overlap changes capacity, deploy serialization, rollback, and failure handling.",
		action:    "Preserve the exact config and obtain an operator architecture decision among a capacity-gated classic read-only handoff, a dedicated persistent Mimir tier, or another independently proven design. Do not automatically set query_store_after or ignore_blocks_within to zero, shared-mount a TSDB, or deploy an unapproved long overlap merely to clear this alert.",
		verify:    "After the selected architecture and its capacity/rollback gates are approved and deployed, every exact child reports valid ordered horizons, the lifecycle has its own exact-process proof, and a controlled then full replacement creates no new §11.20 gap through the complete store/discovery window.",
		playbook:  "SIGNALS.md §11.21 and §11.20",
	}
}

func mimirNoncompactedQueryRiskFinding(instanceCount int, details []string) finding {
	return finding{
		probeId: "observability/mimir-shutdown", tier: tierWarn,
		class: "mimir-noncompacted-query-risk", target: "mimir-fleet", sustain: 1,
		symptom: fmt.Sprintf(
			"%d of %d observed Mimir process(es) query replicated non-compacted block ages",
			len(details), instanceCount,
		),
		mechanism: "A zero query_store_after or ignore_blocks_within removes Mimir's compacted-store safety horizon. Store-gateway does not deduplicate chunks, and the querier's same-timestamp merge may choose either replica; even when ordinary replication produced identical values, the production guidance warns that these reads are inefficient and may contain duplicated samples.",
		baseline:  "query_store_after and ignore_blocks_within remain positive, ignore_blocks_within does not exceed query_store_after, and replacement continuity is solved by an explicitly approved lifecycle or persistent-tier design rather than raw-block querying.",
		observed:  fmt.Sprintf("mimir_instances=%d noncompacted_risk_instances=%d details=%s", instanceCount, len(details), strings.Join(details, ";")),
		evidence:  "Only exact non-secret duration/Boolean fields leave each host; full rendered config and query data remain local.",
		context:   "This is a query correctness/load risk. It must not be treated as successful remediation of a moving §11.20 visibility gap without an explicit duplicate-equivalence and capacity proof for the deployed topology.",
		action:    "Restore positive ordered compacted-store horizons or stop the rollout and obtain an explicit operator decision backed by duplicate, query-load, and rollback evidence. Do not infer safety from a temporarily filled dashboard gap.",
		verify:    "Every exact child reports positive safe horizons on consecutive probes, store/query load stays within its established band, and the approved replacement design passes a full replacement without a new continuity gap.",
		playbook:  "SIGNALS.md §11.21 and §11.20",
	}
}

func parseMimirShutdownHostSample(output string) (mimirShutdownHostSample, error) {
	sample := mimirShutdownHostSample{}
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
				return sample, fmt.Errorf("mimir shutdown line %d: invalid instance_begin", lineNumber+1)
			}
			port, err := strconv.Atoi(fields[1])
			if err != nil || port < 1 || port > 65535 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid port %q", lineNumber+1, fields[1])
			}
			sample.instances = append(sample.instances, mimirShutdownInstance{port: port})
			current = len(sample.instances) - 1
		case "flush":
			if len(fields) != 2 || current < 0 || sample.instances[current].flushSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid flush", lineNumber+1)
			}
			value, err := strconv.ParseBool(fields[1])
			if err != nil {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid flush %q", lineNumber+1, fields[1])
			}
			sample.instances[current].flush = value
			sample.instances[current].flushSeen = true
		case "query_store_after", "query_ingesters_within", "ignore_blocks_within", "bucket_sync_interval", "compactor_cleanup_interval":
			if len(fields) != 2 || current < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid %s", lineNumber+1, fields[0])
			}
			value, err := time.ParseDuration(fields[1])
			if err != nil || value < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid %s %q", lineNumber+1, fields[0], fields[1])
			}
			instance := &sample.instances[current]
			switch fields[0] {
			case "query_store_after":
				if instance.queryStoreAfterSeen {
					return sample, fmt.Errorf("mimir shutdown line %d: duplicate query_store_after", lineNumber+1)
				}
				instance.queryStoreAfter, instance.queryStoreAfterSeen = value, true
			case "query_ingesters_within":
				if instance.queryIngestersWithinSeen {
					return sample, fmt.Errorf("mimir shutdown line %d: duplicate query_ingesters_within", lineNumber+1)
				}
				instance.queryIngestersWithin, instance.queryIngestersWithinSeen = value, true
			case "ignore_blocks_within":
				if instance.ignoreBlocksWithinSeen {
					return sample, fmt.Errorf("mimir shutdown line %d: duplicate ignore_blocks_within", lineNumber+1)
				}
				instance.ignoreBlocksWithin, instance.ignoreBlocksWithinSeen = value, true
			case "bucket_sync_interval":
				if instance.bucketSyncIntervalSeen {
					return sample, fmt.Errorf("mimir shutdown line %d: duplicate bucket_sync_interval", lineNumber+1)
				}
				instance.bucketSyncInterval, instance.bucketSyncIntervalSeen = value, true
			case "compactor_cleanup_interval":
				if instance.compactorCleanupSeen {
					return sample, fmt.Errorf("mimir shutdown line %d: duplicate compactor_cleanup_interval", lineNumber+1)
				}
				instance.compactorCleanupInterval, instance.compactorCleanupSeen = value, true
			}
		case "instance_end":
			if len(fields) != 1 || current < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: unexpected instance_end", lineNumber+1)
			}
			instance := sample.instances[current]
			if !instance.flushSeen || !instance.queryStoreAfterSeen ||
				!instance.queryIngestersWithinSeen || !instance.ignoreBlocksWithinSeen ||
				!instance.bucketSyncIntervalSeen || !instance.compactorCleanupSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: instance omitted required config field", lineNumber+1)
			}
			current = -1
		case "mimir_count":
			if len(fields) != 2 || current >= 0 || sample.countSeen {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid mimir_count", lineNumber+1)
			}
			count, err := strconv.Atoi(fields[1])
			if err != nil || count < 0 {
				return sample, fmt.Errorf("mimir shutdown line %d: invalid mimir_count %q", lineNumber+1, fields[1])
			}
			sample.count = count
			sample.countSeen = true
		default:
			return sample, fmt.Errorf("mimir shutdown line %d: unknown field %q", lineNumber+1, fields[0])
		}
	}
	if current >= 0 {
		return sample, fmt.Errorf("mimir shutdown: unterminated instance")
	}
	if !sample.countSeen {
		return sample, fmt.Errorf("mimir shutdown: missing mimir_count")
	}
	if sample.count != len(sample.instances) {
		return sample, fmt.Errorf("mimir shutdown: count=%d instances=%d", sample.count, len(sample.instances))
	}
	return sample, nil
}

const mimirShutdownScript = `set -u
for required in ss curl awk sort; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'mimir shutdown probe prerequisite missing: %s\n' "$required" >&2
    exit 1
  fi
done
mimir_count=0
ports=$(ss -ltnH 2>/dev/null | awk '$4 ~ /^127[.]0[.]0[.]1:[0-9]+$/ {sub(/.*:/, "", $4); print $4}' | sort -n -u)
for port in $ports; do
	selected=$(curl -fsS --max-time 10 "http://127.0.0.1:${port}/config" 2>/dev/null | awk '
		function yaml_key(value) {sub(/:$/, "", value); return value}
		function clear_path() {
			for (level in path_key) delete path_key[level]
			for (level in path_indent) delete path_indent[level]
			path_depth = 0
		}
		function leave_to_parent(indent) {
			while (path_depth > 0 && indent <= path_indent[path_depth]) {
				delete path_key[path_depth]
				delete path_indent[path_depth]
				path_depth--
			}
		}
		function enter_key(indent, key) {
			path_depth++
			path_indent[path_depth] = indent
			path_key[path_depth] = key
		}
		function parent_is(first) {
			return path_depth == 1 && path_key[1] == first
		}
		function parents_are(first, second) {
			return path_depth == 2 && path_key[1] == first && path_key[2] == second
		}
		{
			if ($0 ~ /^[ ]*$/ || $0 ~ /^[ ]*#/) next
			if (index($0, "\t")) {
				clear_path()
				next
			}
			match($0, /[^ ]/)
			if (RSTART == 0) {
				clear_path()
				next
			}
			indent = RSTART - 1
			leave_to_parent(indent)
			if ($1 !~ /:$/) next
			key = yaml_key($1)
			if (parents_are("blocks_storage", "tsdb") && key == "flush_blocks_on_shutdown") print "flush " $2
			if ((parent_is("blocks_storage") || parents_are("blocks_storage", "bucket_store")) && key == "ignore_blocks_within") print "ignore_blocks_within " $2
			if (parents_are("blocks_storage", "bucket_store") && key == "sync_interval") print "bucket_sync_interval " $2
			if (parent_is("querier") && key == "query_store_after") print "query_store_after " $2
			if (parent_is("limits") && key == "query_ingesters_within") print "query_ingesters_within " $2
			if (parent_is("compactor") && key == "cleanup_interval") print "compactor_cleanup_interval " $2
			enter_key(indent, key)
		}
	' || true)
	case "$selected" in
		*'flush true'*|*'flush false'*) ;;
		*) continue ;;
	esac

	mimir_count=$((mimir_count+1))
	printf 'instance_begin %s\n' "$port"
	printf '%s\n' "$selected"
	printf 'instance_end\n'
done
printf 'mimir_count %s\n' "$mimir_count"
`
