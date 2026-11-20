package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §2.32 maps to signal_zero_carry_verdicts.go and
// signal_zero_carry_verdicts_test.go. The Taskworker's zero-carry judge
// (JudgeProviderZeroCarry, server model/provider_zero_carry.go) takes a
// provider whose contracts carried nothing out of every bucket, answer and
// provider-locations count for up to 72 hours. A burst of verdicts is either
// a build that stopped carrying for real clients or a platform fault that
// stopped acknowledgements and URL probes at once, and in a country the
// URL-success rule exempts no URL success clears such a verdict early. The
// signal reads how many verdicts the judge made in the trailing hour, summed
// over Taskworker processes, from Mimir. Counts and ages are the only output:
// never a provider, a network or a country.
func NewZeroCarryVerdictsSignal() Signal {
	return NewZeroCarryVerdictsSignalWithSettings(DefaultZeroCarryVerdictsSettings())
}

// The signal with explicit thresholds.
func NewZeroCarryVerdictsSignalWithSettings(settings *ZeroCarryVerdictsSettings) Signal {
	return &signalAdapter{
		number: "2.32", key: "zero-carry-verdicts", name: "Zero-carry verdict spikes",
		probe: zeroCarryVerdictsProbe{settings: settings},
	}
}

const (
	zeroCarryVerdictsProbeId       = "runtime/zero-carry-verdicts"
	zeroCarryVerdictsChangesMetric = "urnetwork_provider_zero_carry_changes_total"
	zeroCarryVerdictsLastRunMetric = "urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds"
	zeroCarryVerdictsPlaybook      = "SIGNALS.md §2.32"
	zeroCarryVerdictsTargetKey     = "/zero-carry-verdicts"
)

// The thresholds and bounds of §2.32.
type ZeroCarryVerdictsSettings struct {
	// the trailing window the verdicts made are counted over
	SpikeWindow time.Duration
	// as many verdicts made within SpikeWindow are a spike: twice the whole
	// standing population the judge's first pass over Main made (52 on
	// 2026-11-08), in one hour
	SpikeVerdicts float64
	// consecutive cadences of a spike before it warns
	SpikeSustain int
	// the Taskworker service that runs the judge
	TaskworkerService string
	// the one fleet read
	FleetReadSettings
}

// The defaults of SIGNALS.md §2.32.
func DefaultZeroCarryVerdictsSettings() *ZeroCarryVerdictsSettings {
	return &ZeroCarryVerdictsSettings{
		SpikeWindow:       time.Hour,
		SpikeVerdicts:     100,
		SpikeSustain:      2,
		TaskworkerService: "taskworker",
		FleetReadSettings: FleetReadSettings{
			EvaluationLag:    2 * time.Minute,
			Freshness:        3 * time.Minute,
			FutureSkew:       30 * time.Second,
			QueryTimeout:     15 * time.Second,
			MaxResponseBytes: 64 * 1024,
			MaxRows:          64,
		},
	}
}

type zeroCarryVerdictsProbe struct {
	settings *ZeroCarryVerdictsSettings
}

func (zeroCarryVerdictsProbe) id() string             { return zeroCarryVerdictsProbeId }
func (zeroCarryVerdictsProbe) tier() string           { return tierWarn }
func (zeroCarryVerdictsProbe) cadence() time.Duration { return 5 * time.Minute }

// One instant read: the verdicts made over the trailing window, summed over
// Taskworker processes (a reset-aware increase on each, so a restart or the
// judge moving between processes loses nothing), and the newest judge run,
// which tells a fleet that exports the judge's series from one that does not.
func zeroCarryVerdictsQuery(environment string, settings *ZeroCarryVerdictsSettings) string {
	base := "env=" + strconv.Quote(environment) + ",job=" + strconv.Quote(settings.TaskworkerService)
	window := strconv.FormatInt(int64(settings.SpikeWindow/time.Second), 10) + "s"
	return strings.Join([]string{
		fleetPart("created", "sum(increase("+zeroCarryVerdictsChangesMetric+"{"+base+`,change="created"}[`+window+"]))"),
		fleetPart("judged", "max("+zeroCarryVerdictsLastRunMetric+"{"+base+"})"),
	}, " or ")
}

// What the read found: the verdicts made in the window and the newest run.
type zeroCarryVerdictsFleet struct {
	hasCreated bool
	created    float64
	hasJudged  bool
	judgedAt   float64
	gateway    string
}

func (self zeroCarryVerdictsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultZeroCarryVerdictsSettings()
	}
	target := env.cfg.env + zeroCarryVerdictsTargetKey
	reasons := []string{}

	var fleet *zeroCarryVerdictsFleet
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	out, gateway, err := readFleetMimir(ctx, env, zeroCarryVerdictsQuery(env.cfg.env, settings), at, settings.FleetReadSettings)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		reasons = append(reasons, "fleet_unavailable "+observationFailureDetails(err, classifyObservationError(err)))
	} else if samples, err := parseFleetSamples(out, at, settings.FleetReadSettings); err != nil {
		reasons = append(reasons, "fleet_invalid_response")
	} else {
		fleet = &zeroCarryVerdictsFleet{gateway: gateway.name}
		latest := float64(at.Add(settings.FutureSkew).Unix())
		for _, sample := range samples {
			valid := !math.IsNaN(sample.value) && !math.IsInf(sample.value, 0) && 0 <= sample.value
			switch sample.part {
			case "created":
				if !valid || fleet.hasCreated {
					reasons = append(reasons, "invalid_fleet_sample")
					continue
				}
				fleet.hasCreated = true
				fleet.created = sample.value
			case "judged":
				if !valid || latest < sample.value || fleet.hasJudged {
					reasons = append(reasons, "invalid_fleet_sample")
					continue
				}
				fleet.hasJudged = true
				fleet.judgedAt = sample.value
			}
		}
		if !fleet.hasJudged {
			reasons = append(reasons, "judge_series_missing")
		} else if !fleet.hasCreated {
			reasons = append(reasons, "created_series_missing")
		}
	}
	return evaluateZeroCarryVerdicts(settings, target, at, fleet, reasons), nil
}

func evaluateZeroCarryVerdicts(
	settings *ZeroCarryVerdictsSettings,
	target string,
	at time.Time,
	fleet *zeroCarryVerdictsFleet,
	reasons []string,
) []finding {
	findings := []finding{}
	newFinding := func(class string, sustain int) finding {
		return finding{
			probeId: zeroCarryVerdictsProbeId, tier: tierWarn, class: class, target: target, sustain: sustain,
			playbook: zeroCarryVerdictsPlaybook,
		}
	}

	fleetText := "fleet=unknown"
	if fleet != nil {
		created := "unknown"
		if fleet.hasCreated {
			created = strconv.FormatFloat(fleet.created, 'f', 0, 64)
		}
		newest, age := "none", "none"
		if fleet.hasJudged && 0 < fleet.judgedAt {
			judgedAt := time.Unix(0, int64(fleet.judgedAt*float64(time.Second))).UTC()
			newest = judgedAt.Format(time.RFC3339)
			age = max(0, at.Sub(judgedAt)).Truncate(time.Second).String()
		}
		fleetText = fmt.Sprintf("verdicts_made=%s window=%s spike_at=%.0f newest_run=%s newest_run_age=%s metrics_gateway=%s",
			created, settings.SpikeWindow, settings.SpikeVerdicts, newest, age, fleet.gateway)
	}

	switch {
	case fleet != nil && fleet.hasCreated && settings.SpikeVerdicts <= fleet.created:
		f := newFinding("zero-carry-verdict-spike", settings.SpikeSustain)
		f.symptom = fmt.Sprintf("The zero-carry judge made %.0f verdicts in the last %s", fleet.created, settings.SpikeWindow)
		f.mechanism = "A verdict takes a provider out of every bucket, FindProviders2 answer and provider-locations count for up to 72 hours: its contracts from at least three client networks carried nothing in a day, idle sessions left out, and every URL probe in eight hours failed. A wave of verdicts is either a release whose providers stopped carrying for current clients, or a platform fault that stopped acknowledgements and URL probes together. Outside the countries the URL-success rule exempts, the first URL success after recovery clears a verdict on the judge's next run; inside them nothing clears it before its expiry."
		f.baseline = fmt.Sprintf("The judge's first pass over Main made 52 verdicts in about an hour (2026-11-08); fewer than %.0f in any %s since.", settings.SpikeVerdicts, settings.SpikeWindow)
		f.observed = fleetText
		f.evidence = "sum(increase(urnetwork_provider_zero_carry_changes_total{change=\"created\"}[1h])) over the env's Taskworker processes, through one loopback Mimir read."
		f.context = "The egress probes dashboard's zero-carry judge row charts the verdicts in force by country, the changes by why, and the confirming reads by outcome. The providers dashboard's left out: zero carry stat is the same population as the pool counts it."
		f.action = "Read the Taskworker's [zero-carry] lines for the countries and the client versions of the new verdicts. A release of the provider app or of connect in the last day points to a build that stopped carrying: roll it back, and the verdicts clear as its devices report the old version. A platform fault (an outage of the data plane, a prober that cannot reach providers) clears outside exempt countries once URL probes succeed again. There is no runtime switch: inside exempt countries a wrong verdict holds until its expiry, 72 hours after it was made, and deleting provider_zero_carry rows alone does not help, because the judge re-creates them from the same day's contracts. To lift a wrong burst sooner, stop the judge first (roll the Taskworker back to a build without it), then delete the rows; ask the owner before either."
		f.verify = fmt.Sprintf("Fewer than %.0f verdicts made in the trailing %s on two cadences.", settings.SpikeVerdicts, settings.SpikeWindow)
		findings = append(findings, f)
	case fleet != nil && fleet.hasCreated:
		findings = append(findings, healthyFinding(zeroCarryVerdictsProbeId, tierWarn, "zero-carry-verdict-spike", target))
	}

	if 0 < len(reasons) {
		unique := []string{}
		seen := map[string]bool{}
		for _, reason := range reasons {
			if !seen[reason] {
				seen[reason] = true
				unique = append(unique, reason)
			}
		}
		f := newFinding("zero-carry-verdicts-unobservable", 2)
		f.symptom = "Zero-carry verdict spikes cannot be established"
		f.mechanism = "The Mimir read failed or was annotated, no Taskworker exports the zero-carry judge's series, or a sample was invalid (a run time ahead of the clock), so a burst of verdicts cannot be ruled out from what was read."
		f.baseline = "Every Taskworker exports urnetwork_provider_zero_carry_changes_total and urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds once it runs a build with the judge."
		f.observed = "visibility_reason=" + strings.Join(unique, ";") + " " + fleetText
		f.evidence = "One loopback Mimir read of the two series. A judge's verdicts made on a process that is not scraped are not counted, so a missing Taskworker slot can hide part of a burst."
		f.context = "A Taskworker built before §2.32 exports none of the judge's series, so before the roll, mid-roll and after a rollback the signal is unobservable and pages nobody."
		f.action = "Restore the Mimir path, or finish the Taskworker rollout of the build that exports the series, and rerun."
		f.verify = "Both series read on two cadences."
		findings = append(findings, f)
	}
	return findings
}
