package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §2.31 maps to signal_egress_tls_judge.go and
// signal_egress_tls_judge_test.go. A URL turn's TLS failure is stored
// pending, excluding nobody, until the Taskworker's TLS finding judge
// (JudgeProviderEgressTlsFindings, every five minutes on one Taskworker at a
// time) decides it, so the exclusion of a provider that intercepts fails open
// while the judge is not judging, or while it cannot decide a finding. Each
// judged run exports when it ran and how many pending findings it left
// undecided inside the direct window; the signal reads the newest of them
// from Mimir, with the gauge's freshness on every desired Taskworker slot.
// Counts, ages and slot names are the only output: never a URL, a provider
// or a website.
func NewEgressTlsJudgeSignal() Signal {
	return NewEgressTlsJudgeSignalWithSettings(DefaultEgressTlsJudgeSettings())
}

// The signal with explicit thresholds.
func NewEgressTlsJudgeSignalWithSettings(settings *EgressTlsJudgeSettings) Signal {
	return &signalAdapter{
		number: "2.31", key: "egress-tls-judge", name: "TLS finding judge liveness and undecided findings",
		probe: egressTlsJudgeProbe{settings: settings},
	}
}

const (
	egressTlsJudgeProbeId          = "runtime/egress-tls-judge"
	egressTlsJudgeLastJudgedMetric = "urnetwork_egress_tls_judge_last_judged_timestamp_seconds"
	egressTlsJudgeUndecidedMetric  = "urnetwork_egress_tls_undecided_findings"
	egressTlsJudgePlaybook         = "SIGNALS.md §2.31"
	egressTlsJudgeTargetKey        = "/egress-tls-judge"
)

// The thresholds and bounds of §2.31.
type EgressTlsJudgeSettings struct {
	// the fleet's newest judged run older than this is stale: four of the
	// judge's five-minute intervals
	StaleAfter time.Duration
	// consecutive stale cadences after which the stale alert pages
	StalePageSustain int
	// With a desired slot not reporting, the newest judged run on the slots
	// that report must be older than this to raise stale, which then pages
	// from its second cadence: when the stale alert pages with every slot
	// reporting, StaleAfter and five more cadences. A judge on an unread slot
	// is not ruled out sooner; one stopped is not missed for good.
	StalePageAfter time.Duration
	// consecutive cadences on which the newest judged run left a decidable
	// finding undecided before it warns: thirty minutes at the cadence
	UndecidedSustain int
	// the Taskworker service that runs the judge
	TaskworkerService string
	// the one fleet read
	FleetReadSettings
	// the most slot gaps one finding lists
	MaxListed int
}

// The defaults of SIGNALS.md §2.31.
func DefaultEgressTlsJudgeSettings() *EgressTlsJudgeSettings {
	return &EgressTlsJudgeSettings{
		StaleAfter:        20 * time.Minute,
		StalePageSustain:  6,
		StalePageAfter:    45 * time.Minute,
		UndecidedSustain:  6,
		TaskworkerService: "taskworker",
		FleetReadSettings: FleetReadSettings{
			EvaluationLag:    2 * time.Minute,
			Freshness:        3 * time.Minute,
			FutureSkew:       30 * time.Second,
			QueryTimeout:     15 * time.Second,
			MaxResponseBytes: 256 * 1024,
			MaxRows:          1024,
		},
		MaxListed: 12,
	}
}

type egressTlsJudgeProbe struct {
	settings *EgressTlsJudgeSettings
}

func (egressTlsJudgeProbe) id() string             { return egressTlsJudgeProbeId }
func (egressTlsJudgeProbe) tier() string           { return tierWarn }
func (egressTlsJudgeProbe) cadence() time.Duration { return 5 * time.Minute }

// The newest judged run across the fleet, and the slot coverage of the gauge
// it comes from.
type egressTlsJudgeFleet struct {
	// the newest judged run's Unix seconds, 0 when no process has judged
	// since it started
	judged     bool
	judgedAt   float64
	judgedSlot string
	// the pending findings that run left undecided inside the direct window
	hasUndecided bool
	undecided    float64
	// every desired Taskworker slot exports a fresh last-judged gauge
	complete bool
	coverage string
	gateway  string
}

// One instant read: the newest last-judged gauge over Taskworker processes,
// that process's undecided count, and per-slot freshness of the gauge on the
// slots that must export it.
func egressTlsJudgeQuery(environment string, settings *EgressTlsJudgeSettings, taskworkers fleetSlotInventory) string {
	base := "env=" + strconv.Quote(environment) + ",job=" + strconv.Quote(settings.TaskworkerService)
	newest := "topk(1, " + egressTlsJudgeLastJudgedMetric + "{" + base + "})"
	parts := []string{
		fleetPart("judged", newest),
		fleetPart("undecided", egressTlsJudgeUndecidedMetric+"{"+base+"} and on(job,host,block,instance) "+newest),
	}
	if selector := taskworkers.selector(environment); selector != "" {
		parts = append(parts, fleetSlotFreshnessPart("judged", egressTlsJudgeLastJudgedMetric, selector))
	}
	return strings.Join(parts, " or ")
}

func (self egressTlsJudgeProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultEgressTlsJudgeSettings()
	}
	target := env.cfg.env + egressTlsJudgeTargetKey
	reasons := []string{}

	var fleet *egressTlsJudgeFleet
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	taskworkers := newFleetSlotInventory(env, []string{settings.TaskworkerService}, nil)
	out, gateway, err := readFleetMimir(ctx, env, egressTlsJudgeQuery(env.cfg.env, settings, taskworkers), at, settings.FleetReadSettings)
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
		fleet = &egressTlsJudgeFleet{gateway: gateway.name}
		latest := float64(at.Add(settings.FutureSkew).Unix())
		for _, sample := range samples {
			valid := !math.IsNaN(sample.value) && !math.IsInf(sample.value, 0) && 0 <= sample.value
			switch sample.part {
			case "judged":
				if !valid || latest < sample.value || fleet.judged {
					reasons = append(reasons, "invalid_fleet_sample")
					continue
				}
				fleet.judged = true
				fleet.judgedAt = sample.value
				fleet.judgedSlot = sample.labels["job"] + "/" + sample.labels["host"] + "/" + sample.labels["block"]
			case "undecided":
				if !valid || fleet.hasUndecided {
					reasons = append(reasons, "invalid_fleet_sample")
					continue
				}
				fleet.hasUndecided = true
				fleet.undecided = sample.value
			}
		}
		coverage := newFleetSlotCoverage(taskworkers, samples, []string{"judged"}, at, settings.FleetReadSettings, nil)
		fleet.complete = coverage.full() && fleet.judged
		fleet.coverage = "taskworker_" + coverage.observed(settings.MaxListed)
		if !fleet.judged {
			reasons = append(reasons, "judge_series_missing")
		} else if !fleet.hasUndecided {
			reasons = append(reasons, "undecided_series_missing")
		}
		if !coverage.full() {
			reasons = append(reasons, "slot_coverage_incomplete")
		}
	}
	return evaluateEgressTlsJudge(settings, target, at, fleet, reasons), nil
}

func evaluateEgressTlsJudge(
	settings *EgressTlsJudgeSettings,
	target string,
	at time.Time,
	fleet *egressTlsJudgeFleet,
	reasons []string,
) []finding {
	findings := []finding{}
	newFinding := func(class string, sustain int) finding {
		return finding{
			probeId: egressTlsJudgeProbeId, tier: tierWarn, class: class, target: target, sustain: sustain,
			playbook: egressTlsJudgePlaybook,
		}
	}

	// the newest judged run, and whether it is recent enough to stand for
	// the judge now
	fleetText := "fleet=unknown"
	fresh := false
	var age time.Duration
	ageText := "none"
	if fleet != nil {
		newest := "none"
		undecided := "unknown"
		if fleet.judged && 0 < fleet.judgedAt {
			judgedAt := time.Unix(0, int64(fleet.judgedAt*float64(time.Second))).UTC()
			age = max(0, at.Sub(judgedAt))
			fresh = age <= settings.StaleAfter
			newest = judgedAt.Format(time.RFC3339)
			ageText = age.Truncate(time.Second).String()
		}
		if fleet.hasUndecided {
			undecided = strconv.FormatFloat(fleet.undecided, 'f', 0, 64)
		}
		fleetText = fmt.Sprintf("newest_judged_run=%s newest_judged_age=%s judged_slot=%s undecided_findings=%s stale_after=%s metrics_gateway=%s %s",
			newest, ageText, fleet.judgedSlot, undecided, settings.StaleAfter, fleet.gateway, fleet.coverage)
	}

	// No run has judged for a while. With every desired slot reporting, that
	// is known from StaleAfter. With a slot not reporting, an unread slot may
	// hold the newest run, so the slots that report must show none for
	// StalePageAfter, when a fully read judge would page; then it pages at
	// once. A slot that reports only zero has not judged since it started, as
	// mid-roll, which proves nothing either way.
	partial := fleet != nil && !fleet.complete && fleet.judged && 0 < fleet.judgedAt && settings.StalePageAfter < age
	switch {
	case fleet != nil && fleet.complete && !fresh, partial:
		f := newFinding("egress-tls-judge-stale", 2)
		f.pageSustain = settings.StalePageSustain
		switch {
		case partial:
			f.pageSustain = 2
			f.symptom = fmt.Sprintf("No TLS finding judge run has judged for %s on any Taskworker slot that reports, beyond the %s a fully read judge pages at", ageText, settings.StalePageAfter)
		case 0 < fleet.judgedAt:
			f.symptom = fmt.Sprintf("No TLS finding judge run has judged for %s", ageText)
		default:
			f.symptom = "No Taskworker process has completed a judged TLS finding judge run since it started"
		}
		f.mechanism = "A URL turn's TLS failure is stored pending, and a pending finding excludes nobody until the judge (JudgeProviderEgressTlsFindings, every five minutes on one Taskworker) decides it. While no run judges, every new finding stays pending, so a provider that intercepts TLS stays eligible: the exclusion fails open. A run past its ten-minute bound or a panic skips the Post that schedules the next run and retries on a backoff toward an hour; a pool the judge cannot build judges nothing."
		f.baseline = fmt.Sprintf("The fleet's newest judged run is under %s old: the judge runs every five minutes.", settings.StaleAfter)
		f.observed = fleetText
		f.evidence = "urnetwork_egress_tls_judge_last_judged_timestamp_seconds, the topk(1) over the env's Taskworker processes, with the gauge's freshness on every desired Taskworker slot, through one loopback Mimir read. Every slot exports it, 0 until its process has judged."
		f.context = "urnetwork_egress_tls_judge_runs_total{result} on the egress probes dashboard tells runs that could not build the served pool (pool_unavailable) from runs that never complete (no increase at all); the pending_task row under run_once_key judge_provider_egress_tls_findings carries the reschedule error and count."
		f.action = "Read the Taskworker's [egress]TLS judge lines and the task's reschedule error. For pool_unavailable the served destination pool could not be built, and its own errors on the api and the Taskworker name the cause; a run that times out is most often its direct checks (15 s each, eight at a time, at most 256 hosts). Once fixed, a Taskworker restart schedules a run at once."
		f.verify = fmt.Sprintf("The fleet's newest judged run is under %s old on two cadences.", settings.StaleAfter)
		findings = append(findings, f)
	case fresh:
		// a recent judged run on any slot proves the judge runs, however many
		// slots report
		findings = append(findings, healthyFinding(egressTlsJudgeProbeId, tierWarn, "egress-tls-judge-stale", target))
	}

	// The newest judged run left a pending finding undecided that a direct
	// check could still decide; the sustain asks it of every run for half an
	// hour. A stale count says nothing: the stale class owns that.
	switch {
	case fresh && fleet.hasUndecided && 0 < fleet.undecided:
		f := newFinding("egress-tls-findings-undecided", settings.UndecidedSustain)
		f.symptom = fmt.Sprintf("The newest TLS judge run left %.0f pending findings undecided although a direct check could still decide them", fleet.undecided)
		f.mechanism = "A pending finding excludes nobody. The judge decides a fresh failure by a direct, verified TLS handshake from the server to the URL's host. When that handshake is inconclusive (a dial error, a timeout, a reset, a host past the run's bound) the finding stays pending, and once its failure is older than the one-hour direct window no check can decide it, so a provider intercepting a site the server cannot reach is never excluded for it."
		f.baseline = "Every pending finding whose failure is inside the direct window is decided within a run or two, so the newest judged run leaves none undecided on most cadences."
		f.observed = fleetText
		f.evidence = "urnetwork_egress_tls_undecided_findings from the Taskworker process with the newest judged run (topk(1) of urnetwork_egress_tls_judge_last_judged_timestamp_seconds), through one loopback Mimir read."
		f.context = "urnetwork_egress_tls_direct_checks_total{outcome=\"inconclusive\"} on the egress probes dashboard counts the checks behind it, and each Taskworker [egress]TLS judge line reports undecided= and hosts_checked=. A provider that keeps failing its recheck keeps its finding inside the window, so the count holds while the host stays unreachable."
		f.action = "Find the hosts of the recent pending findings (provider_egress_url_security rows with tls_failure, tls_verdict 0 and a measured_at inside the hour) and check them from a Taskworker host: an unreachable address family, a firewall, or a host that refuses the server is the usual cause. Do not exclude a provider by hand on an undecided finding."
		f.verify = "The newest judged run reports no undecided finding on two cadences."
		findings = append(findings, f)
	case fresh && fleet.hasUndecided && fleet.complete:
		findings = append(findings, healthyFinding(egressTlsJudgeProbeId, tierWarn, "egress-tls-findings-undecided", target))
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
		f := newFinding("egress-tls-judge-unobservable", 2)
		f.symptom = "TLS finding judge liveness or undecided findings cannot be fully established"
		f.mechanism = "The Mimir read failed or was annotated, no Taskworker exports the judge gauges, or a desired Taskworker slot's last-judged gauge is missing, stale (older than three minutes), duplicated or on an excluded or disabled host, so a stopped judge cannot be ruled out from what was read."
		f.baseline = "Every desired Taskworker slot exports a fresh urnetwork_egress_tls_judge_last_judged_timestamp_seconds, and the newest judged run's urnetwork_egress_tls_undecided_findings reads."
		f.observed = "visibility_reason=" + strings.Join(unique, ";") + " " + fleetText
		f.evidence = "Desired slots come from active services.yml placement, not from surviving series. Findings proven from what was read are reported beside this."
		f.context = "A Taskworker built before §2.31 exports no judge gauges, so a fleet partly on an older build, during a roll or after a rollback, reads as unobservable, and this class never pages. Stale is raised beside it only once the slots that report show no judged run for the page bound, and pages then."
		f.action = "Restore the Mimir path, or finish the Taskworker rollout of the build that exports the gauges, and rerun."
		f.verify = "Every source reads on two cadences."
		findings = append(findings, f)
	}
	return findings
}
