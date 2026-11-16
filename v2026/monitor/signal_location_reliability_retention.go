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

// Signal location-reliability-retention implements SIGNALS.md §2.4u.
// The client location reliability retention
// (model.RemoveOldClientLocationReliabilities; owner 2026-11-16, "11 days
// except top-level") runs as one taskworker chain: each run trims every row
// past 31 days, then walks the rows past 11 days from a cursor and deletes
// those of children and gone clients. Each run counts one walk run by why it
// ended. The signal reads the counts in one Mimir evaluation and warns when
// the chain stops running, and when the walk's runs end in errors with none
// making progress. Missing evidence is unobservable, never a measured zero.
// No row or process identifier leaves Mimir.
func NewClientLocationReliabilityRetentionSignal() Signal {
	return NewClientLocationReliabilityRetentionSignalWithSettings(DefaultClientLocationReliabilityRetentionSignalSettings())
}

// The signal with explicit thresholds.
func NewClientLocationReliabilityRetentionSignalWithSettings(settings *ClientLocationReliabilityRetentionSignalSettings) Signal {
	return &signalAdapter{
		number: "2.4u", key: "location-reliability-retention", name: "Client location reliability retention",
		probe: clientLocationReliabilityRetentionProbe{settings: settings},
	}
}

const clientLocationReliabilityRetentionProbeId = "mimir/location-reliability-retention"

// The thresholds of SIGNALS.md §2.4u.
type ClientLocationReliabilityRetentionSignalSettings struct {
	Cadence time.Duration
	// The window the run counts are read over. The chain runs every 15
	// seconds while it has work and every 15 minutes otherwise, each run
	// counting one walk run, so two hours hold at least 8 runs.
	Range time.Duration
	// The walk's runs in the range that end in errors, with none catching up
	// or spending its budget.
	StuckRuns float64
	// A sample older than this is stale.
	Freshness time.Duration
}

// The defaults of SIGNALS.md §2.4u.
func DefaultClientLocationReliabilityRetentionSignalSettings() *ClientLocationReliabilityRetentionSignalSettings {
	return &ClientLocationReliabilityRetentionSignalSettings{
		Cadence:   5 * time.Minute,
		Range:     2 * time.Hour,
		StuckRuns: 3,
		Freshness: 10 * time.Minute,
	}
}

func (self *ClientLocationReliabilityRetentionSignalSettings) valid() bool {
	return self != nil && time.Minute <= self.Cadence && time.Minute <= self.Range && self.Range%time.Second == 0 &&
		0 < self.StuckRuns && 0 < self.Freshness
}

type clientLocationReliabilityRetentionProbe struct {
	settings *ClientLocationReliabilityRetentionSignalSettings
}

func (clientLocationReliabilityRetentionProbe) id() string {
	return clientLocationReliabilityRetentionProbeId
}
func (clientLocationReliabilityRetentionProbe) tier() string { return tierWarn }

func (self clientLocationReliabilityRetentionProbe) cadence() time.Duration {
	return self.effectiveSettings().Cadence
}

func (self clientLocationReliabilityRetentionProbe) effectiveSettings() *ClientLocationReliabilityRetentionSignalSettings {
	if self.settings == nil {
		return DefaultClientLocationReliabilityRetentionSignalSettings()
	}
	return self.settings
}

// The parts of the evaluation, each a single series named by monitor_metric:
// all runs, those that ended in errors and those that made progress.
var clientLocationReliabilityRetentionClasses = []string{"runs", "error_runs", "progress_runs"}

// One evaluation: the walk's run counts over the range, all of them, those
// that ended in errors and those that made progress.
func clientLocationReliabilityRetentionQuery(environment string, settings *ClientLocationReliabilityRetentionSignalSettings) string {
	env := strconv.Quote(environment)
	window := stormDuration(settings.Range)
	runs := func(matcher string) string {
		return fmt.Sprintf("sum(increase(urnetwork_client_location_reliability_retention_runs_total{env=%s%s}[%s]))", env, matcher, window)
	}
	part := func(class string, expression string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote(class))
	}
	return strings.Join([]string{
		part("runs", runs("")),
		part("error_runs", runs(`,stop="error"`)),
		part("progress_runs", runs(`,stop=~"caught_up|budget"`)),
	}, " or ")
}

// One part's value, authoritative only when exactly one fresh, valid sample
// exists.
type clientLocationReliabilityRetentionValue struct {
	value float64
	seen  bool
	valid bool
}

func (self clientLocationReliabilityRetentionValue) observed() string {
	if !self.valid {
		return "unknown"
	}
	return fmt.Sprintf("%.0f", self.value)
}

// The evaluation's parts by class, and why it cannot be read whole.
type clientLocationReliabilityRetentionFrame struct {
	values  map[string]*clientLocationReliabilityRetentionValue
	reasons []string
}

func (self clientLocationReliabilityRetentionFrame) get(class string) clientLocationReliabilityRetentionValue {
	if value, ok := self.values[class]; ok {
		return *value
	}
	return clientLocationReliabilityRetentionValue{}
}

// Groups the evaluation by its class. A missing, duplicate, stale or invalid
// part makes the read unknown without discarding the parts it does prove.
func parseClientLocationReliabilityRetentionFrame(response mimirInstantResponse, now time.Time, settings *ClientLocationReliabilityRetentionSignalSettings) clientLocationReliabilityRetentionFrame {
	frame := clientLocationReliabilityRetentionFrame{values: map[string]*clientLocationReliabilityRetentionValue{}}
	for _, class := range clientLocationReliabilityRetentionClasses {
		frame.values[class] = &clientLocationReliabilityRetentionValue{}
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
	for _, class := range clientLocationReliabilityRetentionClasses {
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

func (self clientLocationReliabilityRetentionProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/location-reliability-retention"
	if !settings.valid() {
		return []finding{clientLocationReliabilityRetentionUnobservable(target, "invalid_policy", "none")}, nil
	}
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("client location reliability retention: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(clientLocationReliabilityRetentionQuery(env.cfg.env, settings))
	output, gateway, err := mimirShellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'", mimirShellResponseMaxBytes)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return nil, fmt.Errorf("client location reliability retention: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("client location reliability retention: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("client location reliability retention: Mimir response was not a successful vector")
	}
	frame := parseClientLocationReliabilityRetentionFrame(response, env.now().UTC(), settings)
	return clientLocationReliabilityRetentionFindings(frame, settings, target, gateway.name), nil
}

// A read that cannot establish the retention's state.
func clientLocationReliabilityRetentionUnobservable(target string, reasons string, gateway string) finding {
	return finding{
		probeId: clientLocationReliabilityRetentionProbeId, tier: tierWarn, class: "location-reliability-retention-unobservable",
		target: target, sustain: 2,
		symptom:   "The client location reliability retention's run counts are missing, partial, stale or ambiguous.",
		mechanism: "Every taskworker registers the walk's run counter at zero for each stop, so its absence means no taskworker exports it: a build before the retention's walk, a scrape gap, or a query that no longer matches. Missing evidence is not a measured zero.",
		baseline:  "One fresh series for all runs, the error runs and the progress runs.",
		observed:  "visibility_reason=" + reasons + " metrics_gateway=" + gateway,
		evidence:  "Only fixed structural reasons are exported; raw samples are discarded.",
		context:   "The walk runs by default; its brake {client_location_reliability_retention}control can stop it, and it still counts brake_off runs then.",
		action:    "Check that the taskworker build carries the walk (urnetwork_client_location_reliability_retention_runs_total), the scrape of the taskworker blocks, and the query shape. Until it is read, follow the retention's run log line ([nclr]trimmed) on the taskworker.",
		verify:    "Every part reads fresh for two cadences.",
		playbook:  "SIGNALS.md §2.4u",
	}
}

func clientLocationReliabilityRetentionFindings(frame clientLocationReliabilityRetentionFrame, settings *ClientLocationReliabilityRetentionSignalSettings, target string, gateway string) []finding {
	findings := []finding{}
	complete := len(frame.reasons) == 0
	if complete {
		findings = append(findings, healthyFinding(clientLocationReliabilityRetentionProbeId, tierWarn, "location-reliability-retention-unobservable", target))
	} else {
		findings = append(findings, clientLocationReliabilityRetentionUnobservable(target, strings.Join(frame.reasons, ","), gateway))
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
			findings = append(findings, healthyFinding(clientLocationReliabilityRetentionProbeId, tierWarn, class, target))
		}
	}

	raise(runs.valid && math.Round(runs.value) == 0, runs.valid, "location-reliability-retention-stalled", finding{
		probeId: clientLocationReliabilityRetentionProbeId, tier: tierWarn, class: "location-reliability-retention-stalled", target: target, sustain: 2,
		symptom:   fmt.Sprintf("No client location reliability retention run ended in %s", window),
		mechanism: "The retention is one taskworker chain (remove_old_client_location_reliabilities): each run trims the rows past 31 days, walks the rows past 11 days, and hands the walk's cursor to the next run through its post, every 15 seconds while it has work and every 15 minutes otherwise, even with its brake off. No run in two hours means the chain is lost, its task never claimed, or no taskworker runs it, so network_client_location_reliability grows again.",
		baseline:  "At least 8 runs in two hours, whatever the brake.",
		observed:  counts,
		evidence:  "sum(increase(urnetwork_client_location_reliability_retention_runs_total[2h])) over every taskworker process.",
		context:   "A taskworker deploy can pause the chain for one claim; two hours cannot.",
		action:    "Check pending_task for run_once_key remove_old_client_location_reliabilities and the taskworker's task health (§2.5). A missing chain is rescheduled by ScheduleRemoveOldClientLocationReliabilities at taskworker start.",
		verify:    "Runs end again for two cadences.",
		playbook:  "SIGNALS.md §2.4u",
	})

	raise(errorRuns.valid && progressRuns.valid && settings.StuckRuns <= math.Round(errorRuns.value) && math.Round(progressRuns.value) == 0,
		errorRuns.valid && progressRuns.valid, "location-reliability-retention-failing", finding{
			probeId: clientLocationReliabilityRetentionProbeId, tier: tierWarn, class: "location-reliability-retention-failing", target: target, sustain: 2,
			symptom:   fmt.Sprintf("The client location reliability retention's walk failed in %s and no run made progress", window),
			mechanism: "A walk batch that fails rolls back and ends the run with the cursor of the last committed batch, so it loses nothing, but a batch that fails every time stops the walk where it is: a lock wait past the batch's bound or a schema the build does not expect. The trim at 31 days runs before the walk in every run, so rows still go, 20 days later than they should.",
			baseline:  fmt.Sprintf("Errors are rare and some walk run catches up or spends its budget in every %s.", window),
			observed:  counts,
			evidence:  "increase(urnetwork_client_location_reliability_retention_runs_total{stop=\"error\"}) against stop caught_up and budget, over every taskworker process.",
			context:   "Each batch skips rows another transaction holds and bounds its lock waits.",
			action:    "Read the taskworker's [nclr] errors for the failing statement. Lower the brake's maxRowsPerSecond, or set mode off, while it is fixed. Never delete the rows by hand outside the retention.",
			verify:    "A walk run makes progress within two cadences.",
			playbook:  "SIGNALS.md §2.4u",
		})
	return findings
}
