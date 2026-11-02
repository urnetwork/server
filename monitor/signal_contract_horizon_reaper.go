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

// Signal contract-horizon-reaper implements SIGNALS.md §2.4r. The contract
// horizon reaper (model.RemoveContractsPastHorizon) runs as one taskworker
// chain, one run at a time on whichever process claims it. It exports its runs
// by why they stopped, the contracts it kept by reason, its mode, and how far
// its pass is behind its horizon. The signal reads them in one Mimir
// evaluation and warns when the chain stops running, when its runs end in
// errors or on an unreadable control with none making progress, when its pass
// falls behind its horizon while it deletes, and when operator block points
// keep contracts past the horizon. The gauges are read only from the processes
// that ran the reaper in the window, since a process that ran it once keeps
// its last values. No contract or process identifier leaves Mimir.
func NewContractHorizonReaperSignal() Signal {
	return NewContractHorizonReaperSignalWithSettings(DefaultContractHorizonReaperSettings())
}

// The signal with explicit thresholds.
func NewContractHorizonReaperSignalWithSettings(settings *ContractHorizonReaperSettings) Signal {
	return &signalAdapter{
		number: "2.4r", key: "contract-horizon-reaper", name: "Contract horizon reaper",
		probe: contractHorizonReaperProbe{settings: settings},
	}
}

const contractHorizonReaperProbeId = "mimir/contract-horizon-reaper"

// The thresholds of SIGNALS.md §2.4r.
type ContractHorizonReaperSettings struct {
	Cadence time.Duration
	// The window the run and kept counts are read over. The chain runs every
	// 15 seconds while it has work and every 5 minutes otherwise, so an hour
	// holds at least 12 runs.
	Range time.Duration
	// Runs in the range that end in errors, or on an unreadable control, with
	// none reaching the horizon or the run budget.
	StuckRuns float64
	// The lag now is compared with the lag this long ago, inside one pass,
	// and may grow by at most LagGrowth.
	LagWindow time.Duration
	LagGrowth time.Duration
	// A sample older than this is stale.
	Freshness time.Duration
}

// The defaults of SIGNALS.md §2.4r.
func DefaultContractHorizonReaperSettings() *ContractHorizonReaperSettings {
	return &ContractHorizonReaperSettings{
		Cadence:   5 * time.Minute,
		Range:     time.Hour,
		StuckRuns: 3,
		LagWindow: 6 * time.Hour,
		LagGrowth: 2 * time.Hour,
		Freshness: 10 * time.Minute,
	}
}

func (self *ContractHorizonReaperSettings) valid() bool {
	return self != nil && time.Minute <= self.Cadence && time.Minute <= self.Range && self.Range%time.Second == 0 &&
		0 < self.StuckRuns && time.Hour <= self.LagWindow && self.LagWindow%time.Second == 0 &&
		0 < self.LagGrowth && self.LagGrowth < self.LagWindow && 0 < self.Freshness
}

type contractHorizonReaperProbe struct {
	settings *ContractHorizonReaperSettings
}

func (contractHorizonReaperProbe) id() string   { return contractHorizonReaperProbeId }
func (contractHorizonReaperProbe) tier() string { return tierWarn }

func (self contractHorizonReaperProbe) cadence() time.Duration {
	return self.effectiveSettings().Cadence
}

func (self contractHorizonReaperProbe) effectiveSettings() *ContractHorizonReaperSettings {
	if self.settings == nil {
		return DefaultContractHorizonReaperSettings()
	}
	return self.settings
}

// The reaper's mode gauge when it deletes (model.ContractHorizonModeOn).
const contractHorizonReaperModeOn = 2

// The parts of the evaluation, each a single series named by monitor_metric.
var contractHorizonReaperClasses = []string{
	"runs", "error_runs", "control_runs", "progress_runs", "block_points",
	"mode", "lag", "lag_before", "pass_start", "pass_start_before",
}

// One evaluation: the run counts by stop and the block points kept over the
// range, and the mode, lag and pass start of the processes that ran the
// reaper, now and a lag window ago.
func contractHorizonReaperQuery(environment string, settings *ContractHorizonReaperSettings) string {
	env := strconv.Quote(environment)
	window := stormDuration(settings.Range)
	runs := func(stop string) string {
		matcher := "env=" + env + stop
		return fmt.Sprintf("sum(increase(urnetwork_contract_horizon_runs_total{%s}[%s]))", matcher, window)
	}
	// a gauge of the processes that ran the reaper in the range ending at offset
	ran := func(metric string, offset string) string {
		return fmt.Sprintf("max(%s{env=%s}%s and on(host,block,instance) (sum by (host,block,instance) (increase(urnetwork_contract_horizon_runs_total{env=%s}[%s]%s)) > 0))",
			metric, env, offset, env, window, offset)
	}
	part := func(class string, expression string) string {
		return fmt.Sprintf(`label_replace(%s,"monitor_metric",%s,"__name__",".*")`, expression, strconv.Quote(class))
	}
	before := " offset " + stormDuration(settings.LagWindow)
	return strings.Join([]string{
		part("runs", runs("")),
		part("error_runs", runs(`,stop="error"`)),
		part("control_runs", runs(`,stop="control_unavailable"`)),
		part("progress_runs", runs(`,stop=~"horizon|budget"`)),
		part("block_points", fmt.Sprintf(`sum(increase(urnetwork_contract_horizon_protected_contracts_total{env=%s,reason="block_points"}[%s]))`, env, window)),
		part("mode", ran("urnetwork_contract_horizon_mode", "")),
		part("lag", ran("urnetwork_contract_horizon_lag_seconds", "")),
		part("lag_before", ran("urnetwork_contract_horizon_lag_seconds", before)),
		part("pass_start", ran("urnetwork_contract_horizon_pass_start_timestamp_seconds", "")),
		part("pass_start_before", ran("urnetwork_contract_horizon_pass_start_timestamp_seconds", before)),
	}, " or ")
}

// One part's value, authoritative only when exactly one fresh, valid sample
// exists.
type contractHorizonReaperValue struct {
	value float64
	seen  bool
	valid bool
}

func (self contractHorizonReaperValue) observed(format string) string {
	if !self.valid {
		return "unknown"
	}
	return fmt.Sprintf(format, self.value)
}

// The evaluation's parts by class, and why it cannot be read whole.
type contractHorizonReaperFrame struct {
	values  map[string]*contractHorizonReaperValue
	reasons []string
}

func (self contractHorizonReaperFrame) get(class string) contractHorizonReaperValue {
	if value, ok := self.values[class]; ok {
		return *value
	}
	return contractHorizonReaperValue{}
}

// Groups the evaluation by its class. A missing, duplicate, stale or invalid
// part makes the read unknown without discarding the parts it does prove.
func parseContractHorizonReaperFrame(response mimirInstantResponse, now time.Time, settings *ContractHorizonReaperSettings) contractHorizonReaperFrame {
	frame := contractHorizonReaperFrame{values: map[string]*contractHorizonReaperValue{}}
	for _, class := range contractHorizonReaperClasses {
		frame.values[class] = &contractHorizonReaperValue{}
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
	for _, class := range []string{"runs", "error_runs", "control_runs", "progress_runs", "block_points"} {
		if !frame.values[class].seen {
			reasonSet["missing_"+class] = true
		}
	}
	// a process that ran the reaper exports its mode, lag and pass start
	if runs := frame.get("runs"); runs.valid && 0 < math.Round(runs.value) {
		for _, class := range []string{"mode", "lag", "pass_start"} {
			if !frame.values[class].seen {
				reasonSet["missing_"+class] = true
			}
		}
	}
	for reason := range reasonSet {
		frame.reasons = append(frame.reasons, reason)
	}
	sort.Strings(frame.reasons)
	return frame
}

func (self contractHorizonReaperProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.effectiveSettings()
	target := env.cfg.env + "/contract-horizon-reaper"
	if !settings.valid() {
		return []finding{contractHorizonReaperUnobservable(target, "invalid_policy", "none")}, nil
	}
	hosts := env.cfg.hostsWithRole("services")
	if len(hosts) == 0 {
		return nil, fmt.Errorf("contract horizon reaper: no services host in inventory for the loopback Mimir query")
	}
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query?query=" + url.QueryEscape(contractHorizonReaperQuery(env.cfg.env, settings))
	output, gateway, err := mimirShellFirstServiceGateway(ctx, env.runner, hosts, nil,
		"curl -fsS --max-time 15 --max-filesize 4194304 '"+queryURL+"'", mimirShellResponseMaxBytes)
	if err != nil {
		if lifecycleErr := probeLifecycleError(ctx, err); lifecycleErr != nil {
			return nil, lifecycleErr
		}
		return nil, fmt.Errorf("contract horizon reaper: query Mimir: %w", err)
	}
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("contract horizon reaper: decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" {
		return nil, fmt.Errorf("contract horizon reaper: Mimir response was not a successful vector")
	}
	frame := parseContractHorizonReaperFrame(response, env.now().UTC(), settings)
	return contractHorizonReaperFindings(frame, settings, target, gateway.name), nil
}

// A read that cannot establish the reaper's state.
func contractHorizonReaperUnobservable(target string, reasons string, gateway string) finding {
	return finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-reaper-unobservable",
		target: target, sustain: 2,
		symptom:   "The contract horizon reaper's metrics are missing, partial, stale or ambiguous.",
		mechanism: "Every taskworker registers the reaper's counters at zero, so their absence means no taskworker exports them: a build before the reaper, a scrape gap, or a query that no longer matches. A process that ran the reaper in the window must also export its mode, lag and pass start. Missing evidence is not a measured zero.",
		baseline:  "One fresh series for each run count by stop and for the block points kept, and, when the reaper ran, for its mode, lag and pass start.",
		observed:  "visibility_reason=" + reasons + " metrics_gateway=" + gateway,
		evidence:  "Only fixed structural reasons are exported; raw samples are discarded.",
		context:   "The reaper ships off (mode off until an operator sets {contract_horizon}control), and it still runs and counts its off stops then.",
		action:    "Check that the taskworker build carries the reaper (urnetwork_contract_horizon_runs_total), the scrape of the taskworker blocks, and the query shape. Until it is read, follow the reaper's run log line ([contract_horizon]run, at V(1)) on the taskworker.",
		verify:    "Every part reads fresh for two cadences.",
		playbook:  "SIGNALS.md §2.4r",
	}
}

func contractHorizonReaperFindings(frame contractHorizonReaperFrame, settings *ContractHorizonReaperSettings, target string, gateway string) []finding {
	findings := []finding{}
	complete := len(frame.reasons) == 0
	if complete {
		findings = append(findings, healthyFinding(contractHorizonReaperProbeId, tierWarn, "contract-horizon-reaper-unobservable", target))
	} else {
		findings = append(findings, contractHorizonReaperUnobservable(target, strings.Join(frame.reasons, ","), gateway))
	}
	window := stormDuration(settings.Range)
	runs := frame.get("runs")
	errorRuns := frame.get("error_runs")
	controlRuns := frame.get("control_runs")
	progressRuns := frame.get("progress_runs")
	blockPoints := frame.get("block_points")
	mode := frame.get("mode")
	lag := frame.get("lag")
	lagBefore := frame.get("lag_before")
	passStart := frame.get("pass_start")
	passStartBefore := frame.get("pass_start_before")
	counts := fmt.Sprintf("runs_%[1]s=%[2]s error_runs_%[1]s=%[3]s control_unavailable_runs_%[1]s=%[4]s progress_runs_%[1]s=%[5]s mode=%[6]s lag_hours=%[7]s metrics_gateway=%[8]s",
		window, runs.observed("%.0f"), errorRuns.observed("%.0f"), controlRuns.observed("%.0f"), progressRuns.observed("%.0f"),
		mode.observed("%.0f"), contractHorizonReaperHours(lag), gateway)
	// a proven positive needs only its own parts; a clear needs the whole read
	raise := func(proven bool, healthy bool, class string, raised finding) {
		switch {
		case proven:
			findings = append(findings, raised)
		case healthy && complete:
			findings = append(findings, healthyFinding(contractHorizonReaperProbeId, tierWarn, class, target))
		}
	}

	raise(runs.valid && math.Round(runs.value) == 0, runs.valid, "contract-horizon-reaper-stalled", finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-reaper-stalled", target: target, sustain: 2,
		symptom:   fmt.Sprintf("No contract horizon reaper run ended in %s", window),
		mechanism: "The reaper is one taskworker chain (remove_contracts_past_horizon): each run hands its cursor to the next through its post, every 15 seconds while it has work and every 5 minutes otherwise, even with the mode off. No run in an hour means the chain is lost, its task never claimed, or no taskworker runs it, so contract rows stop leaving and edge-2's disk grows by the day's contracts.",
		baseline:  "At least 12 runs an hour, whatever the mode.",
		observed:  counts,
		evidence:  "sum(increase(urnetwork_contract_horizon_runs_total[1h])) over every taskworker process.",
		context:   "A taskworker deploy can pause the chain for one claim; a whole hour cannot.",
		action:    "Check pending_task for run_once_key remove_contracts_past_horizon and the taskworker's task health (§12). A missing chain is rescheduled by ScheduleRemoveContractsPastHorizon at taskworker start.",
		verify:    "Runs end again for two cadences.",
		playbook:  "SIGNALS.md §2.4r",
	})

	noProgress := progressRuns.valid && math.Round(progressRuns.value) == 0
	raise(errorRuns.valid && settings.StuckRuns <= math.Round(errorRuns.value) && noProgress, errorRuns.valid && progressRuns.valid, "contract-horizon-reaper-failing", finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-reaper-failing", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%s contract horizon reaper runs failed in %s and none reached the horizon or its budget", errorRuns.observed("%.0f"), window),
		mechanism: "A run that stops with an error rolls back its batch in flight and hands its last committed cursor to the task's retry, so it loses nothing, but a batch that fails every time stops the pass where it is: a statement or lock timeout on a slow table, a lock it cannot get, a schema the build does not expect, or the earning policy or block points ledger unreadable.",
		baseline:  fmt.Sprintf("Errors are rare and some run reaches the horizon or its run budget in every %s.", window),
		observed:  counts,
		evidence:  "increase(urnetwork_contract_horizon_runs_total{stop=\"error\"}) against stop horizon and budget, over every taskworker process.",
		context:   "The reaper takes its locks with SKIP LOCKED and bounds each statement by its settings; a failing batch repeats on the same ids because the cursor only moves on commit.",
		action:    "Read the taskworker's remove_contracts_past_horizon errors (the task's reschedule_error and its log) for the failing statement. Lower the control's batchSize or maxRowsPerSecond, or set mode off, while it is fixed. Never delete contract rows by hand.",
		verify:    "A run reaches the horizon or its budget within two cadences.",
		playbook:  "SIGNALS.md §2.4r",
	})

	raise(controlRuns.valid && settings.StuckRuns <= math.Round(controlRuns.value) && noProgress, controlRuns.valid && progressRuns.valid, "contract-horizon-control-unavailable", finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-control-unavailable", target: target, sustain: 2,
		symptom:   fmt.Sprintf("%s contract horizon reaper runs could not read their control in %s and none made progress", controlRuns.observed("%.0f"), window),
		mechanism: "Each batch first reads the Redis hash {contract_horizon}control with one bounded read. A run that cannot read it stops as if the mode were off, so an unreachable or slow slot holding that key stops the reaper entirely.",
		baseline:  "The control reads every time; a failed read is rare.",
		observed:  counts,
		evidence:  "increase(urnetwork_contract_horizon_runs_total{stop=\"control_unavailable\"}) against stop horizon and budget.",
		context:   "The control is a singleton on the {contract_horizon} hash slot; read the Redis cluster entries (§3) for the node that owns it.",
		action:    "Restore the Redis node or slot that owns {contract_horizon}control. The reaper resumes at its stored cursor on its own.",
		verify:    "Runs read the control again for two cadences.",
		playbook:  "SIGNALS.md §2.4r and §3",
	})

	samePass := passStart.valid && passStartBefore.valid && passStart.value == passStartBefore.value
	growth := lag.value - lagBefore.value
	behind := mode.valid && mode.value == contractHorizonReaperModeOn && lag.valid && lagBefore.valid && samePass &&
		settings.LagGrowth.Seconds() <= growth
	behindKnown := mode.valid && (mode.value != contractHorizonReaperModeOn || (lag.valid && lagBefore.valid && passStart.valid && passStartBefore.valid))
	raise(behind, behindKnown, "contract-horizon-reaper-behind", finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-reaper-behind", target: target, sustain: 2,
		symptom:   fmt.Sprintf("The contract horizon reaper's pass fell %.1f hours further behind its horizon in %s while on", growth/3600, stormDuration(settings.LagWindow)),
		mechanism: "The lag is the time between the horizon and the mint time of the last contract the pass committed past. It grows when the pass deletes contracts more slowly than they cross the horizon, so the contract tables grow by the difference; a pass that keeps up holds it near zero, and one catching up shrinks it.",
		baseline:  fmt.Sprintf("Inside one pass the lag grows by less than %s in %s while the mode is on.", stormDuration(settings.LagGrowth), stormDuration(settings.LagWindow)),
		observed:  counts + " lag_before_hours=" + contractHorizonReaperHours(lagBefore),
		evidence:  "max(urnetwork_contract_horizon_lag_seconds) of the processes that ran the reaper, now and a lag window ago, with an unchanged urnetwork_contract_horizon_pass_start_timestamp_seconds.",
		context:   "The reaper reads at most maxRowsPerSecond contracts a second (200 by default, 2000 at most); at about 85M new contracts a day it needs about 985 a second to hold any horizon. A rescan restarts the pass, which this check skips. A dry run never advances its cursor and is not judged.",
		action:    "Raise the control's maxRowsPerSecond toward the code's 2000 while edge-2's I/O, WAL and vacuum allow, and read §2.4b for the disk. A lower horizonDays makes more contracts eligible, not fewer.",
		verify:    "The lag stops growing for two cadences.",
		playbook:  "SIGNALS.md §2.4r and §2.4b",
	})

	raise(blockPoints.valid && 0 < math.Round(blockPoints.value), blockPoints.valid, "contract-horizon-block-points-holding", finding{
		probeId: contractHorizonReaperProbeId, tier: tierWarn, class: "contract-horizon-block-points-holding", target: target, sustain: 1,
		symptom:   fmt.Sprintf("The contract horizon reaper kept %s contracts past its horizon in %s for operator block points", blockPoints.observed("%.0f"), window),
		mechanism: "An operator block award reads the unpaid sweeps whose sweep time, the settlement, falls in its block's window, until the block is awarded or its reliability is past retention, and the reaper keeps every such contract (block_points). A contract past the horizon with a sweep in a pending block settled long after it closed, or its block's award is overdue.",
		baseline:  "Zero: the daily task awards each block about two hours after it closes, and late settlement is rare.",
		observed:  fmt.Sprintf("block_points_%s=%s %s", window, blockPoints.observed("%.0f"), counts),
		evidence:  "increase(urnetwork_contract_horizon_protected_contracts_total{reason=\"block_points\"}); each pass that reads a kept contract counts it again.",
		context:   "A block is awarded by controller.AwardOperatorBlockPoints daily at 12:00 UTC and by bringyourctl account-points backfill-blocks. The rule keeps a contract at most 21 days and 13 hours after its block opened.",
		action:    "Check payment_report for the plan ids of the recent blocks (OperatorBlockPointsPlanId) and the award task's errors. An award that cannot run is the fix; never delete the kept contracts.",
		verify:    "No block points keep in a further window, or the blocks are awarded.",
		playbook:  "SIGNALS.md §2.4r",
	})
	return findings
}

func contractHorizonReaperHours(value contractHorizonReaperValue) string {
	if !value.valid {
		return "unknown"
	}
	return fmt.Sprintf("%.1f", value.value/3600)
}
