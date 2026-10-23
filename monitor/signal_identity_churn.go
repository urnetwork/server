package monitor

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SIGNALS.md §14.8 maps to signal_identity_churn.go and
// signal_identity_churn_test.go. A hosted proxy device mints a new
// network-client identity for every candidate exit and retires it with
// RemoveNetworkClient; when discovery fails or the database queues, windows
// cancel unproven candidates and mint replacements, and the loop outlives its
// trigger while filling edge-2 and MinIO. The signal reads the proxy fleet's
// hosted mint and retire state queries, admission refusals, active devices
// and primary discovery outcomes from Mimir, with the per-device baseline and
// the 30-minute hold taken from Mimir history, and samples the lifetime of
// one minute of hosted identities from PostgreSQL ten minutes later. Only
// counts, rates, shares and inventory slot names leave the sources: never a
// client, network or device id.
func NewIdentityChurnSignal() Signal {
	return NewIdentityChurnSignalWithSettings(DefaultIdentityChurnSettings())
}

// The signal with explicit thresholds.
func NewIdentityChurnSignalWithSettings(settings *IdentityChurnSettings) Signal {
	return &signalAdapter{
		number: "14.8", key: "identity-churn", name: "Hosted identity churn",
		probe: identityChurnProbe{settings: settings},
	}
}

const (
	identityChurnProbeId         = "mimir/identity-churn"
	identityStateQueryMetric     = "urnetwork_jwt_state_queries_total"
	identityRefusalMetric        = "urnetwork_hosted_mint_admission_refusals_total"
	identityDevicesMetric        = "urnetwork_proxy_devices_active"
	identityDiscoveryMetric      = "urnetwork_findproviders2_native_source_outcomes_total"
	identityDiscoverySchema      = "urnetwork_findproviders2_native_source_schema_version"
	identityDiscoveryQuota       = "quota"
	identityDiscoveryExhausted   = "exhausted"
	identityDiscoveryUnavailable = "unavailable"
)

// The thresholds and bounds of §14.8.
type IdentityChurnSettings struct {
	// rate window of mints, retires and discovery outcomes
	RateWindow time.Duration
	// rate window of hosted mint refusals
	RefusalWindow time.Duration
	// mints per active device-minute must stay over PerDeviceMultiple times
	// its BaselineWindow median for PerDeviceHold, read at PerDeviceHoldStep
	PerDeviceHold     time.Duration
	PerDeviceHoldStep time.Duration
	PerDeviceMultiple float64
	// the per-device median over BaselineWindow at BaselineStep needs at
	// least MinBaselinePoints points
	BaselineWindow    time.Duration
	BaselineStep      time.Duration
	MinBaselinePoints int
	// retire/mint ratio over RateWindow while mints exceed RetireRatioMinMints
	// per second
	RetireRatio         float64
	RetireRatioMinMints float64
	// hosted mint refusals per second
	RefusalRate float64
	// share of primary discovery outcomes that are exhausted or unavailable,
	// judged once outcomes reach DiscoveryMinOutcomes per second
	DiscoveryUnresolvedShare float64
	DiscoveryMinOutcomes     float64
	// the loop is engaged when churn meets queued PgBouncer clients above
	// LoopPgBouncerWaiting or a contract mean above LoopContractMean over
	// LoopContractWindow (§2.30)
	LoopPgBouncerWaiting float64
	LoopContractMean     time.Duration
	LoopContractWindow   time.Duration
	// the lifetime sample: hosted identities created in LifetimeSpan ending
	// LifetimeAge ago, judged when at least LifetimeMinIdentities were made
	LifetimeAge           time.Duration
	LifetimeSpan          time.Duration
	LifetimeMinIdentities int
	// short-lived: under this share still alive, and at least
	// LifetimeNeverConnectedShare retired without ever connecting
	LifetimeAliveShare          float64
	LifetimeNeverConnectedShare float64
	// the most identities one lifetime sample reads
	LifetimeMaxIdentities int
	// the proxy service whose slots must report every family
	Service string
	// the one fleet read, and every family's freshness on every slot
	FleetReadSettings
	// consecutive cadences before a churn or loop alert opens
	Sustain int
	// the most slot gaps one alert lists
	MaxListed int
}

// The defaults of SIGNALS.md §14.8.
func DefaultIdentityChurnSettings() *IdentityChurnSettings {
	return &IdentityChurnSettings{
		RateWindow:                  30 * time.Minute,
		RefusalWindow:               15 * time.Minute,
		PerDeviceHold:               30 * time.Minute,
		PerDeviceHoldStep:           5 * time.Minute,
		PerDeviceMultiple:           2,
		BaselineWindow:              7 * 24 * time.Hour,
		BaselineStep:                time.Hour,
		MinBaselinePoints:           84,
		RetireRatio:                 0.8,
		RetireRatioMinMints:         150,
		RefusalRate:                 100,
		DiscoveryUnresolvedShare:    0.3,
		DiscoveryMinOutcomes:        1,
		LoopPgBouncerWaiting:        500,
		LoopContractMean:            time.Second,
		LoopContractWindow:          5 * time.Minute,
		LifetimeAge:                 10 * time.Minute,
		LifetimeSpan:                time.Minute,
		LifetimeMinIdentities:       3000,
		LifetimeAliveShare:          0.5,
		LifetimeNeverConnectedShare: 0.5,
		LifetimeMaxIdentities:       200000,
		Service:                     "proxy",
		FleetReadSettings:           DefaultFleetReadSettings(),
		Sustain:                     2,
		MaxListed:                   8,
	}
}

type identityChurnProbe struct {
	settings *IdentityChurnSettings
}

func (identityChurnProbe) id() string             { return identityChurnProbeId }
func (identityChurnProbe) tier() string           { return tierWarn }
func (identityChurnProbe) cadence() time.Duration { return 5 * time.Minute }

func identityChurnQuery(environment string, settings *IdentityChurnSettings, inventory fleetSlotInventory) string {
	base := "env=" + strconv.Quote(environment) + ",job=" + strconv.Quote(settings.Service)
	window := promDuration(settings.RateWindow)
	mint := identityStateQueryMetric + "{" + base + `,caller="hosted",operation="mint"}`
	retire := identityStateQueryMetric + "{" + base + `,caller="hosted",operation="retire"}`
	devices := identityDevicesMetric + "{" + base + "}"
	perDevice := "60 * sum(rate(" + mint + "[" + window + "])) / sum(" + devices + ")"
	hold := "(" + perDevice + ")[" + promDuration(settings.PerDeviceHold) + ":" + promDuration(settings.PerDeviceHoldStep) + "]"
	baseline := "(" + perDevice + ")[" + promDuration(settings.BaselineWindow) + ":" + promDuration(settings.BaselineStep) + "]"
	parts := []string{
		fleetPart("mints", "sum(rate("+mint+"["+window+"]))"),
		fleetPart("retires", "sum(rate("+retire+"["+window+"]))"),
		fleetPart("refusals", "sum(rate("+identityRefusalMetric+"{"+base+"}["+promDuration(settings.RefusalWindow)+"]))"),
		fleetPart("devices", "sum("+devices+")"),
		fleetPart("per_device", perDevice),
		fleetPart("per_device_hold", "min_over_time("+hold+")"),
		fleetPart("per_device_hold_points", "count_over_time("+hold+")"),
		fleetPart("per_device_hold_expected", "count_over_time(vector(1)["+promDuration(settings.PerDeviceHold)+":"+promDuration(settings.PerDeviceHoldStep)+"])"),
		fleetPart("per_device_median", "quantile_over_time(0.5, "+baseline+")"),
		fleetPart("per_device_median_points", "count_over_time("+baseline+")"),
		fleetPart("discovery", "sum by (outcome) (rate("+identityDiscoveryMetric+"{"+base+`,source="primary"}[`+window+"]))"),
		fleetPart("pgbouncer_waiting", pgbouncerWaitingExpression(environment, settings.Freshness)),
		fleetPart("contract_mean", contractStageMeanExpression(environment, settings.LoopContractWindow)),
	}
	if selector := inventory.selector(environment); selector != "" {
		parts = append(parts,
			fleetSlotFreshnessPart("devices", identityDevicesMetric, selector),
			fleetSlotFreshnessPart("mints", identityStateQueryMetric, selector+`,caller="hosted",operation="mint"`),
			fleetSlotFreshnessPart("refusals", identityRefusalMetric, selector),
			fleetSlotFreshnessPart("discovery", identityDiscoverySchema, selector),
		)
	}
	return strings.Join(parts, " or ")
}

// Hosted identities are the derived clients of a proxy device's client.
// Client ids lead with their creation millisecond, so one minute is a primary
// key range; a connection row proves the identity connected. Aggregates only.
func identityLifetimeQuery(settings *IdentityChurnSettings) string {
	start := int64((settings.LifetimeAge + settings.LifetimeSpan) / time.Second)
	end := int64(settings.LifetimeAge / time.Second)
	return fmt.Sprintf(`/* monitor-identity-churn-lifetime */
WITH bounds AS (
  SELECT
    (lpad(to_hex((extract(epoch FROM now() - interval '%d seconds') * 1000)::bigint), 12, '0') || '00000000000000000000')::uuid AS lo,
    (lpad(to_hex((extract(epoch FROM now() - interval '%d seconds') * 1000)::bigint), 12, '0') || '00000000000000000000')::uuid AS hi
), sampled AS (
  SELECT
    n.active,
    n.deactivate_time - n.create_time AS life,
    NOT EXISTS (SELECT 1 FROM network_client_connection cc WHERE cc.client_id = n.client_id) AS never_connected
  FROM network_client n, bounds
  WHERE n.client_id >= bounds.lo AND n.client_id < bounds.hi
    AND n.source_client_id IS NOT NULL
    AND EXISTS (SELECT 1 FROM proxy_device_config p WHERE p.client_id = n.source_client_id)
  LIMIT %d
)
SELECT
  count(*),
  count(*) FILTER (WHERE active),
  count(*) FILTER (WHERE NOT active AND never_connected),
  count(*) FILTER (WHERE never_connected),
  COALESCE(round(extract(epoch FROM percentile_disc(0.5) WITHIN GROUP (ORDER BY life) FILTER (WHERE NOT active AND life IS NOT NULL)))::bigint, -1)
FROM sampled;
`, start, end, settings.LifetimeMaxIdentities+1)
}

// One lifetime sample of hosted identities.
type identityLifetime struct {
	capped                bool
	identities            int64
	alive                 int64
	retiredNeverConnected int64
	neverConnected        int64
	medianLifeSeconds     int64
}

func readIdentityLifetime(ctx context.Context, env *probeEnv, settings *IdentityChurnSettings) (identityLifetime, error) {
	rows, err := env.runner.pg(ctx, identityLifetimeQuery(settings))
	if err != nil {
		return identityLifetime{}, err
	}
	if len(rows) != 1 || len(rows[0]) != 5 {
		return identityLifetime{}, fmt.Errorf("identity lifetime sample has an invalid shape")
	}
	values := [5]int64{}
	for i := range values {
		value, err := strconv.ParseInt(rows[0].str(i), 10, 64)
		if err != nil {
			return identityLifetime{}, fmt.Errorf("identity lifetime sample has an invalid value")
		}
		values[i] = value
	}
	lifetime := identityLifetime{
		identities: values[0], alive: values[1], retiredNeverConnected: values[2],
		neverConnected: values[3], medianLifeSeconds: values[4],
	}
	if lifetime.identities < 0 || lifetime.alive < 0 || lifetime.retiredNeverConnected < 0 || lifetime.neverConnected < 0 ||
		lifetime.identities < lifetime.alive || lifetime.identities < lifetime.neverConnected ||
		lifetime.neverConnected < lifetime.retiredNeverConnected || lifetime.identities-lifetime.alive < lifetime.retiredNeverConnected {
		return identityLifetime{}, fmt.Errorf("identity lifetime sample is inconsistent")
	}
	lifetime.capped = int64(settings.LifetimeMaxIdentities) < lifetime.identities
	return lifetime, nil
}

// The parsed fleet read. Absent scalars stay NaN.
type identityChurnObservation struct {
	scalars        map[string]float64
	discovery      map[string]float64
	invalidReasons []string
}

var identityChurnScalarParts = []string{
	"mints", "retires", "refusals", "devices", "per_device", "per_device_hold", "per_device_hold_points",
	"per_device_hold_expected", "per_device_median", "per_device_median_points", "pgbouncer_waiting", "contract_mean",
}

func newIdentityChurnObservation(samples []fleetSample) *identityChurnObservation {
	observation := &identityChurnObservation{scalars: map[string]float64{}, discovery: map[string]float64{}}
	known := map[string]bool{}
	for _, part := range identityChurnScalarParts {
		observation.scalars[part] = math.NaN()
		known[part] = true
	}
	reasons := map[string]bool{}
	seen := map[string]bool{}
	for _, sample := range samples {
		switch {
		case strings.HasPrefix(sample.part, "fresh/"):
		case sample.part == "discovery":
			outcome := sample.labels["outcome"]
			if len(sample.labels) != 1 || (outcome != identityDiscoveryQuota && outcome != identityDiscoveryExhausted && outcome != identityDiscoveryUnavailable) {
				reasons["unrecognized_discovery_outcome"] = true
				continue
			}
			if _, duplicate := observation.discovery[outcome]; duplicate {
				reasons["duplicate_series"] = true
			}
			if math.IsNaN(sample.value) || math.IsInf(sample.value, 0) || sample.value < 0 {
				reasons["invalid_value"] = true
				continue
			}
			observation.discovery[outcome] = sample.value
		case known[sample.part]:
			if len(sample.labels) != 0 {
				reasons["unrecognized_part"] = true
				continue
			}
			if seen[sample.part] {
				reasons["duplicate_series"] = true
			}
			seen[sample.part] = true
			observation.scalars[sample.part] = sample.value
		default:
			reasons["unrecognized_part"] = true
		}
	}
	for reason := range reasons {
		observation.invalidReasons = append(observation.invalidReasons, reason)
	}
	sort.Strings(observation.invalidReasons)
	return observation
}

func (self identityChurnProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultIdentityChurnSettings()
	}
	target := env.cfg.env + "/hosted-identities"
	at := fleetEvaluationTime(env.now(), settings.EvaluationLag)
	inventory := newFleetSlotInventory(env, []string{settings.Service}, nil)
	var observation *identityChurnObservation
	var coverage fleetSlotCoverage
	gatewayName := "none"
	sourceReason := ""
	out, gateway, err := readFleetMimir(ctx, env, identityChurnQuery(env.cfg.env, settings, inventory), at, settings.FleetReadSettings)
	if err != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		sourceReason = "metrics_unavailable " + observationFailureDetails(err, classifyObservationError(err))
	} else {
		gatewayName = gateway.name
		samples, err := parseFleetSamples(out, at, settings.FleetReadSettings)
		if err != nil {
			sourceReason = "metrics_invalid_response"
		} else {
			observation = newIdentityChurnObservation(samples)
			coverage = newFleetSlotCoverage(inventory, samples, []string{"devices", "mints", "refusals", "discovery"}, at, settings.FleetReadSettings, nil)
		}
	}
	// the lifetime sample is independent: a failed read leaves only it unknown
	lifetime, lifetimeErr := readIdentityLifetime(ctx, env, settings)
	if lifetimeErr != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
	}
	return evaluateIdentityChurn(settings, target, gatewayName, sourceReason, observation, coverage, lifetime, lifetimeErr), nil
}

func evaluateIdentityChurn(
	settings *IdentityChurnSettings,
	target string,
	gatewayName string,
	sourceReason string,
	observation *identityChurnObservation,
	coverage fleetSlotCoverage,
	lifetime identityLifetime,
	lifetimeErr error,
) []finding {
	reasons := []string{}
	if sourceReason != "" {
		reasons = append(reasons, sourceReason)
	}
	finite := func(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
	format := func(value float64, precision int) string {
		if !finite(value) {
			return "unknown"
		}
		return strconv.FormatFloat(value, 'f', precision, 64)
	}
	criteria := []string{}
	observedParts := []string{}
	if observation != nil {
		reasons = append(reasons, observation.invalidReasons...)
		if !coverage.full() {
			reasons = append(reasons, "slot_coverage_incomplete")
		}
		value := observation.scalars
		// mints per active device-minute against its own weekly median
		holdComplete := finite(value["per_device_hold_points"]) && finite(value["per_device_hold_expected"]) &&
			0 < value["per_device_hold_expected"] && value["per_device_hold_expected"] <= value["per_device_hold_points"]
		baselineComplete := finite(value["per_device_median_points"]) && float64(settings.MinBaselinePoints) <= value["per_device_median_points"] &&
			finite(value["per_device_median"]) && 0 < value["per_device_median"]
		switch {
		case !holdComplete || !finite(value["per_device_hold"]):
			reasons = append(reasons, "per_device_hold_incomplete")
		case !baselineComplete:
			reasons = append(reasons, "per_device_baseline_short")
		case settings.PerDeviceMultiple*value["per_device_median"] < value["per_device_hold"]:
			criteria = append(criteria, "per_device_mints")
		}
		// retire/mint ratio while mints are high
		switch {
		case !finite(value["mints"]) || !finite(value["retires"]):
			reasons = append(reasons, "mint_rate_unknown")
		case settings.RetireRatioMinMints < value["mints"] && settings.RetireRatio < value["retires"]/value["mints"]:
			criteria = append(criteria, "retire_ratio")
		}
		switch {
		case !finite(value["refusals"]):
			reasons = append(reasons, "refusal_rate_unknown")
		case settings.RefusalRate < value["refusals"]:
			criteria = append(criteria, "mint_refusals")
		}
		discoveryTotal := 0.0
		for _, rate := range observation.discovery {
			discoveryTotal += rate
		}
		unresolved := observation.discovery[identityDiscoveryExhausted] + observation.discovery[identityDiscoveryUnavailable]
		discoveryShare := math.NaN()
		if 0 < discoveryTotal {
			discoveryShare = unresolved / discoveryTotal
		}
		if settings.DiscoveryMinOutcomes <= discoveryTotal && settings.DiscoveryUnresolvedShare < discoveryShare {
			criteria = append(criteria, "discovery_unresolved")
		}
		retireRatio := math.NaN()
		if finite(value["mints"]) && 0 < value["mints"] {
			retireRatio = value["retires"] / value["mints"]
		}
		observedParts = append(observedParts, fmt.Sprintf(
			"mints_per_s=%s retires_per_s=%s retire_mint_ratio=%s refusals_per_s=%s active_devices=%s mints_per_device_minute=%s held_min_%s=%s weekly_median=%s median_points=%s discovery_primary_per_s=%s unresolved_share=%s discovery_exhausted_per_s=%s discovery_unavailable_per_s=%s pgbouncer_clients_waiting=%s contract_mean_ms=%s",
			format(value["mints"], 1), format(value["retires"], 1), format(retireRatio, 2), format(value["refusals"], 1),
			format(value["devices"], 0), format(value["per_device"], 2), settings.PerDeviceHold, format(value["per_device_hold"], 2),
			format(value["per_device_median"], 2), format(value["per_device_median_points"], 0),
			format(discoveryTotal, 1), format(discoveryShare, 2), format(observation.discovery[identityDiscoveryExhausted], 1),
			format(observation.discovery[identityDiscoveryUnavailable], 1), format(value["pgbouncer_waiting"], 0), format(1000*value["contract_mean"], 1),
		))
		observedParts = append(observedParts, coverage.observed(settings.MaxListed))
	}
	if lifetimeErr != nil {
		reasons = append(reasons, "lifetime_unavailable")
		observedParts = append(observedParts, "lifetime=unknown "+observationFailureDetails(lifetimeErr, classifyObservationError(lifetimeErr)))
	} else {
		share := func(count int64) string {
			if lifetime.identities == 0 {
				return "unknown"
			}
			return strconv.FormatFloat(100*float64(count)/float64(lifetime.identities), 'f', 1, 64) + "%"
		}
		median := "unknown"
		if 0 <= lifetime.medianLifeSeconds {
			median = strconv.FormatInt(lifetime.medianLifeSeconds, 10) + "s"
		}
		observedParts = append(observedParts, fmt.Sprintf(
			"lifetime_identities=%d lifetime_capped=%t alive_after_%s=%s retired_never_connected=%s never_connected=%s retired_median_life=%s",
			lifetime.identities, lifetime.capped, settings.LifetimeAge, share(lifetime.alive), share(lifetime.retiredNeverConnected),
			share(lifetime.neverConnected), median,
		))
		if int64(settings.LifetimeMinIdentities) <= lifetime.identities &&
			float64(lifetime.alive) < settings.LifetimeAliveShare*float64(lifetime.identities) &&
			settings.LifetimeNeverConnectedShare*float64(lifetime.identities) <= float64(lifetime.retiredNeverConnected) {
			criteria = append(criteria, "short_lived")
		}
	}
	observedParts = append(observedParts, "metrics_gateway="+gatewayName)
	observed := strings.Join(observedParts, " ")

	// The loop needs the database side as well as the churn. Each side is
	// engaged by its own evidence; not engaged needs both read below the band.
	loopEngaged, loopRuledOut := false, false
	if observation != nil {
		waiting, mean := observation.scalars["pgbouncer_waiting"], observation.scalars["contract_mean"]
		loopEngaged = (finite(waiting) && settings.LoopPgBouncerWaiting < waiting) ||
			(!math.IsNaN(mean) && settings.LoopContractMean.Seconds() < mean)
		loopRuledOut = !loopEngaged && finite(waiting) && finite(mean)
	}
	if 0 < len(criteria) && !loopEngaged && !loopRuledOut {
		reasons = append(reasons, "loop_context_unknown")
	}

	findings := []finding{}
	if 0 < len(reasons) {
		findings = append(findings, identityChurnUnknown(target, "visibility_reason="+strings.Join(reasons, ";")+" "+observed))
	}
	if len(criteria) == 0 {
		if len(reasons) == 0 {
			findings = append(findings,
				healthyFinding(identityChurnProbeId, tierWarn, "identity-churn", target),
				healthyFinding(identityChurnProbeId, tierPage, "identity-churn-loop", target),
			)
		}
		return findings
	}
	criteriaText := strings.Join(criteria, ",")
	contextText := "A fleet rollout recreates windows for its duration, and a growing hosted fleet raises mints without raising per-device churn: the per-device rate against its own weekly median separates the two. The admission bound caps mints, so read refusals beside them. Taskworker and api mints (probes, apps) are separate jobs and are not counted. Once running, the loop outlived its trigger on 2026-10-22: churn stayed at 240-310 mints/s after the gate was postponed."
	action := "Break the loop on the database side (§2.30, §5.6, §5.12) or the client side: discovery pacing, a lower hosted mint cap, racing one candidate for hosted windows, keeping the 429 backoff across a successful discovery, and reusing identities that never connected. Compare the running proxy and SDK artifacts with those repairs before proposing a rollout. Do not restart proxies fleet-wide: every restart recreates windows."
	findings = append(findings, finding{
		probeId: identityChurnProbeId, tier: tierWarn, class: "identity-churn",
		target: target, sustain: settings.Sustain,
		symptom:   "Hosted proxy windows are minting and retiring network-client identities faster than they live (" + criteriaText + ")",
		mechanism: "Every candidate exit of a hosted multi-client window is a newly minted identity, and each one that registers its key writes an immutable client-key history row, two MinIO evidence objects, contracts and close reports. When discovery fails or slows, windows cancel candidates that have not proven themselves and mint replacements, so churn is storage growth on edge-2 and MinIO at once and extra database work for every replacement.",
		baseline:  fmt.Sprintf("On 2026-10-15: about 57 mints/s and 8 retires/s, about 0.5 mints per active device-minute, no hosted mint refusals, and identities that lived. Bands: per-device mints over %.0fx their weekly median held %s; retire/mint over %.1f while mints exceed %.0f/s; refusals over %.0f/s; exhausted plus unavailable over %.0f%% of primary discovery outcomes; or a sampled minute with under %.0f%% alive after %s and at least %.0f%% retired without connecting.", settings.PerDeviceMultiple, settings.PerDeviceHold, settings.RetireRatio, settings.RetireRatioMinMints, settings.RefusalRate, 100*settings.DiscoveryUnresolvedShare, 100*settings.LifetimeAliveShare, settings.LifetimeAge, 100*settings.LifetimeNeverConnectedShare),
		observed:  "criteria=" + criteriaText + " " + observed,
		evidence:  "One Mimir instant query evaluated behind now over the proxy fleet (job=proxy): hosted mint and retire JWT-state queries, admission refusals, active devices, primary native discovery outcomes, the per-device hold and weekly median from Mimir history, and per-slot freshness. One read-only PostgreSQL aggregate over a one-minute primary-key range of hosted derived identities. Discovery label trap: quota is success; exhausted means too few candidates and unavailable means an incomplete source or the 250 ms quality-validation budget ran out.",
		context:   contextText,
		action:    action,
		verify:    "Per-device mints return under twice their weekly median, retire/mint falls under the band, refusals return to 0 and sampled identities live (most alive after ten minutes) for two cadences, with PgBouncer queued clients at 0.",
		playbook:  "SIGNALS.md §14.8",
	})
	switch {
	case loopEngaged:
		findings = append(findings, finding{
			probeId: identityChurnProbeId, tier: tierPage, class: "identity-churn-loop",
			target: target, sustain: settings.Sustain,
			symptom:   "The hosted identity-churn loop is engaged: churn (" + criteriaText + ") together with database queueing",
			mechanism: "Discovery fails or slows, windows cancel unproven candidates and mint replacements (EvaluationPoolMultiple 2 also mints twice the candidates needed), every replacement adds database work and PgBouncer queueing, connects and contracts slow down, and more candidates are cut. Once running the loop outlives its trigger.",
			baseline:  fmt.Sprintf("No churn criterion holds, or PgBouncer queued clients stay at or under %.0f and the contract-creation mean at or under %s (§2.30).", settings.LoopPgBouncerWaiting, settings.LoopContractMean),
			observed:  "criteria=" + criteriaText + " " + observed,
			evidence:  "The churn criteria above joined with fresh queued PgBouncer clients (job=pgbouncer) and the five-minute per-reply contract-creation mean from the same Mimir read.",
			context:   contextText + " On 2026-10-21/22 PgBouncer queued from 14:00Z and the churn filled edge-2's root filesystem at up to 20 GB/h; watch §2.4b and §11.6a runway beside this.",
			action:    action,
			verify:    "Both the churn criteria and the database side clear for two cadences: PgBouncer queued clients 0, the §2.30 mean back under 250 ms, and storage growth on edge-2 and MinIO back to its baseline slope.",
			playbook:  "SIGNALS.md §14.8 and §5.12",
		})
	case loopRuledOut:
		findings = append(findings, healthyFinding(identityChurnProbeId, tierPage, "identity-churn-loop", target))
	}
	return findings
}

// An unavailable, partial or short baseline is never quiet churn.
func identityChurnUnknown(target string, observed string) finding {
	return finding{
		probeId: identityChurnProbeId, tier: tierWarn, class: "identity-churn-unobservable",
		target: target, sustain: 2,
		symptom:   "Hosted identity churn cannot be fully established",
		mechanism: "The Mimir read or the PostgreSQL lifetime sample failed, a desired proxy slot lacks a fresh devices, hosted-mint, refusal or discovery-schema series, the per-device hold has missing points, the weekly baseline is too short, or churn is proven while the database side of the loop is unread. Churn on a missing slot or in an unread minute cannot be ruled out.",
		baseline:  "Every desired proxy slot exports all four families within the freshness bound, the 30-minute hold is complete, the weekly median has at least half its points, and the lifetime sample reads.",
		observed:  observed,
		evidence:  "Desired slots come from active services.yml placement, not from surviving series. Criteria proven on the observed slots are reported beside this, never instead of it.",
		context:   "This is telemetry, inventory or database-read uncertainty, not evidence that identities live.",
		action:    "Restore the proxy metrics or the Mimir and PostgreSQL paths, allow a replaced proxy one full hold, and rerun.",
		verify:    "Every desired proxy slot reports every family, the hold and baseline are complete, and the lifetime sample reads for two cadences.",
		playbook:  "SIGNALS.md §14.8",
	}
}
