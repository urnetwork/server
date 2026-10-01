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
	provenanceFreshness = 90 * time.Second
	provenanceJobRegexp = "api|competitionworker|connect|proxy|taskworker"
)

// Signal provenance implements SIGNALS.md §8.12. It joins each newest fresh
// long-running Go service process to its executable's Go VCS metadata and the
// immutable OCI digest that Warp actually executed.
func NewProvenanceSignal() Signal {
	return &signalAdapter{
		number: "8.12", key: "provenance", name: "Fleet service artifact provenance",
		probe: provenanceProbe{},
	}
}

type provenanceProbe struct{}

func (provenanceProbe) id() string             { return "deploy/provenance" }
func (provenanceProbe) tier() string           { return tierWarn }
func (provenanceProbe) cadence() time.Duration { return time.Minute }

const (
	provenanceMetricRSS uint8 = 1 << iota
	provenanceMetricStart
	provenanceMetricBuild
	provenanceMetricSource
)

var provenanceMetricNames = []string{
	"process_resident_memory_bytes",
	"process_start_time_seconds",
	"urnetwork_build_info",
	"urnetwork_source_info",
}

type provenanceProcess struct {
	job            string
	host           string
	block          string
	instance       string
	configVersion  string
	sourceRevision string
	sourceModified string
	imageDigest    string
	rss            float64
	start          float64
	availableMask  uint8
}

func provenanceQuery(environment string) string {
	parts := make([]string, 0, len(provenanceMetricNames))
	for _, metricName := range provenanceMetricNames {
		series := fmt.Sprintf(
			`label_replace(%s{env=%s,job=~%s},"monitor_metric",%s,"job",".*")`,
			metricName,
			strconv.Quote(environment),
			strconv.Quote(provenanceJobRegexp),
			strconv.Quote(metricName),
		)
		parts = append(parts, fmt.Sprintf(
			`(%s and on(monitor_metric,env,job,host,block,instance) (timestamp(%s) >= time() - %d))`,
			series,
			series,
			int64(provenanceFreshness/time.Second),
		))
	}
	return strings.Join(parts, " or ")
}

func (provenanceProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("provenance: no services host in inventory for the loopback Mimir query")
	}

	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" +
		url.QueryEscape(provenanceQuery(env.cfg.env))
	out, metricHost, err := shellFirstServiceGateway(
		ctx,
		env.runner,
		metricHosts,
		nil,
		"curl -fsS --max-time 15 '"+queryURL+"'",
	)
	if err != nil {
		return nil, fmt.Errorf("provenance: query Mimir through service gateways: %w", err)
	}

	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("provenance: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf(
			"provenance: Mimir status=%q result_type=%q error=%q",
			response.Status,
			response.Data.ResultType,
			response.Error,
		)
	}

	now := env.now().UTC()
	processes := map[string]*provenanceProcess{}
	for _, series := range response.Data.Result {
		metricName := series.Metric["monitor_metric"]
		if metricName == "" {
			metricName = series.Metric["__name__"]
		}
		observedAt, value, err := mimirInstantValue(series.Value)
		if err != nil {
			return nil, fmt.Errorf("provenance: parse %s sample: %w", metricName, err)
		}
		age := now.Sub(observedAt)
		if age > provenanceFreshness || age < -30*time.Second {
			continue
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, fmt.Errorf("provenance: invalid %s value %v", metricName, value)
		}

		job := series.Metric["job"]
		host := series.Metric["host"]
		if job == "" || host == "" {
			continue
		}
		block := series.Metric["block"]
		instance := series.Metric["instance"]
		key := strings.Join([]string{job, host, block, instance}, "\x00")
		process := processes[key]
		if process == nil {
			process = &provenanceProcess{
				job: job, host: host, block: block, instance: instance,
			}
			processes[key] = process
		}

		switch metricName {
		case "process_resident_memory_bytes":
			if value > 0 {
				process.rss = value
				process.availableMask |= provenanceMetricRSS
			}
		case "process_start_time_seconds":
			if value > 0 {
				process.start = value
				process.availableMask |= provenanceMetricStart
			}
		case "urnetwork_build_info":
			if value > 0 && series.Metric["version"] != "" {
				process.configVersion = series.Metric["version"]
				process.availableMask |= provenanceMetricBuild
			}
		case "urnetwork_source_info":
			if value > 0 {
				process.sourceRevision = series.Metric["revision"]
				process.sourceModified = series.Metric["modified"]
				process.imageDigest = series.Metric["image_digest"]
				process.availableMask |= provenanceMetricSource
			}
		}
	}

	current := newestProvenanceProcesses(processes)
	if len(current) == 0 {
		return nil, fmt.Errorf("provenance: Mimir returned no actual-scrape-fresh service process samples")
	}
	sort.Slice(current, func(i, j int) bool {
		return provenanceLabel(current[i]) < provenanceLabel(current[j])
	})

	missing := []string{}
	invalid := []string{}
	for _, process := range current {
		missingMetrics := provenanceMissingMetrics(process.availableMask)
		if len(missingMetrics) > 0 {
			missing = append(missing, fmt.Sprintf(
				"%s[%s]",
				provenanceLabel(process),
				strings.Join(missingMetrics, ","),
			))
		}
		if process.availableMask&provenanceMetricSource == 0 {
			continue
		}
		reasons := provenanceInvalidReasons(process)
		if len(reasons) > 0 {
			invalid = append(invalid, fmt.Sprintf(
				"%s[%s](config_version=%q source_revision=%q source_modified=%q image_digest=%q)",
				provenanceLabel(process),
				strings.Join(reasons, ","),
				process.configVersion,
				process.sourceRevision,
				process.sourceModified,
				process.imageDigest,
			))
		}
	}
	conflicts := provenanceConflicts(current)

	findings := []finding{}
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/provenance", tier: tierWarn,
			class: "service-provenance-unobservable", target: "service-fleet", sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh service identities lack complete runtime artifact provenance",
				len(missing), len(current),
			),
			mechanism: "Fresh process RSS proves that the exact service identity is being scraped, but its process start, mutable config annotation, or immutable executable/image identity is absent on the same labels. Without process start the monitor cannot safely distinguish overlap generations; without source and digest it cannot determine which code the process executes. A green route and a desired registry tag do not close either gap.",
			baseline:  "Every newest actual-scrape-fresh API, Connect, Competitionworker, Proxy, and Taskworker identity exports process start, urnetwork_build_info.version, and urnetwork_source_info with full revision, modified state, and image digest on exact env/job/host/block/instance labels.",
			observed: fmt.Sprintf(
				"current_service_identities=%d complete_provenance_identities=%d missing_identities=%d missing=%s metrics_gateway=%s",
				len(current), len(current)-len(missing), len(missing), strings.Join(missing, ";"), metricHost.name,
			),
			evidence: fmt.Sprintf("Each of the four metric families is independently filtered by its source timestamp at no more than %.0f seconds old before the exact identity join. Newest process start suppresses a draining generation; a fresh RSS identity without start remains visible because its generation cannot be proven.", provenanceFreshness.Seconds()),
			context:  "WARP_VERSION is deliberately reported as config_version only. It can advance in a config-only rollout and is neither source ancestry nor immutable OCI identity. This is an observability and deployment-control failure, not proof that the intended fix is absent.",
			action:   "Build and deploy only the affected services from an intentional local checkout containing server commit 236bf0ce, then let ordinary host-serialized rollout replace each legacy generation. A config-only rollout cannot add the executable-owned source-info metric. Record the checkout base and any participating local diff, and do not substitute a tag, config generation, checkout HEAD alone, or BuildKit context attestation for the running executable and container identities.",
			verify:   "For two consecutive scrapes, every newest service identity has all four families; then independently inspect each running container's exact digest and the digest's extracted executable and require the same full revision and Boolean modified identity.",
			playbook: "SIGNALS.md §8.12",
		})
	}
	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/provenance", tier: tierWarn,
			class: "service-provenance-invalid", target: "service-fleet", sustain: 1,
			symptom: fmt.Sprintf(
				"%d of %d newest fresh service identities report malformed artifact provenance",
				len(invalid), len(current),
			),
			mechanism: "The source labels come from debug.ReadBuildInfo in the executable, while Warp injects the pulled image's inspected content digest before executing that digest. The Boolean modified bit is valid context for an intentional local-checkout build; a malformed revision, non-Boolean modified label, or malformed digest breaks the identity join. BuildKit context provenance alone is insufficient when an image copies a binary compiled earlier.",
			baseline:  "Every newest service identity reports a 40- or 64-hex Go VCS revision, source_modified=true or source_modified=false, and an exact sha256 OCI image content digest.",
			observed: fmt.Sprintf(
				"current_service_identities=%d invalid_identities=%d invalid=%s metrics_gateway=%s",
				len(current), len(invalid), strings.Join(invalid, ";"), metricHost.name,
			),
			evidence: "On 2026-09-01, six directly observed Taskworker blocks ran one exact image digest whose extracted executable reported modified base revision 078d6c11 while the image's Docker context attested a52392db. Symbols proved the intended code was present, but neither revision alone described that executable. This is why an intentional modified build must retain its checkout diff; modified=true itself is not malformed.",
			context:  "This is an identity-format failure, not proof that a behavioral fix is absent and not a ban on local modifications. Mutable WARP_VERSION/config generations cannot close it.",
			action:   "Repair the missing or malformed build identity and rebuild the affected service through the intentional local-checkout workflow. Preserve and record any participating local diff instead of discarding it merely to clear the monitor. Do not retag or reuse an artifact whose revision, Boolean modified field, or content digest cannot be parsed.",
			verify:   "For two consecutive scrapes, every newest identity reports an intended full revision, a Boolean modified value, and a valid digest; independently require the running container and the digest's extracted executable to report those same identities.",
			playbook: "SIGNALS.md §8.12",
		})
	}
	if len(conflicts) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/provenance", tier: tierWarn,
			class: "service-provenance-conflict", target: "service-fleet", sustain: 1,
			symptom: fmt.Sprintf(
				"%d immutable image digests claim conflicting executable source identities",
				len(conflicts),
			),
			mechanism: "One OCI content digest is immutable and therefore cannot legitimately contain two different executable revision/modified tuples for the same platform artifact. Conflicting fresh reports indicate collector-label contamination, a platform-manifest attribution error, or a runtime identity injection defect; any of those makes release verification unsafe.",
			baseline:  "Each exact sha256 image digest maps to one and only one executable source revision and modified state across the fresh service fleet.",
			observed:  fmt.Sprintf("conflicting_digests=%d conflicts=%s metrics_gateway=%s", len(conflicts), strings.Join(conflicts, ";"), metricHost.name),
			evidence:  "The comparison uses only newest actual-scrape-fresh identities with syntactically valid full revisions, Boolean modified labels, and exact sha256 digests; mutable tags and config versions are excluded from the equality key.",
			context:   "Do not infer which claimant is correct from service health or lexicographic version order. The immutable identity contract itself is broken.",
			action:    "Stop promotion of the conflicting artifact. Preserve the raw metric series, inspect the platform-specific manifest and running container digest for every named identity, extract each binary, and repair the first collector, manifest-selection, or runtime-injection mismatch before rebuilding from the intended local checkout.",
			verify:    "Two consecutive scrapes map every exact digest to one source tuple, and independent container plus extracted-binary inspection agrees with that tuple on each affected platform.",
			playbook:  "SIGNALS.md §8.12",
		})
	}
	if len(findings) == 0 {
		return []finding{healthyFinding("deploy/provenance", tierWarn, "service-provenance-invalid", "service-fleet")}, nil
	}
	return findings, nil
}

func newestProvenanceProcesses(processes map[string]*provenanceProcess) []*provenanceProcess {
	newest := map[string]*provenanceProcess{}
	missingStart := []*provenanceProcess{}
	for _, process := range processes {
		if process.availableMask&provenanceMetricRSS == 0 || process.rss <= 0 {
			continue
		}
		if process.availableMask&provenanceMetricStart == 0 {
			// It is a live denominator identity but cannot safely participate in
			// generation selection. Retain it as an explicit visibility failure.
			missingStart = append(missingStart, process)
			continue
		}
		slot := strings.Join([]string{process.job, process.host, process.block}, "\x00")
		if process.block == "" {
			slot += "\x00" + process.instance
		}
		previous := newest[slot]
		if previous == nil || process.start > previous.start ||
			(process.start == previous.start && process.instance > previous.instance) {
			newest[slot] = process
		}
	}
	current := make([]*provenanceProcess, 0, len(newest)+len(missingStart))
	for _, process := range newest {
		current = append(current, process)
	}
	current = append(current, missingStart...)
	return current
}

func provenanceLabel(process *provenanceProcess) string {
	label := process.job + "/" + process.host
	if process.block != "" {
		label += "/" + process.block
	}
	if process.instance != "" {
		label += "#" + process.instance
	}
	return label
}

func provenanceMissingMetrics(mask uint8) []string {
	metrics := []struct {
		bit  uint8
		name string
	}{
		{provenanceMetricStart, "process-start"},
		{provenanceMetricBuild, "build-info"},
		{provenanceMetricSource, "source-info"},
	}
	missing := []string{}
	for _, metric := range metrics {
		if mask&metric.bit == 0 {
			missing = append(missing, metric.name)
		}
	}
	return missing
}

func provenanceInvalidReasons(process *provenanceProcess) []string {
	reasons := []string{}
	if !validGoSourceRevision(process.sourceRevision) {
		reasons = append(reasons, "source-revision")
	}
	switch process.sourceModified {
	case "true", "false":
	default:
		reasons = append(reasons, "source-modified-label")
	}
	if !validOCIImageDigest(process.imageDigest) {
		reasons = append(reasons, "image-digest")
	}
	return reasons
}

func provenanceConflicts(processes []*provenanceProcess) []string {
	type lineage struct {
		revision string
		modified bool
	}
	byDigest := map[string]map[lineage][]string{}
	for _, process := range processes {
		if process.availableMask&provenanceMetricSource == 0 ||
			!validGoSourceRevision(process.sourceRevision) ||
			!validOCIImageDigest(process.imageDigest) {
			continue
		}
		if process.sourceModified != "false" && process.sourceModified != "true" {
			continue
		}
		modified := process.sourceModified == "true"
		lineages := byDigest[process.imageDigest]
		if lineages == nil {
			lineages = map[lineage][]string{}
			byDigest[process.imageDigest] = lineages
		}
		key := lineage{revision: process.sourceRevision, modified: modified}
		lineages[key] = append(lineages[key], provenanceLabel(process))
	}

	digests := make([]string, 0, len(byDigest))
	for digest, lineages := range byDigest {
		if len(lineages) > 1 {
			digests = append(digests, digest)
		}
	}
	sort.Strings(digests)
	conflicts := make([]string, 0, len(digests))
	for _, digest := range digests {
		lineages := byDigest[digest]
		parts := make([]string, 0, len(lineages))
		for source, identities := range lineages {
			sort.Strings(identities)
			parts = append(parts, fmt.Sprintf(
				"revision=%s,modified=%t,identities=%s",
				source.revision,
				source.modified,
				strings.Join(identities, ","),
			))
		}
		sort.Strings(parts)
		conflicts = append(conflicts, fmt.Sprintf("%s[%s]", digest, strings.Join(parts, "|")))
	}
	return conflicts
}
