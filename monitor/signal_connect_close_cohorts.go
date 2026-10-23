package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §23.3 maps to signal_connect_close_cohorts.go and
// signal_connect_close_cohorts_test.go. Connect counts authenticated H1
// transports by carrier and first observed terminal event
// (urnetwork_connect_h1_sessions_closed_total{carrier,reason}) and its
// providing clients' connections by app release
// (urnetwork_connect_provider_connections{app_version}). The close counter
// has no version label, so a close burst is attributed to a cohort by the
// matching drop in that cohort's connections in the same minute. A burst
// confined to websocket closes and to a few release cohorts, with h1plus flat
// and no Connect restart, is the client pattern seen after the 2026-10-13 mass
// reconnect and calls for no server action. A burst spread across carriers,
// cohorts or a restart points at the load balancer, network or Connect path.
// Bursts are one-minute events, so every run reads the per-minute history of
// a lookback as one range query: a watcher with a cadence floor still sees
// each minute. Only carrier, reason and release labels, counts and inventory
// slot names leave Mimir.
func NewConnectCloseCohortsSignal() Signal {
	return NewConnectCloseCohortsSignalWithSettings(DefaultConnectCloseCohortsSettings())
}

// The signal with explicit thresholds.
func NewConnectCloseCohortsSignalWithSettings(settings *ConnectCloseCohortsSettings) Signal {
	return &signalAdapter{
		number: "23.3", key: "connect-close-cohorts", name: "Connect H1 close bursts by carrier and provider cohort",
		probe: connectCloseCohortsProbe{settings: settings},
	}
}

const (
	connectCloseCohortsProbeId       = "mimir/connect-close-cohorts"
	connectH1ClosedMetric            = "urnetwork_connect_h1_sessions_closed_total"
	connectProviderConnectionsMetric = "urnetwork_connect_provider_connections"
	connectCarrierWebsocket          = "websocket"
	connectCarrierH1Plus             = "h1plus"
)

// Release labels are the server's bounded normalization: year.month.day for
// recent releases, year.month for older ones, unknown and other.
var connectCohortLabelPattern = regexp.MustCompile(`^(?:20[0-9]{2}\.[0-9]{1,2}(?:\.[0-9]{1,2})?|unknown|other)$`)
var connectCloseReasonPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,47}$`)

// The thresholds and bounds of §23.3.
type ConnectCloseCohortsSettings struct {
	// every run reads the per-minute history of Lookback at Step; each step's
	// closes are the increase over the step before it
	Lookback time.Duration
	Step     time.Duration
	// a carrier and reason is judged against its median over the lookback,
	// which needs at least MinBaselinePoints points
	MinBaselinePoints int
	// a burst is at least BurstMinimum closes in one step and at least
	// BurstMultiple times its median
	BurstMultiple float64
	BurstMinimum  float64
	// a cohort is struck when it lost at least CohortDropShare of its
	// connections and at least CohortMinDrop of them in the burst step
	CohortDropShare float64
	CohortMinDrop   float64
	// confined: struck cohorts carry at least ConfinedShare of the fleet's
	// drop while the rest of the fleet lost under RestDropShare of itself, and
	// the struck cohorts held at most MaxStruckShare of the fleet before it
	ConfinedShare  float64
	RestDropShare  float64
	MaxStruckShare float64
	// a slot whose newest process started within RestartWindow of a burst
	// step restarted during it
	RestartWindow time.Duration
	// the Connect service whose slots must report every family
	Service string
	// the one range read, which ends EvaluationLag behind now; MaxRows bounds
	// its points, and every family's freshness is judged at its last step
	FleetReadSettings
	// the most release cohorts one read accepts
	MaxCohorts int
	// the most cohorts and slot gaps one alert lists
	MaxListed int
}

// The defaults of SIGNALS.md §23.3.
func DefaultConnectCloseCohortsSettings() *ConnectCloseCohortsSettings {
	return &ConnectCloseCohortsSettings{
		Lookback:          15 * time.Minute,
		Step:              time.Minute,
		MinBaselinePoints: 10,
		BurstMultiple:     3,
		BurstMinimum:      1000,
		CohortDropShare:   0.2,
		CohortMinDrop:     100,
		ConfinedShare:     0.7,
		RestDropShare:     0.05,
		MaxStruckShare:    0.5,
		RestartWindow:     2 * time.Minute,
		Service:           "connect",
		FleetReadSettings: FleetReadSettings{
			EvaluationLag:    2 * time.Minute,
			Freshness:        3 * time.Minute,
			FutureSkew:       30 * time.Second,
			QueryTimeout:     15 * time.Second,
			MaxResponseBytes: 4 * 1024 * 1024,
			MaxRows:          65536,
		},
		MaxCohorts: 256,
		MaxListed:  5,
	}
}

type connectCloseCohortsProbe struct {
	settings *ConnectCloseCohortsSettings
}

func (connectCloseCohortsProbe) id() string             { return connectCloseCohortsProbeId }
func (connectCloseCohortsProbe) tier() string           { return tierWarn }
func (connectCloseCohortsProbe) cadence() time.Duration { return time.Minute }

func connectCloseCohortsQuery(environment string, settings *ConnectCloseCohortsSettings, inventory fleetSlotInventory) string {
	base := "env=" + strconv.Quote(environment) + ",job=" + strconv.Quote(settings.Service)
	parts := []string{
		fleetPart("closes", "sum by (carrier, reason) (increase("+connectH1ClosedMetric+"{"+base+"}["+promDuration(settings.Step)+"]))"),
		fleetPart("cohorts", "sum by (app_version) ("+connectProviderConnectionsMetric+"{"+base+"})"),
	}
	if selector := inventory.selector(environment); selector != "" {
		parts = append(parts,
			fleetSlotFreshnessPart("start", "process_start_time_seconds", selector),
			fleetSlotFreshnessPart("closes", connectH1ClosedMetric, selector),
			fleetSlotFreshnessPart("cohorts", connectProviderConnectionsMetric, selector),
			fleetPart("newest_start", "max by (job,host,block) (process_start_time_seconds{"+selector+"})"),
		)
	}
	return strings.Join(parts, " or ")
}

// The per-step history of one range read.
type connectCloseHistory struct {
	steps          []int64
	closes         map[string]map[int64]float64
	cohorts        map[string]map[int64]float64
	newestStarts   map[string]map[int64]float64
	invalidReasons []string
}

func newConnectCloseHistory(series []fleetSeries, steps []int64, settings *ConnectCloseCohortsSettings) *connectCloseHistory {
	history := &connectCloseHistory{
		steps: steps, closes: map[string]map[int64]float64{}, cohorts: map[string]map[int64]float64{}, newestStarts: map[string]map[int64]float64{},
	}
	reasons := map[string]bool{}
	valid := func(points map[int64]float64) bool {
		for _, value := range points {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return false
			}
		}
		return true
	}
	for _, one := range series {
		switch one.part {
		case "closes":
			carrier, reason := one.labels["carrier"], one.labels["reason"]
			if len(one.labels) != 2 || (carrier != connectCarrierWebsocket && carrier != connectCarrierH1Plus) || !connectCloseReasonPattern.MatchString(reason) {
				reasons["unrecognized_close_label"] = true
				continue
			}
			if !valid(one.points) {
				reasons["invalid_value"] = true
				continue
			}
			key := carrier + "/" + reason
			if _, duplicate := history.closes[key]; duplicate {
				reasons["duplicate_series"] = true
			}
			history.closes[key] = one.points
		case "cohorts":
			label := one.labels["app_version"]
			if len(one.labels) != 1 || !connectCohortLabelPattern.MatchString(label) {
				reasons["unrecognized_cohort"] = true
				continue
			}
			if !valid(one.points) {
				reasons["invalid_value"] = true
				continue
			}
			if _, duplicate := history.cohorts[label]; duplicate {
				reasons["duplicate_series"] = true
			}
			history.cohorts[label] = one.points
		case "newest_start":
			history.newestStarts[one.labels["job"]+"/"+one.labels["host"]+"/"+one.labels["block"]] = one.points
		default:
			if !strings.HasPrefix(one.part, "fresh/") {
				reasons["unrecognized_part"] = true
			}
		}
	}
	if settings.MaxCohorts < len(history.cohorts) {
		reasons["cohort_bound"] = true
	}
	for reason := range reasons {
		history.invalidReasons = append(history.invalidReasons, reason)
	}
	sort.Strings(history.invalidReasons)
	return history
}

// One judged burst step.
type connectCloseBurst struct {
	step        int64
	closes      map[string]float64
	burstCloses float64
	confined    bool
	reasons     []string
	struck      []string
	topCohort   string
	totalDrop   float64
	restShare   float64
	restarted   int
	h1PlusBurst bool
}

func (self connectCloseCohortsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultConnectCloseCohortsSettings()
	}
	target := env.cfg.env + "/connect-h1-closes"
	end := fleetEvaluationTime(env.now(), settings.EvaluationLag).Truncate(settings.Step)
	start := end.Add(-settings.Lookback)
	inventory := newFleetSlotInventory(env, []string{settings.Service}, nil)
	out, gateway, err := readFleetMimirRange(ctx, env, connectCloseCohortsQuery(env.cfg.env, settings, inventory), start, end, settings.Step, settings.FleetReadSettings)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []finding{connectCloseUnknown(target, "visibility_reason=source_unavailable "+observationFailureDetails(err, classifyObservationError(err)))}, nil
	}
	series, err := parseFleetSeries(out, start, end, settings.Step, settings.FleetReadSettings)
	if err != nil {
		return []finding{connectCloseUnknown(target, "visibility_reason=invalid_response metrics_gateway="+gateway.name)}, nil
	}
	steps := []int64{}
	for at := start; !at.After(end); at = at.Add(settings.Step) {
		steps = append(steps, at.Unix())
	}
	history := newConnectCloseHistory(series, steps, settings)
	coverage := newFleetSlotCoverage(inventory, fleetSamplesAt(series, end), []string{"start", "closes", "cohorts"}, end, settings.FleetReadSettings, nil)
	return evaluateConnectCloseCohorts(settings, target, gateway.name, history, coverage), nil
}

func evaluateConnectCloseCohorts(
	settings *ConnectCloseCohortsSettings,
	target string,
	gatewayName string,
	history *connectCloseHistory,
	coverage fleetSlotCoverage,
) []finding {
	reasons := append([]string(nil), history.invalidReasons...)
	if len(history.closes) == 0 {
		reasons = append(reasons, "close_series_missing")
	}
	if len(history.cohorts) == 0 {
		reasons = append(reasons, "cohort_series_missing")
	}
	if !coverage.full() {
		reasons = append(reasons, "slot_coverage_incomplete")
	}
	medians := map[string]float64{}
	for key, points := range history.closes {
		values := []float64{}
		for _, value := range points {
			values = append(values, value)
		}
		if settings.MinBaselinePoints <= len(values) {
			medians[key] = medianFloat64(values)
		}
	}
	cohortAt := func(label string, step int64) (float64, bool) {
		value, present := history.cohorts[label][step]
		return value, present
	}
	unknownBurstSteps := 0
	bursts := []connectCloseBurst{}
	for index := 1; index < len(history.steps); index++ {
		step, previous := history.steps[index], history.steps[index-1]
		burst := connectCloseBurst{step: step, closes: map[string]float64{}}
		unknownBurst := false
		for key, points := range history.closes {
			closes, present := points[step]
			if !present {
				continue
			}
			burst.closes[key] = closes
			if closes < settings.BurstMinimum {
				continue
			}
			median, known := medians[key]
			if !known {
				unknownBurst = true
				continue
			}
			if settings.BurstMultiple*median <= closes {
				burst.burstCloses += closes
				if strings.HasPrefix(key, connectCarrierH1Plus+"/") {
					burst.h1PlusBurst = true
				}
			}
		}
		if unknownBurst {
			unknownBurstSteps++
		}
		if burst.burstCloses == 0 {
			continue
		}
		websocketBurst := false
		for key, closes := range burst.closes {
			median, known := medians[key]
			if known && strings.HasPrefix(key, connectCarrierWebsocket+"/") && settings.BurstMinimum <= closes && settings.BurstMultiple*median <= closes {
				websocketBurst = true
			}
		}
		for _, points := range history.newestStarts {
			if started, present := points[step]; present && float64(step)-settings.RestartWindow.Seconds() < started && started <= float64(step) {
				burst.restarted++
			}
		}
		// the cohorts' connection drop over the same step as the closes
		labels := []string{}
		pointsBefore, pointsNow := 0, 0
		for label := range history.cohorts {
			labels = append(labels, label)
			if _, present := cohortAt(label, previous); present {
				pointsBefore++
			}
			if _, present := cohortAt(label, step); present {
				pointsNow++
			}
		}
		sort.Strings(labels)
		if pointsBefore == 0 || pointsNow == 0 {
			burst.reasons = append(burst.reasons, "cohort_points_missing")
		}
		struckDrop, struckBefore, restBefore, restDrop, topDrop := 0.0, 0.0, 0.0, 0.0, 0.0
		for _, label := range labels {
			before, _ := cohortAt(label, previous)
			now, _ := cohortAt(label, step)
			drop := max(0, before-now)
			burst.totalDrop += drop
			if 0 < before && settings.CohortDropShare*before <= drop && settings.CohortMinDrop <= drop {
				struckDrop += drop
				struckBefore += before
				burst.struck = append(burst.struck, fmt.Sprintf("%s:%.0f/%.0f", label, drop, before))
				if topDrop < drop {
					topDrop, burst.topCohort = drop, label
				}
			} else {
				restBefore += before
				restDrop += drop
			}
		}
		if 0 < restBefore {
			burst.restShare = restDrop / restBefore
		}
		switch {
		case len(burst.reasons) != 0:
		case !websocketBurst || burst.h1PlusBurst:
			burst.reasons = append(burst.reasons, "carriers_spread")
		case 0 < burst.restarted:
			burst.reasons = append(burst.reasons, "connect_restarted")
		case len(burst.struck) == 0:
			burst.reasons = append(burst.reasons, "no_struck_cohort")
		case struckDrop < settings.ConfinedShare*burst.totalDrop || settings.RestDropShare <= burst.restShare ||
			settings.MaxStruckShare*(struckBefore+restBefore) < struckBefore:
			burst.reasons = append(burst.reasons, "drop_spread_across_cohorts")
		default:
			burst.confined = true
		}
		bursts = append(bursts, burst)
	}
	if 0 < unknownBurstSteps {
		reasons = append(reasons, "baseline_short")
	}
	missingCohortSteps := 0
	for _, burst := range bursts {
		for _, reason := range burst.reasons {
			if reason == "cohort_points_missing" {
				missingCohortSteps++
			}
		}
	}
	if 0 < missingCohortSteps {
		reasons = append(reasons, "cohort_points_missing")
	}
	listedCohorts := func(values []string) string {
		if len(values) == 0 {
			return "none"
		}
		if len(values) <= settings.MaxListed {
			return strings.Join(values, ",")
		}
		return strings.Join(values[:settings.MaxListed], ",") + fmt.Sprintf(",and %d more", len(values)-settings.MaxListed)
	}
	describe := func(burst connectCloseBurst) string {
		keys := []string{}
		for key := range burst.closes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		closes := []string{}
		for _, key := range keys {
			median := "unknown"
			if value, known := medians[key]; known {
				median = strconv.FormatFloat(value, 'f', 0, 64)
			}
			closes = append(closes, fmt.Sprintf("%s=%.0f(median=%s)", key, burst.closes[key], median))
		}
		reasonText := "none"
		if len(burst.reasons) != 0 {
			reasonText = strings.Join(burst.reasons, ",")
		}
		return fmt.Sprintf("burst_minute=%s closes=%s struck_cohorts=%s cohort_drop=%.0f rest_of_fleet_drop_share=%.3f restarted_slots=%d spread_reasons=%s",
			time.Unix(burst.step, 0).UTC().Format(time.RFC3339), strings.Join(closes, ","), listedCohorts(burst.struck), burst.totalDrop, burst.restShare, burst.restarted, reasonText)
	}
	cohortMinutes, spreadMinutes := 0, 0
	var largestCohort, largestSpread *connectCloseBurst
	for i := range bursts {
		burst := &bursts[i]
		if burst.confined {
			cohortMinutes++
			if largestCohort == nil || largestCohort.burstCloses < burst.burstCloses {
				largestCohort = burst
			}
		} else if len(burst.reasons) != 1 || burst.reasons[0] != "cohort_points_missing" {
			spreadMinutes++
			if largestSpread == nil || largestSpread.burstCloses < burst.burstCloses {
				largestSpread = burst
			}
		}
	}
	summary := fmt.Sprintf("lookback=%s burst_minutes=%d cohort_minutes=%d spread_minutes=%d %s metrics_gateway=%s",
		settings.Lookback, len(bursts), cohortMinutes, spreadMinutes, coverage.observed(settings.MaxListed), gatewayName)

	findings := []finding{}
	if 0 < len(reasons) {
		findings = append(findings, connectCloseUnknown(target, "visibility_reason="+strings.Join(reasons, ";")+" "+summary))
	}
	baseline := fmt.Sprintf("No carrier and reason closes at least %.0f in one minute and %.0fx its %s median. On 2026-10-13 the baseline was about 350 websocket read_timeout closes a minute; Connect's transport read timeout is 30 s, so a read_timeout close means the client sent nothing for 30 s.", settings.BurstMinimum, settings.BurstMultiple, settings.Lookback)
	if largestCohort != nil {
		findings = append(findings, finding{
			probeId: connectCloseCohortsProbeId, tier: tierWarn, class: "connect-close-burst-cohort",
			target: target, frame: largestCohort.topCohort, sustain: 1,
			symptom:   fmt.Sprintf("A burst of websocket H1 closes was confined to provider release cohort %s while h1plus stayed flat", largestCohort.topCohort),
			mechanism: "The burst came on websocket alone, the matching connection drop sat in a few release cohorts while the rest of the fleet held, and no Connect process restarted. After the 2026-10-13 mass reconnect the same pattern recurred cohort by cohort (3,564 closes at 07:12Z from the other bucket plus 76% of 2026.9.9; 1,642 at 07:42Z from 2026.3; 1,430 at 07:47Z from 2026.7): most likely a client-side per-version timer firing in lockstep after everyone reconnected together.",
			baseline:  baseline,
			observed:  describe(*largestCohort) + " " + summary,
			evidence:  "One Mimir range read at one-minute steps: closes by carrier and reason, provider connections by app release, and per-slot Connect start, close and cohort freshness. The cohort join is by time: the close counter has no version label, and other mixes unrecognized versions, so one bucket can hide several cohorts.",
			context:   "The healthy control is the h1plus carrier in the same minute. A cohort-confined burst with h1plus flat and steady edge NIC receive is this client pattern, not a server or network fault. The gauge is per process and restarts with it.",
			action:    "No server action is indicated: do not restart Connect for it. Check edge NIC receive for the same minute, then the client code of the named release for a timer that fires after reconnect.",
			verify:    "The release cohort reconnects within minutes and later lookbacks show no repeat; a repeat on a fixed period is the client timer to fix in that release.",
			playbook:  "SIGNALS.md §23.3",
		})
	}
	if largestSpread != nil {
		findings = append(findings, finding{
			probeId: connectCloseCohortsProbeId, tier: tierWarn, class: "connect-close-burst-spread",
			target: target, sustain: 1,
			symptom:   "A burst of Connect H1 session closes was not confined to one carrier and a few release cohorts",
			mechanism: "Closes rose on both carriers, the connection drop spread across release cohorts, no cohort was struck, or a Connect process restarted in the minute. That is not the client per-version pattern: a load balancer reload, a network dip, a Connect restart or drain, or a database stall can close sessions across every cohort at once.",
			baseline:  baseline,
			observed:  describe(*largestSpread) + " " + summary,
			evidence:  "One Mimir range read at one-minute steps: closes by carrier and reason, provider connections by app release, per-slot freshness, and each slot's newest process start. Restart means a slot's newest process started within the restart window of the burst minute.",
			context:   "A restart wave drops every cohort together (on 2026-10-13 providers fell from 71.3k to about 1.9k and back within six minutes); edge NIC receive is the network discriminator. Provider supply itself is owned by §1.1a.",
			action:    "Check the LB, network and Connect path for the burst minute (§2.7, §8.6), the rollout and restart state (§8.2), and §1.1a provider supply. Do not attribute it to a client release without a confined cohort drop.",
			verify:    "Later lookbacks show closes back within their median band on both carriers and provider connections recovered across every cohort.",
			playbook:  "SIGNALS.md §23.3, §2.7 and §8.6",
		})
	}
	if len(reasons) == 0 {
		if largestCohort == nil {
			findings = append(findings, healthyFinding(connectCloseCohortsProbeId, tierWarn, "connect-close-burst-cohort", target))
		}
		if largestSpread == nil {
			findings = append(findings, healthyFinding(connectCloseCohortsProbeId, tierWarn, "connect-close-burst-spread", target))
		}
	}
	return findings
}

// Missing, stale or ambiguous telemetry is never a quiet close rate.
func connectCloseUnknown(target string, observed string) finding {
	return finding{
		probeId: connectCloseCohortsProbeId, tier: tierWarn, class: "connect-close-telemetry-unobservable",
		target: target, sustain: 2,
		symptom:   "Connect H1 close bursts and provider cohorts cannot be fully observed",
		mechanism: "The Mimir range read failed, a desired Connect slot lacks fresh process-start, close or cohort series, a carrier and reason crossed the burst floor without enough lookback points to judge it, the cohort series are missing in a burst minute, or a label is outside the server's bounded vocabulary. A burst or its cohort attribution on a missing slot cannot be ruled out.",
		baseline:  "Every desired Connect slot exports fresh process-start, close and provider-cohort series, and every carrier and reason has a full lookback.",
		observed:  observed,
		evidence:  "Desired slots come from active services.yml placement, not from surviving series. A burst proven on the observed slots is reported beside this, never instead of it.",
		context:   "This is telemetry uncertainty, not evidence that sessions are stable.",
		action:    "Restore the Connect metrics or the Mimir path, allow a replaced Connect process one full lookback, and rerun.",
		verify:    "Every desired Connect slot reports every family for a full lookback on two cadences.",
		playbook:  "SIGNALS.md §23.3",
	}
}
