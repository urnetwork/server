package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const hostpowerMarker = "monitor-signal-21.2-hostpower"

// hostpowerCommand reduces effective policy, current-boot suspend history,
// physical failure-domain equality, and detailed media-health capability on
// the host. It never returns device names, sysfs paths, serials, addresses, or
// raw journal/SMART output.
const hostpowerCommand = `# ` + hostpowerMarker + `
set -u

effective_value() {
  key=$1
  awk -F= -v key="$key" '
    $1 ~ "^[[:space:]]*" key "[[:space:]]*$" {value=$2}
    END {gsub(/[[:space:]]/, "", value); print value}
  '
}

sleep_config=$(systemd-analyze cat-config systemd/sleep.conf 2>/dev/null) || exit 31
sleep_policy=deny-all
for pair in AllowSuspend=no AllowHibernation=no AllowSuspendThenHibernate=no AllowHybridSleep=no; do
  key=${pair%%=*}
  expected=${pair#*=}
  value=$(printf '%s\n' "$sleep_config" | effective_value "$key")
  [ "$value" = "$expected" ] || sleep_policy=unsafe
done

logind_config=$(systemd-analyze cat-config systemd/logind.conf 2>/dev/null) || exit 32
logind_policy=ignore-all
for pair in HandleLidSwitch=ignore HandleLidSwitchExternalPower=ignore HandleLidSwitchDocked=ignore IdleAction=ignore; do
  key=${pair%%=*}
  expected=${pair#*=}
  value=$(printf '%s\n' "$logind_config" | effective_value "$key")
  [ "$value" = "$expected" ] || logind_policy=unsafe
done

desktop_policy=nothing-all
for key in sleep-inactive-ac-type sleep-inactive-battery-type lid-close-ac-action lid-close-battery-action; do
  value=$(DCONF_PROFILE=user gsettings get org.gnome.settings-daemon.plugins.power "$key" 2>/dev/null) || exit 33
  [ "$value" = "'nothing'" ] || desktop_policy=unsafe
done

timeout 10s journalctl -q -k -b 0 -n 0 --no-pager >/dev/null 2>&1 || exit 34
suspend_log=$(timeout 10s journalctl -q -k -b 0 --since '30 days ago' \
  --grep 'PM: suspend (entry|exit)' -n 128 --no-pager -o short-unix 2>/dev/null)
journal_status=$?
case "$journal_status" in 0|1) ;; *) exit 34 ;; esac
set -- $(printf '%s\n' "$suspend_log" | awk '
  /PM: suspend entry/ {
    split($1, stamp, "."); pending=stamp[1]+0; entries++; latest_suspend=pending
  }
  /PM: suspend exit/ {
    split($1, stamp, "."); resumed=stamp[1]+0; resumes++; latest_resume=resumed
    if (pending > 0 && resumed >= pending) {
      duration=resumed-pending; pairs++
      if (duration > maximum) maximum=duration
      pending=0
    }
  }
  END {
    printf "%d %d %d %d %d %d %d\n", entries+0, resumes+0, pairs+0,
      latest_suspend+0, latest_resume+0, maximum+0, (pending > 0)
  }
')
[ "$#" -eq 7 ] || exit 35
suspend_entries=$1
resume_entries=$2
suspend_pairs=$3
latest_suspend_epoch=$4
latest_resume_epoch=$5
max_suspend_seconds=$6
unmatched_suspend=$7

topology_state=not-applicable
media_health=not-applicable
if [ "$archive_expected" -eq 1 ]; then
  topology_state=unobservable
  media_health=unobservable
  hardware=$(sudo -n /var/bringyour/backup/hostpower-hardware.sh 2>/dev/null)
  hardware_status=$?
  if [ "$hardware_status" -eq 0 ] &&
     [ "$(printf '%s\n' "$hardware" | awk 'NF {count++} END {print count+0}')" -eq 2 ]; then
    topology_candidate=$(printf '%s\n' "$hardware" | sed -n 's/^topology_state=//p')
    media_candidate=$(printf '%s\n' "$hardware" | sed -n 's/^media_health=//p')
    case "$topology_candidate" in shared-removable|separate|unobservable) topology_state=$topology_candidate ;; esac
    case "$media_candidate" in healthy|failed|unobservable) media_health=$media_candidate ;; esac
  fi
fi

printf '%s\n' \
  'observation_schema=1' \
  "sleep_policy=$sleep_policy" \
  "logind_policy=$logind_policy" \
  "desktop_policy=$desktop_policy" \
  "suspend_entries=$suspend_entries" \
  "resume_entries=$resume_entries" \
  "suspend_pairs=$suspend_pairs" \
  "latest_suspend_epoch=$latest_suspend_epoch" \
  "latest_resume_epoch=$latest_resume_epoch" \
  "max_suspend_seconds=$max_suspend_seconds" \
  "unmatched_suspend=$unmatched_suspend" \
  "topology_state=$topology_state" \
  "media_health=$media_health"
`

// Signal hostpower implements SIGNALS.md §21.2.
func NewHostpowerSignal() Signal {
	return &signalAdapter{
		number: "21.2", key: "hostpower", name: "Stationary host power and shared removable failure domains",
		probe: hostpowerProbe{},
	}
}

type hostpowerProbe struct{}

func (hostpowerProbe) id() string             { return "host/power-policy" }
func (hostpowerProbe) tier() string           { return tierWarn }
func (hostpowerProbe) cadence() time.Duration { return 5 * time.Minute }

type hostpowerSample struct {
	sleepPolicy      string
	logindPolicy     string
	desktopPolicy    string
	suspendEntries   int64
	resumeEntries    int64
	suspendPairs     int64
	latestSuspend    time.Time
	latestResume     time.Time
	maxSuspend       time.Duration
	unmatchedSuspend bool
	topologyState    string
	mediaHealth      string
}

func (hostpowerProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hostsByName := map[string]*host{}
	for _, role := range []string{"backup", "stationary"} {
		for _, target := range env.cfg.hostsWithRole(role) {
			hostsByName[target.name] = target
		}
	}
	if len(hostsByName) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(hostsByName))
	for name := range hostsByName {
		names = append(names, name)
	}
	sort.Strings(names)

	findings := []finding{}
	for _, name := range names {
		target := hostsByName[name]
		archiveExpected := target.hasRole("backup") || target.backup != nil
		command := "archive_expected=0\n" + hostpowerCommand
		if archiveExpected {
			command = "archive_expected=1\n" + hostpowerCommand
		}
		output, err := env.runner.shell(ctx, target, command)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/hostpower", err))
			continue
		}
		sample, err := parseHostpowerSample(output, env.now().UTC())
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/hostpower", err))
			continue
		}
		findings = append(findings, evaluateHostpower(target.name, sample)...)
	}
	return findings, nil
}

func parseHostpowerSample(raw string, now time.Time) (hostpowerSample, error) {
	keys := []string{
		"observation_schema", "sleep_policy", "logind_policy", "desktop_policy",
		"suspend_entries", "resume_entries", "suspend_pairs", "latest_suspend_epoch",
		"latest_resume_epoch", "max_suspend_seconds", "unmatched_suspend",
		"topology_state", "media_health",
	}
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	values := map[string]string{}
	for _, rawLine := range strings.Split(raw, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowed[key] || strings.TrimSpace(value) == "" {
			return hostpowerSample{}, fmt.Errorf("hostpower: malformed or unexpected observation field")
		}
		if _, duplicate := values[key]; duplicate {
			return hostpowerSample{}, fmt.Errorf("hostpower: duplicate observation field")
		}
		values[key] = strings.TrimSpace(value)
	}
	for _, key := range keys {
		if values[key] == "" {
			return hostpowerSample{}, fmt.Errorf("hostpower: observation omitted %s", key)
		}
	}
	if values["observation_schema"] != "1" {
		return hostpowerSample{}, fmt.Errorf("hostpower: unsupported observation schema")
	}
	if values["sleep_policy"] != "deny-all" && values["sleep_policy"] != "unsafe" {
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid sleep policy")
	}
	if values["logind_policy"] != "ignore-all" && values["logind_policy"] != "unsafe" {
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid logind policy")
	}
	if values["desktop_policy"] != "nothing-all" && values["desktop_policy"] != "unsafe" {
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid desktop policy")
	}
	parseCount := func(key string) (int64, error) {
		value, err := strconv.ParseInt(values[key], 10, 64)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("hostpower: invalid %s", key)
		}
		return value, nil
	}
	entries, err := parseCount("suspend_entries")
	if err != nil {
		return hostpowerSample{}, err
	}
	resumes, err := parseCount("resume_entries")
	if err != nil {
		return hostpowerSample{}, err
	}
	pairs, err := parseCount("suspend_pairs")
	if err != nil {
		return hostpowerSample{}, err
	}
	suspendEpoch, err := parseCount("latest_suspend_epoch")
	if err != nil {
		return hostpowerSample{}, err
	}
	resumeEpoch, err := parseCount("latest_resume_epoch")
	if err != nil {
		return hostpowerSample{}, err
	}
	maxSuspendSeconds, err := parseCount("max_suspend_seconds")
	if err != nil {
		return hostpowerSample{}, err
	}
	if pairs > entries || pairs > resumes || (entries == 0) != (suspendEpoch == 0) ||
		(resumes == 0) != (resumeEpoch == 0) || (pairs == 0) != (maxSuspendSeconds == 0) {
		return hostpowerSample{}, fmt.Errorf("hostpower: contradictory suspend history")
	}
	unmatched := false
	switch values["unmatched_suspend"] {
	case "0":
	case "1":
		unmatched = true
	default:
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid unmatched suspend state")
	}
	if unmatched != (entries > pairs) {
		return hostpowerSample{}, fmt.Errorf("hostpower: contradictory unmatched suspend state")
	}
	latestSuspend := time.Time{}
	if suspendEpoch > 0 {
		latestSuspend = time.Unix(suspendEpoch, 0).UTC()
	}
	latestResume := time.Time{}
	if resumeEpoch > 0 {
		latestResume = time.Unix(resumeEpoch, 0).UTC()
	}
	if latestSuspend.After(now.Add(5*time.Minute)) || latestResume.After(now.Add(5*time.Minute)) {
		return hostpowerSample{}, fmt.Errorf("hostpower: suspend history is in the future")
	}
	topology := values["topology_state"]
	if topology != "shared-removable" && topology != "separate" && topology != "unobservable" && topology != "not-applicable" {
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid topology state")
	}
	media := values["media_health"]
	if media != "healthy" && media != "failed" && media != "unobservable" && media != "not-applicable" {
		return hostpowerSample{}, fmt.Errorf("hostpower: invalid media health")
	}
	if (topology == "not-applicable") != (media == "not-applicable") {
		return hostpowerSample{}, fmt.Errorf("hostpower: contradictory archive hardware state")
	}
	return hostpowerSample{
		sleepPolicy: values["sleep_policy"], logindPolicy: values["logind_policy"], desktopPolicy: values["desktop_policy"],
		suspendEntries: entries, resumeEntries: resumes, suspendPairs: pairs,
		latestSuspend: latestSuspend, latestResume: latestResume,
		maxSuspend: time.Duration(maxSuspendSeconds) * time.Second, unmatchedSuspend: unmatched,
		topologyState: topology, mediaHealth: media,
	}, nil
}

func evaluateHostpower(target string, sample hostpowerSample) []finding {
	findings := []finding{}
	probeId := "host/power-policy"
	if sample.sleepPolicy == "deny-all" && sample.logindPolicy == "ignore-all" && sample.desktopPolicy == "nothing-all" {
		findings = append(findings, healthyFinding(probeId, tierPage, "hostpower-suspend-policy-unsafe", target))
	} else {
		findings = append(findings, finding{
			probeId: probeId, tier: tierPage, class: "hostpower-suspend-policy-unsafe", target: target, sustain: 1,
			symptom:   fmt.Sprintf("stationary host %s still permits a session, idle, lid, or system sleep path", target),
			mechanism: "A stationary backup host that suspends on battery removes its management path, telemetry publisher, and archive writers at the same time a site or dock power event needs them most.",
			baseline:  "Effective systemd sleep policy denies suspend and hibernation, logind ignores lid and idle actions, and locked GNOME AC/battery idle and lid actions are all nothing.",
			observed:  fmt.Sprintf("system_sleep=%s logind=%s desktop=%s", sample.sleepPolicy, sample.logindPolicy, sample.desktopPolicy),
			evidence:  "The host reduces effective layered systemd configuration and locked desktop values to bounded policy classes; it does not export users, sessions, or configuration file contents.",
			context:   "This is a software-owned policy hazard. It is separate from archive filesystem recovery in §11.22 and from the physical cause of an AC or dock loss.",
			action:    "After archive recovery is complete, apply the reviewed stationary-server Xops policy. Do not restart logind or trigger sleep as a test; verify effective configuration in place.",
			verify:    "The effective systemd, logind, and locked GNOME reductions are all safe for two probes, and no new same-boot suspend entry occurs.",
			playbook:  "SIGNALS.md §21.2 and §11.22",
		})
	}

	if sample.suspendEntries == 0 {
		findings = append(findings, healthyFinding(probeId, tierPage, "hostpower-suspend-observed", target))
	} else {
		tier := tierWarn
		if sample.unmatchedSuspend || sample.maxSuspend >= 5*time.Minute {
			tier = tierPage
		}
		latestSuspend := sample.latestSuspend.Format(time.RFC3339)
		latestResume := "none-in-bounded-current-boot-evidence"
		if !sample.latestResume.IsZero() {
			latestResume = sample.latestResume.Format(time.RFC3339)
		}
		findings = append(findings, finding{
			probeId: probeId, tier: tier, class: "hostpower-suspend-observed", target: target, sustain: 1,
			symptom:   fmt.Sprintf("stationary host %s entered suspend during its current boot", target),
			mechanism: "Kernel current-boot evidence proves a suspend entry. A paired exit gives the unavailable interval; an unmatched entry means the bounded journal cannot prove a corresponding resume.",
			baseline:  "A stationary backup host records no kernel suspend entry during its current boot.",
			observed:  fmt.Sprintf("entries=%d resumes=%d paired=%d latest_suspend=%s latest_resume=%s longest_paired_suspend=%s unmatched=%t", sample.suspendEntries, sample.resumeEntries, sample.suspendPairs, latestSuspend, latestResume, sample.maxSuspend, sample.unmatchedSuspend),
			evidence:  "At most 128 kernel suspend entry/exit records from the current boot and last 30 days are reduced on-host to counts, timestamps, and duration. No unrelated journal text leaves the host.",
			context:   "Correlate this interval with §11.22 archive, writer, and telemetry transitions. Temporal overlap localizes host unavailability but does not by itself name the upstream power, dock, or link failure.",
			action:    "Preserve the same-boot evidence, inspect the bounded power/link transition, and correct the effective suspend policy. Recover and clear the archive filesystem independently under §11.22 before enabling writers or timers.",
			verify:    "No new suspend entry occurs through the next power-path disturbance; management and telemetry stay reachable on battery, and §11.22 independently proves the archive recovered.",
			playbook:  "SIGNALS.md §21.2 and §11.22",
		})
	}

	switch sample.topologyState {
	case "shared-removable":
		findings = append(findings, finding{
			probeId: probeId, tier: tierWarn, class: "hostpower-shared-removable-domain", target: target, frame: "management-archive-shared-removable", sustain: 1,
			symptom:   fmt.Sprintf("stationary host %s places its management uplink and archive device behind one removable bus domain", target),
			mechanism: "The active management uplink and archive disk resolve beneath the same Thunderbolt or USB ancestor. One dock, cable, bus, or power failure can therefore remove recovery access and archive storage together.",
			baseline:  "Management access and the archive device do not share one removable Thunderbolt/USB failure domain, or an independently powered and observable recovery path is documented and tested.",
			observed:  "shared_removable_failure_domain=true",
			evidence:  "Ancestor equality is computed on-host. Only the Boolean class leaves the host; interface names, block names, sysfs paths, serials, MACs, and topology are discarded.",
			context:   "This is an operational/hardware risk, not a software defect. §11.22 owns archive filesystem and writer recovery; this signal only identifies the common physical failure boundary.",
			action:    "Provide an independently powered management path or separate the archive from the management dock/bus. Do not treat a software deployment or a green filesystem check as removal of the shared physical failure domain.",
			verify:    "A reviewed physical-path test proves independent management reachability when the archive attachment is removed, and the on-host topology reduction reports separate.",
			playbook:  "SIGNALS.md §21.2 and §11.22",
		})
	case "unobservable":
		findings = append(findings, finding{
			probeId: probeId, tier: tierWarn, class: "hostpower-topology-unobservable", target: target, sustain: 2,
			symptom:  fmt.Sprintf("stationary host %s cannot prove whether management and archive hardware share a removable failure domain", target),
			baseline: "The configured archive device and active management uplink both resolve to bounded local ancestry, producing shared-removable or separate.",
			observed: "topology_state=unobservable",
			evidence: "No raw device or topology identifier is exported.",
			action:   "Restore the privacy-reduced Xops hardware observer and archive-device visibility. If the archive is intentionally locked during recovery, retain §11.22 as the owning outage and re-evaluate topology before enabling writers.",
			verify:   "The observer returns a concrete shared-removable or separate class without exporting hardware identity.",
			playbook: "SIGNALS.md §21.2 and §11.22",
		})
	default:
		findings = append(findings, healthyFinding(probeId, tierWarn, "hostpower-shared-removable-domain", target))
		findings = append(findings, healthyFinding(probeId, tierWarn, "hostpower-topology-unobservable", target))
	}

	switch sample.mediaHealth {
	case "failed":
		findings = append(findings, finding{
			probeId: probeId, tier: tierPage, class: "hostpower-media-health-failed", target: target, sustain: 1,
			symptom:  fmt.Sprintf("archive media on %s reports a failed SMART health state", target),
			baseline: "The archive disk reports passing overall health and exposes the detailed error and self-test logs needed to support that classification.",
			observed: "media_health=failed",
			evidence: "SMART output, device identity, and serial remain on-host; only the bounded failed class is returned.",
			action:   "Keep archive writers stopped, preserve recovery evidence, and replace or recover the media under §11.22 with an authorized operator. Do not clear or overwrite SMART history.",
			verify:   "An independently identified replacement/recovered device passes offline filesystem recovery and exposes passing detailed SMART evidence before one controlled archive run.",
			playbook: "SIGNALS.md §21.2 and §11.22",
		})
		findings = append(findings, healthyFinding(probeId, tierWarn, "hostpower-media-health-unobservable", target))
	case "unobservable":
		findings = append(findings, healthyFinding(probeId, tierPage, "hostpower-media-health-failed", target))
		findings = append(findings, finding{
			probeId: probeId, tier: tierWarn, class: "hostpower-media-health-unobservable", target: target, sustain: 2,
			symptom:   fmt.Sprintf("detailed archive media health is unobservable on %s", target),
			mechanism: "The storage bridge or device does not expose both the error log and self-test log needed to classify media history. A bridge-level SCSI Health=OK result alone is not evidence of healthy media.",
			baseline:  "The archive device exposes an overall passing state plus readable detailed error and self-test logs.",
			observed:  "media_health=unobservable",
			evidence:  "Raw SMART output and device identity remain on-host. Unsupported or failed mandatory SMART commands are UNKNOWN, never healthy.",
			context:   "Filesystem recovery and archive completeness remain independently owned by §11.22; lack of detailed SMART support neither proves nor disproves media damage.",
			action:    "Keep the uncertainty explicit. Prefer an enclosure/bridge that exposes full SMART logs, preserve offline filesystem evidence, and do not resume writers solely because a bridge-level summary says OK.",
			verify:    "Detailed error and self-test logs become readable and passing, or the operational record retains this visibility limitation while §11.22 proves every recovery invariant independently.",
			playbook:  "SIGNALS.md §21.2 and §11.22",
		})
	default:
		findings = append(findings, healthyFinding(probeId, tierPage, "hostpower-media-health-failed", target))
		findings = append(findings, healthyFinding(probeId, tierWarn, "hostpower-media-health-unobservable", target))
	}
	return findings
}
