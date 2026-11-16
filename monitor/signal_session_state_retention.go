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

// Signal session-state-retention implements SIGNALS.md §2.4t. The session
// state retention (model.RemoveExpiredSessionState; owner 2026-11-16) runs as
// one taskworker chain that deletes provider work session heads whose clients
// have no event left in the journal and device audit state offline for a day.
// Each run counts one run per table by why it ended. The signal reads the
// counts in one Mimir evaluation and warns when the chain stops running, and
// when its runs end in errors with none making progress. Missing evidence is
// unobservable, never a measured zero. No row or process identifier leaves
// Mimir.
func NewSessionStateRetentionSignal() Signal {
	return NewSessionStateRetentionSignalWithSettings(DefaultSessionStateRetentionSignalSettings())
}

// The signal with explicit thresholds.
func NewSessionStateRetentionSignalWithSettings(settings *SessionStateRetentionSignalSettings) Signal {
	return &signalAdapter{
		number: "2.4t", key: "session-state-retention", name: "Session state retention",
		probe: sessionStateRetentionProbe{settings: settings},
	}
}

const sessionStateRetentionProbeId = "mimir/session-state-retention"

// The thresholds of SIGNALS.md §2.4t.
type SessionStateRetentionSignalSettings struct {
	Cadence time.Duration
	// The window the run counts are read over. The chain runs every 15
	// seconds while it has work and every 15 minutes otherwise, each run
	// counting one run for each of the two tables, so two hours hold at least
	// 16 runs.
	Range time.Duration
	// Runs in the range that end in errors, with none reaching the end of a
	// pass, the run budget or a pass not yet due.
	StuckRuns float64
	// A sample older than this is stale.
	Freshness time.Duration
}

// The defaults of SIGNALS.md §2.4t.
func DefaultSessionStateRetentionSignalSettings() *SessionStateRetentionSignalSettings {
	return &SessionStateRetentionSignalSettings{
		Cadence:   5 * time.Minute,
		Range:     2 * time.Hour,
		StuckRuns: 3,
		Freshness: 10 * time.Minute,
	}
}

func (self *SessionStateRetentionSignalSettings) valid() bool {
	return self != nil && time.Minute <= self.Cadence && time.Minute <= self.Range && self.Range%time.Second == 0 &&
		0 < self.StuckRuns && 0 < self.Freshness
}

type sessionStateRetentionProbe struct {
	settings *SessionStateRetentionSignalSettings
}

func (sessionStateRetentionProbe) id() string   { return sessionStateRetentionProbeId }
func (sessionStateRetentionProbe) tier() string { return tierWarn }

func (self sessionStateRetentionProbe) cadence() time.Duration {
	return self.effectiveSettings().Cadence
}

func (self sessionStateRetentionProbe) effectiveSettings() *SessionStateRetentionSignalSettings {
	if self.settings == nil {
		return DefaultSessionStateRetentionSignalSettings()
	}
	return self.settings
}

// The parts of the evaluation, each a single series named by monitor_metric.
var sessionStateRetentionClasses = []string{"runs", "error_runs", "progress_runs"}

// One evaluation: the run counts over the range, all of them, those that
// ended in errors and those that made progress.
func sessionStateRetentionQuery(environment string, settings *SessionStateRetentionSignalSettings) string {
	env := strconv.Quote(environment)
	window := stormDuration(settings.Range)
	runs := func(stop string) string {
		return fmt.Sprintf("sum(increase(urnetwork_session_state_retention_runs_total{env=%s%s}[%s]))", env, stop, window)
	}
	part := func(class string, expression string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote(class))
	}
	return strings.Join([]string{
		part("runs", runs("")),
		part("error_runs", runs(`,stop="error"`)),
		part("progress_runs", runs(`,stop=~"pass|budget|waiting"`)),
	}, " or ")
}

// One part's value, authoritative only when exactly one fresh, valid sample
// exists.
type sessionStateRetentionValue struct {
	value float64
	seen  bool
	valid bool
}

func (self sessionStateRetentionValue) observed() string {
	if !self.valid {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", self.value)
}

// The evaluation's parts by class, and why it cannot be read whole.
type sessionStateRetentionFrame struct {
	values  map[string]*sessionStateRetentionValue
	reasons []string
}

func (self sessionStateRetentionFrame) get(class string) sessionStateRetentionValue {
	if value, ok := self.values[class]; ok {
		return *value
	}
	return sessionStateRetentionValue{}
}

// Groups the evaluation by its class. A missing, duplicate, stale or invalid
// part makes the read unknown without discarding the parts it does prove.
func parseSessionStateRetentionFrame(response mimirInstantResponse, now time.Time, settings *SessionStateRetentionSignalSettings) sessionStateRetentionFrame {
	frame := sessionStateRetentionFrame{values: map[string]*sessionStateRetentionValue{}}
	for _, class := range sessionStateRetentionClasses {
		frame.values[class] = &sessionStateRetentionValue{}
	}
	reasonSet := map[string]bool{}
	for _, series := range response.Data.Result {
		value, ok := frame.values[series.Metric["monitor_metric"]]
		if !ok {
			reasonSet["unknown_metric_class"] = true
			continue
		}
		duplicate := value.seen
		value.seen = true
		if duplicate {
			value.valid = false
			reasonSet["duplicate_metric"] = true
			continue
		}
		observedAt, sample, err := mimirInstantValue(series.Value)
		if err != nil || settings.Freshness < now.Sub(observedAt) || now.Sub(observedAt) < -30*time.Second ||
			math.IsNaN(sample) || math.IsInf(sample, 0) || sample < 0 {
			reasonSet["invalid_or_stale_metric"] = true
			continue
		}
		value.value, value.valid = sample, true
	}
	for _, class := range sessionStateRetentionClasses {
		if !frame.values[class].seen {
			reasonSet["missing_"+class] = true
		}
	}
	for reason := range reasonSet {
		frame.reasons = append(frame.reasons, reason)
	}
	sort.Strings(frame.reasons)
	return frame
}

func (self sessionStateRetentionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/session-state-retention"
	if !settings.valid() {
		return []finding{sessionStateRetentionUnobservable(target, "invalid_policy", "none")}, nil
	}
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("session state retention: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(sessionStateRetentionQuery(env.cfg.env, settings))
	output, gateway, err := mimirShellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'", mimirShellResponseMaxBytes)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return nil, fmt.Errorf("session state retention: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("session state retention: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("session state retention: Mimir response was not a successful vector")
	}
	frame := parseSessionStateRetentionFrame(response, env.now().UTC(), settings)
	return sessionStateRetentionFindings(frame, settings, target, gateway.name), nil
}

// A read that cannot establish the retention's state.
func sessionStateRetentionUnobservable(target string, reasons string, gateway string) finding {
	return finding{
		probeId: sessionStateRetentionProbeId, tier: tierWarn, class: "session-state-retention-unobservable",
		target: target, sustain: 2,
		symptom:   "The session state retention's run counts are missing, partial, stale or ambiguous.",
		mechanism: "Every taskworker registers the retention's run counter at zero for each table and stop, so its absence means no taskworker exports it: a build before the retention, a scrape gap, or a query that no longer matches. Missing evidence is not a measured zero.",
		baseline:  "One fresh series for all runs, error runs and progress runs.",
		observed:  "visibility_reason=" + reasons + " metrics_gateway=" + gateway,
		evidence:  "Only fixed structural reasons are exported; raw samples are discarded.",
		context:   "The retention runs by default; its brake {session_state_retention}control can stop it, and it still counts brake_off runs then.",
		action:    "Check that the taskworker build carries the retention (urnetwork_session_state_retention_runs_total), the scrape of the taskworker blocks, and the query shape. Until it is read, follow the retention's run log line ([ssr]removed) on the taskworker.",
		verify:    "Every part reads fresh for two cadences.",
		playbook:  "SIGNALS.md §2.4t",
	}
}

func sessionStateRetentionFindings(frame sessionStateRetentionFrame, settings *SessionStateRetentionSignalSettings, target string, gateway string) []finding {
	findings := []finding{}
	complete := len(frame.reasons) == 0
	if complete {
		findings = append(findings, healthyFinding(sessionStateRetentionProbeId, tierWarn, "session-state-retention-unobservable", target))
	} else {
		findings = append(findings, sessionStateRetentionUnobservable(target, strings.Join(frame.reasons, ","), gateway))
	}
	window := stormDuration(settings.Range)
	runs := frame.get("runs")
	errorRuns := frame.get("error_runs")
	progressRuns := frame.get("progress_runs")
	counts := fmt.Sprintf("runs_%[1]s=%[2]s error_runs_%[1]s=%[3]s progress_runs_%[1]s=%[4]s metrics_gateway=%[5]s",
		window, runs.observed(), errorRuns.observed(), progressRuns.observed(), gateway)
	// a proven positive needs only its own parts; a clear needs the whole read
	raise := func(proven bool, healthy bool, class string, raised finding) {
		switch {
		case proven:
			findings = append(findings, raised)
		case healthy && complete:
			findings = append(findings, healthyFinding(sessionStateRetentionProbeId, tierWarn, class, target))
		}
	}

	raise(runs.valid && math.Round(runs.value) == 0, runs.valid, "session-state-retention-stalled", finding{
		probeId: sessionStateRetentionProbeId, tier: tierWarn, class: "session-state-retention-stalled", target: target, sustain: 2,
		symptom:   fmt.Sprintf("No session state retention run ended in %s", window),
		mechanism: "The retention is one taskworker chain (remove_expired_session_state): each run hands both tables' cursors to the next through its post, every 15 seconds while it has work and every 15 minutes otherwise, even with its brake off. No run in two hours means the chain is lost, its task never claimed, or no taskworker runs it, so provider_work_session_head and audit_device_state grow again.",
		baseline:  "At least 16 runs in two hours, whatever the brake.",
		observed:  counts,
		evidence:  "sum(increase(urnetwork_session_state_retention_runs_total[2h])) over every taskworker process.",
		context:   "A taskworker deploy can pause the chain for one claim; two hours cannot.",
		action:    "Check pending_task for run_once_key remove_expired_session_state and the taskworker's task health (§12). A missing chain is rescheduled by ScheduleRemoveExpiredSessionState at taskworker start.",
		verify:    "Runs end again for two cadences.",
		playbook:  "SIGNALS.md §2.4t",
	})

	noProgress := progressRuns.valid && math.Round(progressRuns.value) == 0
	raise(errorRuns.valid && settings.StuckRuns <= math.Round(errorRuns.value) && noProgress, errorRuns.valid && progressRuns.valid, "session-state-retention-failing", finding{
		probeId: sessionStateRetentionProbeId, tier: tierWarn, class: "session-state-retention-failing", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%s session state retention runs failed in %s and none made progress", errorRuns.observed(), window),
		mechanism: "A batch that fails rolls back and ends its table's run with the cursor of the last committed batch, so it loses nothing, but a batch that fails every time stops the pass where it is: a lock wait past the batch's bound, a head guard that refuses the delete (a migration before the head retention guard), or a schema the build does not expect.",
		baseline:  fmt.Sprintf("Errors are rare and some run ends a pass, spends its budget or finds no pass due in every %s.", window),
		observed:  counts,
		evidence:  "increase(urnetwork_session_state_retention_runs_total{stop=\"error\"}) against stop pass, budget and waiting, over every taskworker process.",
		context:   "Each batch skips rows another transaction holds and bounds its lock waits; the head guard admits a delete only for a head with no event and no receipt left.",
		action:    "Read the taskworker's [ssr] errors for the failing table and statement, and check that Main's migration head includes the head retention guard. Lower the brake's maxHeadsPerSecond or maxDeviceStatesPerSecond, or set mode off, while it is fixed. Never delete heads by hand.",
		verify:    "A run makes progress within two cadences.",
		playbook:  "SIGNALS.md §2.4t",
	})
	return findings
}
