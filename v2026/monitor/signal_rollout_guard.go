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
	rolloutGuardMarker          = "monitor-signal-8.11-rollout-guard"
	rolloutGuardFull            = "full-overlap"
	rolloutGuardDrainOnly       = "drain-only"
	rolloutGuardDisabled        = "disabled"
	rolloutGuardMissing         = "missing"
	rolloutGuardUnknown         = "unknown"
	rolloutGuardCommit          = "7e2075c"
	rolloutGuardValidatedCommit = "a85a277"
)

// Signal rollout-guard implements SIGNALS.md §8.11. The guard lives in each
// long-running Warp service worker, so installing a new warpctl binary is not
// sufficient: every worker must start after that binary was replaced.
func NewRolloutGuardSignal() Signal {
	return &signalAdapter{
		number: "8.11", key: "rollout-guard", name: "Fleet service rollout serialization and worker freshness",
		probe: rolloutGuardProbe{},
	}
}

type rolloutGuardProbe struct{}

func (rolloutGuardProbe) id() string             { return "deploy/rollout-guard" }
func (rolloutGuardProbe) tier() string           { return tierWarn }
func (rolloutGuardProbe) cadence() time.Duration { return 5 * time.Minute }

type rolloutGuardSample struct {
	managedHost              bool
	enabledUnits             int64
	runningUnits             int64
	guard                    string
	binaryChangeEpoch        int64
	oldestWorkerStartEpoch   int64
	newestWorkerStartEpoch   int64
	staleWorkerUnits         int64
	unverifiableWorkerUnits  int64
	guardDisabledUnits       int64
	staleWorkerNames         string
	unverifiableWorkerNames  string
	guardDisabledWorkerNames string
}

type rolloutGuardHostResult struct {
	host   *host
	sample rolloutGuardSample
	err    error
}

func (rolloutGuardProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, nil
	}

	command := "# " + rolloutGuardMarker + "\n" +
		"rollout_unit_pattern=" + shellSingleQuote("warp-"+env.cfg.env+"-*.service") + "\n" +
		rolloutGuardScript
	results := make(chan rolloutGuardHostResult, len(hosts))
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
				results <- rolloutGuardHostResult{host: target, err: ctx.Err()}
				return
			}
			output, err := env.runner.shell(ctx, target, command)
			if err != nil {
				results <- rolloutGuardHostResult{host: target, err: err}
				return
			}
			sample, err := parseRolloutGuardSample(output)
			results <- rolloutGuardHostResult{host: target, sample: sample, err: err}
		}()
	}
	wait.Wait()
	close(results)

	ordered := make([]rolloutGuardHostResult, 0, len(hosts))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].host.name < ordered[j].host.name })

	findings := make([]finding, 0, len(ordered))
	for _, result := range ordered {
		if result.err != nil {
			findings = append(findings, cannotObserveFinding(result.host.name+"/rollout-guard", result.err))
			continue
		}
		if !result.sample.managedHost {
			continue
		}
		findings = append(findings, evaluateRolloutGuard(result.host.name, result.sample)...)
	}
	return findings, nil
}

const rolloutGuardScript = `set -u
unit_file_rows=$(systemctl list-unit-files --type=service --no-legend --no-pager "$rollout_unit_pattern" 2>/dev/null || true)
running_unit_names=$(systemctl list-units --type=service --state=running --no-legend --no-pager --plain "$rollout_unit_pattern" 2>/dev/null |
  awk '$1 ~ /[.]service$/ {print $1}')
enabled_units=$(printf '%s\n' "$unit_file_rows" | awk '$2 ~ /^enabled/ {n++} END {print n+0}')
running_units=$(printf '%s\n' "$running_unit_names" | awk 'NF {n++} END {print n+0}')
if [ "$enabled_units" -eq 0 ] && [ "$running_units" -eq 0 ]; then
  printf 'managed_host 0\n'
  exit 0
fi
printf 'managed_host 1\nenabled_units %s\nrunning_units %s\n' "$enabled_units" "$running_units"

unit_file_names=$(printf '%s\n' "$unit_file_rows" | awk '$1 ~ /[.]service$/ {print $1}')
known_unit_names=$(printf '%s\n%s\n' "$unit_file_names" "$running_unit_names" | awk 'NF && !seen[$1]++ {print $1}')
guard_disabled_units=0
guard_disabled_names=
for unit in $known_unit_names; do
  if systemctl show "$unit" -p Environment --value 2>/dev/null | grep -Fq 'WARPCTL_STAGGER_HOST_DRAIN=0'; then
    guard_disabled_units=$((guard_disabled_units+1))
    guard_disabled_names="${guard_disabled_names}${guard_disabled_names:+,}${unit}"
  fi
done

warpctl_path=/usr/local/sbin/warpctl
rollout_guard=missing
if [ "$guard_disabled_units" -gt 0 ]; then
  rollout_guard=disabled
elif [ -r "$warpctl_path" ]; then
  if grep -aFq 'host rollout lock not acquired within' "$warpctl_path"; then
    rollout_guard=full-overlap
  elif grep -aFq 'Draining %d overlapping container(s) (staggered=%t)' "$warpctl_path"; then
    rollout_guard=drain-only
  else
    rollout_guard=unknown
  fi
fi
printf 'rollout_guard %s\nguard_disabled_units %s\nguard_disabled_worker_names %s\n' \
  "$rollout_guard" "$guard_disabled_units" "${guard_disabled_names:--}"

binary_change_epoch=$(stat -Lc %Z "$warpctl_path" 2>/dev/null || true)
case "$binary_change_epoch" in
  ''|*[!0-9]*) binary_change_epoch=0 ;;
esac
stale_worker_units=0
unverifiable_worker_units=0
stale_worker_names=
unverifiable_worker_names=
oldest_worker_start_epoch=0
newest_worker_start_epoch=0
for unit in $running_unit_names; do
  start_text=$(systemctl show "$unit" -p ExecMainStartTimestamp --value 2>/dev/null || true)
  start_epoch=
  if [ -n "$start_text" ]; then
    start_epoch=$(LC_ALL=C date -d "$start_text" +%s 2>/dev/null || true)
  fi
  case "$start_epoch" in
    ''|*[!0-9]*)
      unverifiable_worker_units=$((unverifiable_worker_units+1))
      unverifiable_worker_names="${unverifiable_worker_names}${unverifiable_worker_names:+,}${unit}"
      continue
      ;;
  esac
  if [ "$oldest_worker_start_epoch" -eq 0 ] || [ "$start_epoch" -lt "$oldest_worker_start_epoch" ]; then
    oldest_worker_start_epoch=$start_epoch
  fi
  if [ "$start_epoch" -gt "$newest_worker_start_epoch" ]; then
    newest_worker_start_epoch=$start_epoch
  fi
  if [ "$binary_change_epoch" -eq 0 ]; then
    unverifiable_worker_units=$((unverifiable_worker_units+1))
    unverifiable_worker_names="${unverifiable_worker_names}${unverifiable_worker_names:+,}${unit}"
  elif [ "$start_epoch" -lt "$binary_change_epoch" ]; then
    stale_worker_units=$((stale_worker_units+1))
    stale_worker_names="${stale_worker_names}${stale_worker_names:+,}${unit}"
  fi
done
printf 'binary_change_epoch %s\noldest_worker_start_epoch %s\nnewest_worker_start_epoch %s\n' \
  "$binary_change_epoch" "$oldest_worker_start_epoch" "$newest_worker_start_epoch"
printf 'stale_worker_units %s\nunverifiable_worker_units %s\n' \
  "$stale_worker_units" "$unverifiable_worker_units"
printf 'stale_worker_names %s\nunverifiable_worker_names %s\n' \
  "${stale_worker_names:--}" "${unverifiable_worker_names:--}"
`

func parseRolloutGuardSample(output string) (rolloutGuardSample, error) {
	sample := rolloutGuardSample{}
	managedHostValue := int64(-1)
	values := map[string]*int64{
		"managed_host":              &managedHostValue,
		"enabled_units":             &sample.enabledUnits,
		"running_units":             &sample.runningUnits,
		"binary_change_epoch":       &sample.binaryChangeEpoch,
		"oldest_worker_start_epoch": &sample.oldestWorkerStartEpoch,
		"newest_worker_start_epoch": &sample.newestWorkerStartEpoch,
		"stale_worker_units":        &sample.staleWorkerUnits,
		"unverifiable_worker_units": &sample.unverifiableWorkerUnits,
		"guard_disabled_units":      &sample.guardDisabledUnits,
	}
	stringsByKey := map[string]*string{
		"stale_worker_names":          &sample.staleWorkerNames,
		"unverifiable_worker_names":   &sample.unverifiableWorkerNames,
		"guard_disabled_worker_names": &sample.guardDisabledWorkerNames,
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if key == "rollout_guard" {
			switch value {
			case rolloutGuardFull, rolloutGuardDrainOnly, rolloutGuardDisabled, rolloutGuardMissing, rolloutGuardUnknown:
				sample.guard = value
				seen[key] = true
				continue
			default:
				return rolloutGuardSample{}, fmt.Errorf("rollout guard: invalid %s %q", key, value)
			}
		}
		if destination, ok := stringsByKey[key]; ok {
			if value == "" || strings.ContainsAny(value, " \t") {
				return rolloutGuardSample{}, fmt.Errorf("rollout guard: invalid %s %q", key, value)
			}
			*destination = value
			seen[key] = true
			continue
		}
		destination, ok := values[key]
		if !ok {
			continue
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return rolloutGuardSample{}, fmt.Errorf("rollout guard: invalid %s %q", key, value)
		}
		*destination = parsed
		seen[key] = true
	}
	if !seen["managed_host"] || (managedHostValue != 0 && managedHostValue != 1) {
		return rolloutGuardSample{}, fmt.Errorf("rollout guard: observation omitted or invalid managed_host")
	}
	sample.managedHost = managedHostValue == 1
	if !sample.managedHost {
		return sample, nil
	}
	for _, key := range []string{
		"enabled_units", "running_units", "rollout_guard", "guard_disabled_units",
		"binary_change_epoch", "oldest_worker_start_epoch", "newest_worker_start_epoch",
		"stale_worker_units", "unverifiable_worker_units", "stale_worker_names",
		"unverifiable_worker_names", "guard_disabled_worker_names",
	} {
		if !seen[key] {
			return rolloutGuardSample{}, fmt.Errorf("rollout guard: observation omitted %s", key)
		}
	}
	if sample.staleWorkerUnits+sample.unverifiableWorkerUnits > sample.runningUnits {
		return rolloutGuardSample{}, fmt.Errorf("rollout guard: worker freshness counts exceed running units")
	}
	if (sample.guard == rolloutGuardDisabled) != (sample.guardDisabledUnits > 0) {
		return rolloutGuardSample{}, fmt.Errorf("rollout guard: disabled status and unit count disagree")
	}
	if sample.oldestWorkerStartEpoch > sample.newestWorkerStartEpoch {
		return rolloutGuardSample{}, fmt.Errorf("rollout guard: worker start epochs are inconsistent")
	}
	return sample, nil
}

func evaluateRolloutGuard(host string, sample rolloutGuardSample) []finding {
	observed := fmt.Sprintf(
		"rollout_guard=%s enabled_units=%d running_units=%d binary_change_epoch=%d oldest_worker_start_epoch=%d newest_worker_start_epoch=%d stale_worker_units=%d unverifiable_worker_units=%d guard_disabled_units=%d stale_worker_names=%s unverifiable_worker_names=%s guard_disabled_worker_names=%s",
		sample.guard, sample.enabledUnits, sample.runningUnits, sample.binaryChangeEpoch,
		sample.oldestWorkerStartEpoch, sample.newestWorkerStartEpoch, sample.staleWorkerUnits,
		sample.unverifiableWorkerUnits, sample.guardDisabledUnits, sample.staleWorkerNames,
		sample.unverifiableWorkerNames, sample.guardDisabledWorkerNames,
	)
	commonContext := "This is a software deployment gate shared by API, Connect, taskworker, proxy, and every other Warp-managed service on the host. It is separate from proxy hardware capacity: serialization prevents accidental fleet overlap but does not create RAM, CPU, host slots, or active-client capacity."
	commonVerify := "Every managed services host reports rollout_guard=full-overlap, every running Warp service worker started at or after the installed binary change time, and one controlled rollout keeps candidate overlap within the configured host bound. Do not validate this by launching a full proxy-fleet rollout."

	switch sample.guard {
	case rolloutGuardDrainOnly:
		return []finding{{
			probeId: "deploy/rollout-guard", tier: tierWarn,
			class: "rollout-guard-stale", target: host, frame: sample.guard, sustain: 1,
			symptom:   fmt.Sprintf("%s runs the legacy drain-only Warp rollout guard", host),
			mechanism: "The installed Warp binary acquires its host lease only around old-container drain. Independent service workers can all start candidates before any worker takes that lease, permitting almost a complete duplicate fleet to coexist and exhaust a constrained host.",
			baseline:  "The host lease begins before candidate start, remains held through synchronous old-container drain, and refuses replacement when lock acquisition times out.",
			observed:  observed,
			context:   commonContext,
			action:    fmt.Sprintf("Deploy validated Warp commit %s (which contains rollout root fix %s) or later, then restart every running Warp service worker on this host before any service release. Do not test the fix with a full proxy rollout.", rolloutGuardValidatedCommit, rolloutGuardCommit),
			verify:    commonVerify,
			playbook:  "SIGNALS.md §8.11",
		}}
	case rolloutGuardDisabled:
		return []finding{{
			probeId: "deploy/rollout-guard", tier: tierWarn,
			class: "rollout-guard-disabled", target: host, frame: sample.guard, sustain: 1,
			symptom:   fmt.Sprintf("%s explicitly disables the full-overlap Warp rollout guard", host),
			mechanism: "One or more Warp service units set WARPCTL_STAGGER_HOST_DRAIN=0. That override bypasses the host lease, so workers may start candidates concurrently even if the installed binary contains the complete-overlap fix.",
			baseline:  "No managed service unit disables host serialization, and candidate start through old-container drain is covered by one host lease.",
			observed:  observed,
			context:   commonContext,
			action:    fmt.Sprintf("Remove WARPCTL_STAGGER_HOST_DRAIN=0 from every named unit, deploy validated Warp commit %s (containing %s) or later if needed, reload systemd, and restart every running Warp service worker before any service release. Do not test with a full proxy rollout.", rolloutGuardValidatedCommit, rolloutGuardCommit),
			verify:    commonVerify,
			playbook:  "SIGNALS.md §8.11",
		}}
	case rolloutGuardMissing, rolloutGuardUnknown:
		return []finding{{
			probeId: "deploy/rollout-guard", tier: tierWarn,
			class: "rollout-guard-unverified", target: host, frame: sample.guard, sustain: 1,
			symptom:   fmt.Sprintf("%s rollout serialization cannot be verified", host),
			mechanism: "The services host has no readable /usr/local/sbin/warpctl or the installed executable exposes neither the complete-overlap nor known legacy signature. The monitor therefore cannot prove what code long-running workers will execute for the next deployment.",
			baseline:  "The installed Warp executable exposes the full-overlap host-lock signature and no managed unit disables it.",
			observed:  observed,
			context:   commonContext,
			action:    fmt.Sprintf("Resolve the executable used by the units, deploy validated Warp commit %s (containing %s) or later, and restart every running Warp service worker. Do not begin a service rollout or use a full proxy rollout as the discovery test.", rolloutGuardValidatedCommit, rolloutGuardCommit),
			verify:    commonVerify,
			playbook:  "SIGNALS.md §8.11",
		}}
	case rolloutGuardFull:
		if sample.staleWorkerUnits == 0 && sample.unverifiableWorkerUnits == 0 {
			return []finding{healthyFinding("deploy/rollout-guard", tierWarn, "rollout-guard-workers-stale", host)}
		}
		return []finding{{
			probeId: "deploy/rollout-guard", tier: tierWarn,
			class: "rollout-guard-workers-stale", target: host, frame: "worker-lifecycle", sustain: 1,
			symptom: fmt.Sprintf("%s has %d stale and %d unverifiable Warp service worker(s) after the binary replacement",
				host, sample.staleWorkerUnits, sample.unverifiableWorkerUnits),
			mechanism: "Warp service workers are long-running processes. Replacing /usr/local/sbin/warpctl changes the executable on disk but does not replace code already mapped by an older worker; that worker will continue to run the old rollout path until it restarts. An unparseable start time leaves the same deployment boundary unproven.",
			baseline:  "Every running Warp service worker has a parseable ExecMainStartTimestamp at or after /usr/local/sbin/warpctl's inode-change time.",
			observed:  observed,
			context:   commonContext,
			action:    "Restart every stale or unverifiable Warp service worker named in the observation, preserving normal service availability and generation handoff. Re-run this probe before any service release; reinstalling the same binary without worker restarts is insufficient.",
			verify:    commonVerify,
			playbook:  "SIGNALS.md §8.11",
		}}
	default:
		return []finding{cannotObserveFinding(host+"/rollout-guard", fmt.Errorf("unsupported rollout guard %q", sample.guard))}
	}
}
