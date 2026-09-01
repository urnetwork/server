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

const (
	releaseBuilderMarker = "monitor-signal-8.13-release-builder"
	releaseBuilderCommit = "217392e"
)

// Signal release-builder implements SIGNALS.md §8.13. It inspects the exact
// local and managed-host Warpctl executables so a clean tag or service image
// cannot hide a release builder that still permits dirty, mismatched binaries.
func NewReleaseBuilderSignal() Signal {
	return &signalAdapter{
		number: "8.13", key: "release-builder", name: "Warpctl release provenance enforcement",
		probe: releaseBuilderProbe{},
	}
}

type releaseBuilderProbe struct{}

func (releaseBuilderProbe) id() string             { return "deploy/release-builder" }
func (releaseBuilderProbe) tier() string           { return tierWarn }
func (releaseBuilderProbe) cadence() time.Duration { return 5 * time.Minute }

type releaseBuilderSample struct {
	path              string
	revision          string
	modified          string
	startCleanGuard   bool
	binaryCleanGuard  bool
	sourceStableGuard bool
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
	missingGuards := []string{}
	findings := []finding{}
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
		if missing := releaseBuilderMissingGuards(result.sample); len(missing) > 0 {
			missingGuards = append(missingGuards, fmt.Sprintf(
				"%s(path=%s revision=%s missing=%s)",
				result.target, result.sample.path, result.sample.revision,
				strings.Join(missing, ","),
			))
		}
	}

	if len(invalid) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/release-builder", tier: tierWarn,
			class: "warpctl-provenance-invalid", target: "warpctl-control-plane", sustain: 1,
			symptom:   fmt.Sprintf("%d Warpctl executable(s) have dirty or malformed embedded source provenance", len(invalid)),
			mechanism: "Warpctl is the release builder and service launcher. An executable with modified=true contains source not named by its embedded revision, so neither its own behavior nor an artifact it publishes is reproducible from that revision.",
			baseline:  "The monitor-host and every enabled managed-services host execute a Warpctl with a full 40- or 64-hex Go VCS revision and modified=false.",
			observed:  strings.Join(invalid, ";"),
			evidence:  "The probe scans Go build settings in the exact executable resolved by command -v warpctl on the monitor/release host and the canonical /usr/local/sbin/warpctl on managed hosts; it does not use a checkout, image tag, desired version, or file timestamp.",
			context:   "A dirty Warpctl does not prove every existing service artifact is dirty, but it cannot be trusted to establish clean release provenance. Hardware and a successful route do not repair this control-plane boundary.",
			action:    "Stop release builds through every listed executable. Use the intentional local-checkout workflows: rebuild the workstation executable through the current warp/warpctl/Makefile and rerun xops/main/ansible/run-edges.sh to build and install managed-host copies from the current local Warp checkout. Do not substitute a published or cached Warpctl. Require Warp commit 217392e or a clean descendant, then rebuild affected service images from clean source; do not retag or reuse an unverifiable binary or infer source from WARP_VERSION.",
			verify:    "Rerun this signal and require modified=false with one valid full revision on every target; then require each rebuilt service's urnetwork_source_info revision and image digest to match the extracted running executable for two scrapes.",
			playbook:  "SIGNALS.md §8.13 and §8.12",
		})
	} else if observable {
		findings = append(findings, healthyFinding(
			"deploy/release-builder", tierWarn, "warpctl-provenance-invalid", "warpctl-control-plane",
		))
	}

	if len(missingGuards) > 0 {
		findings = append(findings, finding{
			probeId: "deploy/release-builder", tier: tierWarn,
			class: "warpctl-release-guard-missing", target: "warpctl-control-plane", sustain: 1,
			symptom:   fmt.Sprintf("%d Warpctl executable(s) lack one or more fail-closed release provenance guards", len(missingGuards)),
			mechanism: "Legacy Warpctl can start from a dirty worktree, compile a binary whose embedded source differs from the later Docker context, and publish that image. A clean Warpctl executable alone is insufficient unless its release pipeline rejects a dirty start, a dirty/mismatched built binary, and source changes before publication.",
			baseline:  "Every exact Warpctl executable contains all three release gates introduced by Warp 217392e: clean starting source, clean matching binary provenance, and unchanged source through publication.",
			observed:  strings.Join(missingGuards, ";"),
			evidence:  "The probe checks three independent guard-specific strings in the exact executable and records its embedded revision. A missing string identifies code that cannot execute that corresponding fail-closed branch.",
			context:   "This is a software deployment and release-operations gate. Installing more capacity or redeploying a service with the same legacy builder cannot establish which source produced its executable.",
			action:    "Do not run another release build with the listed Warpctl copies. Use the intentional local-checkout workflows: rebuild the workstation executable through the current warp/warpctl/Makefile and rerun xops/main/ansible/run-edges.sh to build and install managed-host copies from the current local Warp checkout. Do not substitute a published or cached Warpctl. Require Warp 217392e or later, preserve §8.11 worker-freshness handling, then rebuild rather than retag affected artifacts.",
			verify:    "Every target reports start-clean, binary-clean, and source-stable guards present; a synthetic dirty build is rejected before compilation/publication, and the next clean service artifact reports matching executable revision and immutable image digest.",
			playbook:  "SIGNALS.md §8.13 and §8.12",
		})
	} else if observable {
		findings = append(findings, healthyFinding(
			"deploy/release-builder", tierWarn, "warpctl-release-guard-missing", "warpctl-control-plane",
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
	case "false":
	case "true":
		reasons = append(reasons, "source-modified")
	default:
		reasons = append(reasons, "source-modified-label")
	}
	return reasons
}

func releaseBuilderMissingGuards(sample releaseBuilderSample) []string {
	missing := []string{}
	if !sample.startCleanGuard {
		missing = append(missing, "start-clean")
	}
	if !sample.binaryCleanGuard {
		missing = append(missing, "binary-clean")
	}
	if !sample.sourceStableGuard {
		missing = append(missing, "source-stable")
	}
	return missing
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
		case "guard_start_clean", "guard_binary_clean", "guard_source_stable":
			parsed, err := strconv.Atoi(value)
			if err != nil || (parsed != 0 && parsed != 1) {
				return releaseBuilderSample{}, fmt.Errorf("release builder: invalid %s %q", key, value)
			}
			switch key {
			case "guard_start_clean":
				sample.startCleanGuard = parsed == 1
			case "guard_binary_clean":
				sample.binaryCleanGuard = parsed == 1
			case "guard_source_stable":
				sample.sourceStableGuard = parsed == 1
			}
		default:
			continue
		}
		seen[key] = true
	}
	for _, key := range []string{
		"path", "revision", "modified", "guard_start_clean",
		"guard_binary_clean", "guard_source_stable",
	} {
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
guard_start_clean=0
guard_binary_clean=0
guard_source_stable=0
if $grep_bin -aFq 'Git worktree is dirty; commit or remove changes before a release build' "$warpctl_path"; then
  guard_start_clean=1
fi
if $grep_bin -aFq 'was built from a modified source tree' "$warpctl_path"; then
  guard_binary_clean=1
fi
if $grep_bin -aFq 'release source changed during build' "$warpctl_path"; then
  guard_source_stable=1
fi
printf 'path %s\nrevision %s\nmodified %s\n' "$warpctl_path" "$revision" "$modified"
printf 'guard_start_clean %s\nguard_binary_clean %s\nguard_source_stable %s\n' \
  "$guard_start_clean" "$guard_binary_clean" "$guard_source_stable"
`
