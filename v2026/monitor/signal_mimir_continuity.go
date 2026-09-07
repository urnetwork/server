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
	"sync"
	"time"
)

const (
	mimirContinuityMarker       = "monitor-signal-11.20-mimir-continuity"
	mimirContinuityWindow       = 7 * 24 * time.Hour
	mimirContinuityStep         = 5 * time.Minute
	mimirContinuityLookback     = 2 * time.Minute
	mimirContinuityMissingSteps = 3
	// Mimir 3.1.1's compacted-store default is the longest interval during
	// which a freshly replaced ephemeral ingester may have preserved data that
	// is not yet queryable from the new generation. Two evaluation steps cover
	// range rounding and bounded discovery; the exact live settings remain the
	// independent §11.21 source of truth.
	mimirContinuityDefaultQueryStoreAfter = 12 * time.Hour
	mimirContinuityStoreBoundarySlack     = 2 * mimirContinuityStep
)

// Signal mimir-continuity implements SIGNALS.md §11.20. It uses an
// always-emitted service identity metric to distinguish an observation gap
// from real zero traffic and from a Grafana panel or datasource failure. A
// repeated watcher observation then distinguishes a moving recent-store
// boundary from a fixed gap.
func NewMimirContinuitySignal() Signal {
	return &signalAdapter{
		number: "11.20", key: "mimir-continuity", name: "Mimir historical sample continuity",
		probe: &mimirContinuityProbe{},
	}
}

type mimirContinuityProbe struct {
	historyLock sync.Mutex
	history     map[int64]mimirContinuityGapHistory
}

func (*mimirContinuityProbe) id() string             { return "observability/mimir-continuity" }
func (*mimirContinuityProbe) tier() string           { return tierWarn }
func (*mimirContinuityProbe) cadence() time.Duration { return 5 * time.Minute }

type mimirRangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

type mimirContinuityGap struct {
	previous time.Time
	resumed  time.Time
	missing  int
}

func (g mimirContinuityGap) missingStart() time.Time {
	return g.previous.Add(mimirContinuityStep)
}

func (g mimirContinuityGap) missingEnd() time.Time {
	return g.resumed.Add(-mimirContinuityStep)
}

type mimirContinuityClassification string

const (
	mimirContinuityUnclassified mimirContinuityClassification = "unclassified"
	mimirContinuityRecovering   mimirContinuityClassification = "query-store-recovering"
	mimirContinuityFixedLoss    mimirContinuityClassification = "fixed-loss"
)

type mimirContinuityGapHistory struct {
	anchorStart      time.Time
	anchorObservedAt time.Time
	lastStart        time.Time
	lastObservedAt   time.Time
	recovering       bool
	stationaryTicks  int
	recoveryMovement time.Duration
	recoveryElapsed  time.Duration
}

type mimirContinuityAssessment struct {
	gap              mimirContinuityGap
	classification   mimirContinuityClassification
	recoveryMovement time.Duration
	recoveryElapsed  time.Duration
}

func mimirContinuityQuery(environment string) string {
	return fmt.Sprintf(
		`sum(count_over_time(urnetwork_build_info{env=%s,instance!=""}[%s]))`,
		strconv.Quote(environment),
		mimirContinuityLookback,
	)
}

func (p *mimirContinuityProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	metricHosts := env.cfg.hostsWithRole("services")
	if len(metricHosts) == 0 {
		return nil, fmt.Errorf("mimir continuity: no services host in inventory for the loopback Mimir query")
	}

	end := env.now().UTC().Truncate(mimirContinuityStep)
	start := end.Add(-mimirContinuityWindow)
	queryValues := url.Values{}
	queryValues.Set("query", mimirContinuityQuery(env.cfg.env))
	queryValues.Set("start", strconv.FormatInt(start.Unix(), 10))
	queryValues.Set("end", strconv.FormatInt(end.Unix(), 10))
	queryValues.Set("step", strconv.FormatInt(int64(mimirContinuityStep/time.Second), 10))
	queryURL := "http://127.0.0.1:3100/prometheus/api/v1/query_range?" + queryValues.Encode()
	command := "# " + mimirContinuityMarker + "\n" +
		"curl -fsS --max-time 20 '" + queryURL + "'"
	out, metricHost, err := shellFirstServiceGateway(ctx, env.runner, metricHosts, nil, command)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: query Mimir through service gateways: %w", err)
	}

	timestamps, err := parseMimirContinuityTimestamps(out)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: response from %s: %w", metricHost.name, err)
	}
	gaps, err := findMimirContinuityGaps(timestamps)
	if err != nil {
		return nil, fmt.Errorf("mimir continuity: response from %s: %w", metricHost.name, err)
	}
	if len(gaps) == 0 {
		p.observeGaps(end, nil)
		return mimirContinuityHealthyFindings(nil), nil
	}

	assessments := p.observeGaps(end, gaps)
	grouped := map[mimirContinuityClassification][]mimirContinuityAssessment{}
	for _, assessment := range assessments {
		grouped[assessment.classification] = append(grouped[assessment.classification], assessment)
	}
	findings := make([]finding, 0, len(grouped)+2)
	for _, classification := range []mimirContinuityClassification{
		mimirContinuityFixedLoss,
		mimirContinuityRecovering,
		mimirContinuityUnclassified,
	} {
		if classified := grouped[classification]; len(classified) > 0 {
			findings = append(findings, mimirContinuityGapFinding(
				classification, classified, start, end, metricHost.name,
			))
		}
	}
	findings = append(findings, mimirContinuityHealthyFindings(grouped)...)
	return findings, nil
}

func mimirContinuityHealthyFindings(
	broken map[mimirContinuityClassification][]mimirContinuityAssessment,
) []finding {
	classes := []struct {
		classification mimirContinuityClassification
		class          string
	}{
		{mimirContinuityFixedLoss, "mimir-ingestion-gap"},
		{mimirContinuityRecovering, "mimir-query-store-visibility-gap"},
		{mimirContinuityUnclassified, "mimir-continuity-gap-unclassified"},
	}
	findings := make([]finding, 0, len(classes))
	for _, candidate := range classes {
		if len(broken[candidate.classification]) > 0 {
			continue
		}
		findings = append(findings, healthyFinding(
			"observability/mimir-continuity", tierWarn,
			candidate.class, "mimir-global-continuity",
		))
	}
	return findings
}

func (p *mimirContinuityProbe) observeGaps(now time.Time, gaps []mimirContinuityGap) []mimirContinuityAssessment {
	p.historyLock.Lock()
	defer p.historyLock.Unlock()

	if p.history == nil {
		p.history = map[int64]mimirContinuityGapHistory{}
	}
	next := make(map[int64]mimirContinuityGapHistory, len(gaps))
	assessments := make([]mimirContinuityAssessment, 0, len(gaps))
	for _, gap := range gaps {
		key := gap.resumed.Unix()
		missingStart := gap.missingStart()
		history, observedBefore := p.history[key]
		if !observedBefore || !now.After(history.lastObservedAt) || missingStart.Before(history.lastStart) {
			history = mimirContinuityGapHistory{
				anchorStart: missingStart, anchorObservedAt: now,
				lastStart: missingStart, lastObservedAt: now,
			}
			observedBefore = false
		}

		classification := mimirContinuityUnclassified
		if observedBefore {
			stepMovement := missingStart.Sub(history.lastStart)
			stepElapsed := now.Sub(history.lastObservedAt)
			advancingWithClock := stepMovement > 0 &&
				absDuration(stepMovement-stepElapsed) <= mimirContinuityStep
			switch {
			case advancingWithClock:
				history.recovering = true
				history.stationaryTicks = 0
				history.recoveryMovement = missingStart.Sub(history.anchorStart)
				history.recoveryElapsed = now.Sub(history.anchorObservedAt)
				classification = mimirContinuityRecovering
			case missingStart.Equal(history.lastStart):
				history.stationaryTicks++
				if history.recovering {
					// One stationary cadence can be range rounding or store
					// discovery jitter. A second one proves that the boundary
					// is no longer advancing with wall clock.
					classification = mimirContinuityRecovering
				}
			default:
				history.recovering = false
				history.stationaryTicks = 0
			}

			storeBoundary := gap.missingEnd().Add(
				mimirContinuityDefaultQueryStoreAfter + mimirContinuityStoreBoundarySlack,
			)
			stationaryLimit := 1
			if history.recovering {
				stationaryLimit = 2
			}
			if !now.Before(storeBoundary) &&
				missingStart.Equal(history.lastStart) &&
				history.stationaryTicks >= stationaryLimit {
				// A second observation is required before taking the fixed-loss
				// branch. Once movement has been proven, tolerate one additional
				// stationary cadence before declaring the residual interval fixed.
				classification = mimirContinuityFixedLoss
				history.recovering = false
			}
		}

		history.lastStart = missingStart
		history.lastObservedAt = now
		next[key] = history
		assessments = append(assessments, mimirContinuityAssessment{
			gap: gap, classification: classification,
			recoveryMovement: history.recoveryMovement,
			recoveryElapsed:  history.recoveryElapsed,
		})
	}
	p.history = next
	return assessments
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func mimirContinuityGapFinding(
	classification mimirContinuityClassification,
	assessments []mimirContinuityAssessment,
	queryStart time.Time,
	queryEnd time.Time,
	gateway string,
) finding {
	totalMissing := 0
	worst := assessments[0]
	evidence := make([]string, 0, len(assessments))
	for _, assessment := range assessments {
		gap := assessment.gap
		totalMissing += gap.missing
		if gap.missing > worst.gap.missing {
			worst = assessment
		}
		line := fmt.Sprintf(
			"%s through %s: %d missing 5-minute evaluations (last present %s; resumed %s)",
			gap.missingStart().Format(time.RFC3339),
			gap.missingEnd().Format(time.RFC3339),
			gap.missing,
			gap.previous.Format(time.RFC3339),
			gap.resumed.Format(time.RFC3339),
		)
		if assessment.classification == mimirContinuityRecovering {
			line += fmt.Sprintf(
				"; observed boundary movement=%s over elapsed=%s",
				assessment.recoveryMovement, assessment.recoveryElapsed,
			)
		}
		evidence = append(evidence, line)
	}

	result := finding{
		probeId: "observability/mimir-continuity", tier: tierWarn,
		target: "mimir-global-continuity", frame: "urnetwork_build_info", sustain: 1,
		baseline: fmt.Sprintf(
			"The aggregate urnetwork_build_info control has a sample at every %s evaluation across the trailing %s; fewer than %d consecutive missing evaluations in any bounded interval remain below the alert threshold.",
			mimirContinuityStep, mimirContinuityWindow, mimirContinuityMissingSteps,
		),
		observed: fmt.Sprintf(
			"classification=%s query_start=%s query_end=%s gateway=%s gaps=%d total_missing_steps=%d worst_missing_start=%s worst_missing_end=%s worst_missing_steps=%d",
			classification, queryStart.Format(time.RFC3339), queryEnd.Format(time.RFC3339), gateway,
			len(assessments), totalMissing, worst.gap.missingStart().Format(time.RFC3339),
			worst.gap.missingEnd().Format(time.RFC3339), worst.gap.missing,
		),
		evidence: strings.Join(evidence, "\n"),
		playbook: "SIGNALS.md §11.20 and §11.21",
	}

	switch classification {
	case mimirContinuityRecovering:
		result.class = "mimir-query-store-visibility-gap"
		result.symptom = fmt.Sprintf(
			"Mimir is progressively restoring %d five-minute control evaluations across %d bounded query gap(s)",
			totalMissing, len(assessments),
		)
		result.mechanism = "The same gap's right edge stayed fixed while its left edge advanced with wall clock. Previously absent historical evaluations therefore became readable without producer backfill. This is the deterministic Mimir recent-store cutoff signature after replacement ingesters lose the old generation's local head; it is not permanent raw-sample loss."
		result.context = "Build-info is independent of user traffic and the range query bypasses the Grafana panel. The metrics front assigns receive time, so a current producer cannot recreate those old timestamps. Exact-process §11.21 values distinguish the known compacted-store cutoff from another store-visibility mechanism."
		result.action = "Run §11.21 and preserve the moving boundaries. Do not zero the store horizons solely to clear this alert: Mimir 3.1.1 warns that doing so queries replicated non-compacted blocks. Choosing zero-horizon reads, a long read-only handoff, or a dedicated persistent Mimir tier is an operator architecture decision, not an automatic monitor repair."
		result.verify = "The existing gap becomes fully queryable by its configured store-age boundary. After an explicitly selected replacement design is deployed, a controlled and full replacement creates no new bounded gap through its complete handoff and discovery window."
	case mimirContinuityFixedLoss:
		result.class = "mimir-ingestion-gap"
		result.symptom = fmt.Sprintf(
			"Mimir still lacks %d five-minute control evaluations across %d fixed bounded gap(s) after the recent-store boundary",
			totalMissing, len(assessments),
		)
		result.mechanism = "The bounded interval remained absent on consecutive observations after its right edge exceeded Mimir 3.1.1's 12-hour query-store default plus the evaluation/discovery allowance. The default recent-store split can no longer explain it. The old ephemeral ingester head was not durably readable, or the long-term store has a persistent interval-specific loss."
		result.context = "This is not zero throughput or a Grafana panel denominator: build-info is always emitted and the probe queries Mimir directly. Unlike an advancing-left-edge gap, this fixed branch is a real historical observation loss; source and independent-family reconciliation determine whether ingestion or object storage owned it."
		result.action = "Correlate the exact fixed interval with independent Connect/taskworker controls and the old/new Mimir process boundary. Run §11.21 for current configuration, but do not redeploy solely to clear historical evidence, zero-fill the range, or shared-mount a TSDB across overlapping generations."
		result.verify = "The selected replacement design passes §11.21 and the next controlled then full replacement creates no new bounded gap. The fixed historical interval remains recorded until it leaves the seven-day window; only independently proven store recovery may reclassify it."
	default:
		result.class = "mimir-continuity-gap-unclassified"
		result.symptom = fmt.Sprintf(
			"Mimir has %d unavailable five-minute control evaluations across %d bounded gap(s) whose permanence is not yet classified",
			totalMissing, len(assessments),
		)
		result.mechanism = "One range response proves that historical control evaluations are currently unavailable, but cannot distinguish raw loss from a recent block that replacement ingesters no longer hold and the store path does not yet query. A repeated observation supplies the moving-versus-fixed discriminator."
		result.context = "This is a Mimir observation gap, not zero throughput and not a Grafana panel failure. It deliberately makes no permanence claim on the first observation or before the configured/default recent-store boundary."
		result.action = "Keep the standing watcher on the same query and run §11.21. An approximately wall-clock-moving left edge with a fixed right edge becomes mimir-query-store-visibility-gap; a consecutive fixed interval beyond the store-age boundary becomes mimir-ingestion-gap. Do not mutate or hide the range while classification is pending."
		result.verify = "A subsequent cadence produces the explicit recovering or fixed classification, or every bounded gap becomes queryable and the signal clears."
	}
	return result
}

func parseMimirContinuityTimestamps(output string) ([]time.Time, error) {
	var response mimirRangeResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return nil, fmt.Errorf("decode Mimir response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "matrix" {
		return nil, fmt.Errorf(
			"Mimir status=%q result_type=%q error=%q",
			response.Status, response.Data.ResultType, response.Error,
		)
	}
	if len(response.Data.Result) != 1 {
		return nil, fmt.Errorf("Mimir returned %d aggregate continuity series, want 1", len(response.Data.Result))
	}

	timestamps := make([]time.Time, 0, len(response.Data.Result[0].Values))
	for _, raw := range response.Data.Result[0].Values {
		observedAt, value, err := mimirInstantValue(raw)
		if err != nil {
			return nil, fmt.Errorf("parse continuity sample: %w", err)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return nil, fmt.Errorf("continuity control has invalid value %v at %s", value, observedAt.Format(time.RFC3339Nano))
		}
		timestamps = append(timestamps, observedAt.UTC())
	}
	if len(timestamps) == 0 {
		return nil, fmt.Errorf("Mimir returned no continuity control samples")
	}
	sort.Slice(timestamps, func(i, j int) bool { return timestamps[i].Before(timestamps[j]) })
	return timestamps, nil
}

func findMimirContinuityGaps(timestamps []time.Time) ([]mimirContinuityGap, error) {
	if len(timestamps) == 0 {
		return nil, fmt.Errorf("no continuity timestamps")
	}
	gaps := []mimirContinuityGap{}
	for index := 1; index < len(timestamps); index++ {
		delta := timestamps[index].Sub(timestamps[index-1])
		stepCount := math.Round(float64(delta) / float64(mimirContinuityStep))
		if delta <= 0 || math.Abs(float64(delta)-stepCount*float64(mimirContinuityStep)) > float64(time.Millisecond) {
			return nil, fmt.Errorf(
				"irregular evaluation timestamps %s then %s",
				timestamps[index-1].Format(time.RFC3339Nano), timestamps[index].Format(time.RFC3339Nano),
			)
		}
		missing := int(stepCount) - 1
		if missing >= mimirContinuityMissingSteps {
			gaps = append(gaps, mimirContinuityGap{
				previous: timestamps[index-1],
				resumed:  timestamps[index],
				missing:  missing,
			})
		}
	}
	return gaps, nil
}
