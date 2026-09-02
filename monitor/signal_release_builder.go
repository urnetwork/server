package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const releaseBuilderMarker = "monitor-signal-8.13-release-builder"

// Signal release-builder implements SIGNALS.md §8.13. It inventories the exact
// local and managed-host Warpctl executables built from the operator's local
// checkout. A dirty checkout is an intentional supported source, so its Go VCS
// modified bit is identity context rather than a release failure.
func NewReleaseBuilderSignal() Signal {
	return &signalAdapter{
		number: "8.13", key: "release-builder", name: "Warpctl local-checkout executable identity",
		probe: releaseBuilderProbe{},
	}
}

type releaseBuilderProbe struct{}

func (releaseBuilderProbe) id() string             { return "deploy/release-builder" }
func (releaseBuilderProbe) tier() string           { return tierWarn }
func (releaseBuilderProbe) cadence() time.Duration { return 5 * time.Minute }

type releaseBuilderSample struct {
	path     string
	revision string
	modified string
}

type releaseBuilderResult struct {
	target string
	sample releaseBuilderSample
	err    error
}

func (releaseBuilderProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	localCommand := "# " + releaseBuilderMarker + "\n" + releaseBuilderScript
	hostCommand := "# " + releaseBuilderMarker + "\n" +
		"release_builder_path=/usr/local/sbin/warpctl\n" + releaseBuilderScript
	results := []releaseBuilderResult{}

	localOutput, localErr := env.runner.local(ctx, "sh", "-c", localCommand)
	localSample := releaseBuilderSample{}
	if localErr == nil {
		localSample, localErr = parseReleaseBuilderSample(localOutput)
	}
	results = append(results, releaseBuilderResult{
		target: "monitor-host", sample: localSample, err: localErr,
	})

	hosts := env.cfg.hostsWithRole("services")
	hostResults := make(chan releaseBuilderResult, len(hosts))
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
				hostResults <- releaseBuilderResult{target: target.name, err: ctx.Err()}
				return
			}
			output, err := env.runner.shell(ctx, target, hostCommand)
			sample := releaseBuilderSample{}
			if err == nil {
				sample, err = parseReleaseBuilderSample(output)
			}
			hostResults <- releaseBuilderResult{target: target.name, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(hostResults)
	for result := range hostResults {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].target < results[j].target })

	observable := true
	invalid := []string{}
	// Clear state left by watcher binaries that implemented the withdrawn
	// fail-closed release policy. Guard absence is now intentional in Warp.
	findings := []finding{healthyFinding(
		"deploy/release-builder", tierWarn, "warpctl-release-guard-missing", "warpctl-control-plane",
	)}
	for _, result := range results {
		if result.err != nil {
			observable = false
			findings = append(findings, cannotObserveFinding(
				result.target+"/warpctl-provenance", result.err,
			))
			continue
		}
		if reasons := releaseBuilderInvalidReasons(result.sample); len(reasons) > 0 {
			invalid = append(invalid, fmt.Sprintf(
				"%s(path=%s revision=%s modified=%s reasons=%s)",
				result.target, result.sample.path, result.sample.revision,
				result.sample.modified, strings.Join(reasons, ","),
			))
		}
	}

	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/release-builder", tier: tierWarn,
			class: "warpctl-provenance-invalid", target: "warpctl-control-plane", sustain: 1,
			symptom:   fmt.Sprintf("%d Warpctl executable(s) lack parseable embedded local-checkout identity", len(invalid)),
			mechanism: "Warpctl is built directly from an operator-controlled local checkout. Its Go VCS revision names the checkout base and its Boolean modified bit records whether local changes participated. A missing or malformed field prevents even that bounded attribution; modified=true by itself is intentional and is not a defect.",
			baseline:  "The monitor-host and every enabled managed-services host execute a Warpctl with one full 40- or 64-hex Go VCS revision and a Boolean modified label; either true or false is valid.",
			observed:  strings.Join(invalid, ";"),
			evidence:  "The probe scans Go build settings in the exact executable resolved by command -v warpctl on the monitor/release host and the canonical /usr/local/sbin/warpctl on managed hosts; it does not use a service image tag, desired version, install timestamp, or a different Warpctl copy.",
			context:   "This signal does not require a clean checkout and does not establish the exact content of an intentionally modified build. Preserve the owning checkout and diff when exact replay matters. Hardware, a service tag, or a successful route cannot reconstruct malformed identity.",
			action:    "Rebuild each listed executable through the intentional local-checkout workflow: use the current warp/warpctl/Makefile for the workstation copy and xops/main/ansible/run-edges.sh for managed-host copies. Do not substitute a published or cached Warpctl. Do not discard an intentional local diff merely to clear this warning; the fault is the missing or malformed identity field.",
			verify:    "Every exact executable reports one full revision and modified=true or modified=false. A synthetic dirty local build remains accepted, while a missing revision or non-Boolean modified label remains visible. Validate each deployed service artifact independently through §8.12.",
			playbook:  "SIGNALS.md §8.13 and §8.12",
		})
	} else if observable {
		findings = append(findings, healthyFinding(
			"deploy/release-builder", tierWarn, "warpctl-provenance-invalid", "warpctl-control-plane",
		))
	}

	return findings, nil
}

func releaseBuilderInvalidReasons(sample releaseBuilderSample) []string {
	reasons := []string{}
	if !validGoSourceRevision(sample.revision) {
		reasons = append(reasons, "source-revision")
	}
	switch sample.modified {
	case "false", "true":
	default:
		reasons = append(reasons, "source-modified-label")
	}
	return reasons
}

func parseReleaseBuilderSample(output string) (releaseBuilderSample, error) {
	sample := releaseBuilderSample{}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "path":
			sample.path = value
		case "revision":
			sample.revision = value
		case "modified":
			sample.modified = value
		default:
			continue
		}
		seen[key] = true
	}
	for _, key := range []string{"path", "revision", "modified"} {
		if !seen[key] {
			return releaseBuilderSample{}, fmt.Errorf("release builder: observation omitted %s", key)
		}
	}
	return sample, nil
}

const releaseBuilderScript = `set -eu
warpctl_path=${release_builder_path:-}
if [ -z "$warpctl_path" ]; then
  warpctl_path=$(command -v warpctl 2>/dev/null || true)
fi
if [ -z "$warpctl_path" ] || [ ! -r "$warpctl_path" ]; then
  echo 'warpctl executable is absent or unreadable' >&2
  exit 2
fi
grep_bin=$(command -v grep 2>/dev/null || true)
if [ -z "$grep_bin" ]; then
  echo 'grep executable is unavailable' >&2
  exit 2
fi
tab=$(printf '\t')
revision=$($grep_bin -aEo "build${tab}vcs[.]revision=[0-9a-f]{40}([0-9a-f]{24})?" "$warpctl_path" | sed 's/.*=//' | sort -u | paste -sd, -)
modified=$($grep_bin -aEo "build${tab}vcs[.]modified=(true|false)" "$warpctl_path" | sed 's/.*=//' | sort -u | paste -sd, -)
[ -n "$revision" ] || revision=-
[ -n "$modified" ] || modified=-
printf 'path %s\nrevision %s\nmodified %s\n' "$warpctl_path" "$revision" "$modified"
`
