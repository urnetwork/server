package model

// provider_status_model.go — why each of a network's own provider clients is
// or is not offered to clients (GET /network/provider-status).
//
// The status is the decision FindProviders2 draws from: the common gates, the
// egress decision and the ranking UpdateClientScores publishes, computed for
// the network's providers with the bulk filter scoped to their client ids. The
// fleet-wide filter never runs for a status. Ranking numbers are shown with
// what they mean; the anti-abuse signals (ARIN risk and non-quality, the URL
// security quarantine, the legacy dark verdict) are one generic reason, and
// their values never leave this file.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// The first reason a provider is not offered, in the order the selection
// applies its rules. A provider that passes them all is "none".
const (
	ProviderStatusReasonNotProviding         = "not_providing"
	ProviderStatusReasonNotConnected         = "not_connected"
	ProviderStatusReasonLocationInvalid      = "location_invalid"
	ProviderStatusReasonNetworkOnly          = "network_only"
	ProviderStatusReasonReliabilityWarmingUp = "reliability_warming_up"
	ProviderStatusReasonReliabilityLow       = "reliability_low"
	// every anti-abuse signal, never which
	ProviderStatusReasonNotEligible      = "not_eligible"
	ProviderStatusReasonEgressUnprobed   = "egress_unprobed"
	ProviderStatusReasonEgressFailing    = "egress_failing"
	ProviderStatusReasonSpeedTestMissing = "speed_test_missing"
	ProviderStatusReasonSlow             = "slow"
	ProviderStatusReasonNone             = "none"
)

// The reasons in the order they apply.
var ProviderStatusReasons = []string{
	ProviderStatusReasonNotProviding,
	ProviderStatusReasonNotConnected,
	ProviderStatusReasonLocationInvalid,
	ProviderStatusReasonNetworkOnly,
	ProviderStatusReasonReliabilityWarmingUp,
	ProviderStatusReasonReliabilityLow,
	ProviderStatusReasonNotEligible,
	ProviderStatusReasonEgressUnprobed,
	ProviderStatusReasonEgressFailing,
	ProviderStatusReasonSpeedTestMissing,
	ProviderStatusReasonSlow,
	ProviderStatusReasonNone,
}

var providerStatusReasonTexts = map[string]string{
	ProviderStatusReasonNotProviding:         "This device isn't set to share its connection.",
	ProviderStatusReasonNotConnected:         "This device isn't connected right now. Clients are offered only connected devices.",
	ProviderStatusReasonLocationInvalid:      "This device is connecting from more than one address, or its location is unknown. Clients are offered only devices with one address and a known location.",
	ProviderStatusReasonNetworkOnly:          "This device provides only to your own devices. Choose Always to share with everyone.",
	ProviderStatusReasonReliabilityWarmingUp: "Building reliability. This device has been steady for the last hour; clients are offered it once its 12-hour reliability reaches the minimum, about 8 hours after a fresh start.",
	ProviderStatusReasonReliabilityLow:       "Reliability over the last hour is below what clients need. It rises with every minute this device stays connected; disconnects lower it.",
	ProviderStatusReasonNotEligible:          "This connection isn't eligible to provide right now.",
	ProviderStatusReasonEgressUnprobed:       "The network check hasn't run on this connection yet. Until it passes, clients are offered this device only when checked providers run out.",
	ProviderStatusReasonEgressFailing:        "Too many test sites failed to load through this connection in the last 8 hours. Clients are offered this device only when checked providers run out.",
	ProviderStatusReasonSpeedTestMissing:     "No speed test yet. Until one completes, clients are offered this device much less often.",
	ProviderStatusReasonSlow:                 "This connection measured slower than clients need, so it is offered much less often.",
	ProviderStatusReasonNone:                 "Everything checks out. How often clients are offered this device depends on demand in its region and on how it ranks against nearby providers.",
}

// The names of the ranking numbers, in the order a status lists them. A
// reliability lookback outside ClientLookbacks' first three is
// "reliability_lookback_<index>".
const (
	ProviderStatusNumberReliability5m  = "reliability_5m"
	ProviderStatusNumberReliability1h  = "reliability_1h"
	ProviderStatusNumberReliability12h = "reliability_12h"
	ProviderStatusNumberUrlChecks      = "url_checks"
	ProviderStatusNumberSpeedTest      = "speed_test"
	ProviderStatusNumberLatency        = "latency"
	ProviderStatusNumberWeightQuality  = "weight_quality"
	ProviderStatusNumberWeightSpeed    = "weight_speed"
	ProviderStatusNumberTierQuality    = "tier_quality"
	ProviderStatusNumberTierSpeed      = "tier_speed"
)

// The lookback indexes of ClientLookbacks by number, and what each spans.
var providerStatusReliabilityNumbers = []struct {
	lookbackIndex int
	name          string
	span          string
}{
	{lookbackIndex: 0, name: ProviderStatusNumberReliability5m, span: "5 minutes"},
	{lookbackIndex: 1, name: ProviderStatusNumberReliability1h, span: "hour"},
	{lookbackIndex: 2, name: ProviderStatusNumberReliability12h, span: "12 hours"},
}

// The lookback whose floor a steady provider passes first; failing only the
// longer ones is the warm-up.
const providerStatusShortLookbackIndex = 1

// Which FindProviders2 gates a provider passes.
type ProviderAdmission struct {
	// a connected session the selection can see
	Connected bool `json:"connected"`
	// one current address and a known location; the selection pools only
	// such providers
	LocationValid bool `json:"location_valid"`
	// offered to every network, not only the provider's own
	ProvidePublic bool `json:"provide_public"`
	// every reliability lookback at or above its floor
	ReliabilityOk bool `json:"reliability_ok"`
	// a completed speed and latency test
	SpeedTestDone bool `json:"speed_test_done"`
	// the URL check verdict over the evidence window: pass, fail or unprobed
	Egress string `json:"egress"`
	// the latest URL check in the window, absent when unprobed
	EgressMeasuredAt *time.Time `json:"egress_measured_at,omitempty"`
}

const (
	ProviderEgressPass     = "pass"
	ProviderEgressFail     = "fail"
	ProviderEgressUnprobed = "unprobed"
)

// One number the selection reads, with what it means and what raises it. The
// unit follows the name: reliability_* a share of steady uptime (0 to 1),
// url_checks a share of loaded test sites (count of total), speed_test bytes
// per second, latency milliseconds above the expected delay, weight_* the
// relative selection weight, tier_* a tier (0 best, ClientScoreCutoffTier past
// the cutoff).
type ProviderRankingNumber struct {
	Name     string  `json:"name"`
	HasValue bool    `json:"has_value"`
	Value    float64 `json:"value"`
	// the selection's floor, for a number that has one
	HasMinimum bool    `json:"has_minimum"`
	Minimum    float64 `json:"minimum"`
	// the selection's ceiling, for a number that has one
	HasMaximum bool    `json:"has_maximum"`
	Maximum    float64 `json:"maximum"`
	// false when the number holds the provider back
	Passes bool `json:"passes"`
	// for a counted number (url_checks): passed of total
	Count       int    `json:"count"`
	Total       int    `json:"total"`
	Explanation string `json:"explanation"`
}

// Where clients find the provider.
type ProviderStatusCountry struct {
	// the country clients are offered the provider under
	CountryCode string `json:"country_code"`
	Country     string `json:"country"`
	// the country a fresh network check saw the connection exit in, "" for none
	ObservedCountryCode string `json:"observed_country_code"`
	Explanation         string `json:"explanation"`
}

type ProviderStatus struct {
	ClientId server.Id `json:"client_id"`
	// the first reason in ProviderStatusReasons that holds the provider back,
	// "none" for none, and its English text
	Reason     string                   `json:"reason"`
	ReasonText string                   `json:"reason_text"`
	Admission  *ProviderAdmission       `json:"admission"`
	Ranking    []*ProviderRankingNumber `json:"ranking"`
	Country    *ProviderStatusCountry   `json:"country,omitempty"`
	// when the admission and ranking were decided
	EvaluateTime time.Time `json:"evaluate_time"`
	// read fresh on every call, never cached with the rest
	Appearances *ProviderAppearanceHistogram `json:"appearances,omitempty"`
}

// The most provider clients one status answer decides.
const ProviderStatusMaxClients = 32

type ProviderStatusesResult struct {
	Providers []*ProviderStatus `json:"providers"`
	// the network has more provider clients than one answer decides
	Truncated bool `json:"truncated"`
}

// The common SQL of providerCountFilterCommonSql scoped to the client ids in
// $1: the same predicates, so a scoped map holds exactly the fleet map's
// entries for those providers.
func providerCountFilterClientSql() string {
	return `WITH failed_reliability AS MATERIALIZED (
		SELECT DISTINCT observed_reliability.client_id
		FROM client_connection_reliability_score AS observed_reliability
		WHERE observed_reliability.client_id = ANY($1) AND NOT (` + providerReliabilityEligibilitySql("observed_reliability.client_id") + `)
	)
	SELECT client_id, arin_risk, arin_non_quality, false
	FROM network_client_location_reliability
	WHERE client_id = ANY($1) AND (arin_risk OR arin_non_quality)
	UNION ALL
	SELECT failed_reliability.client_id, false, false, true
	FROM failed_reliability
	WHERE EXISTS (
		SELECT 1 FROM network_client_location_reliability AS provider_location
		WHERE provider_location.client_id = failed_reliability.client_id
	)`
}

// newProviderCountFilter for only clientIds: every map holds what the fleet
// filter holds for those providers and nothing for any other.
func newProviderCountFilterForClients(ctx context.Context, clientIds []server.Id) providerCountFilter {
	f := providerCountFilter{
		now:                     server.NowUtc(),
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: getProviderEgressTLSAuthenticationFailedClientIds(ctx, clientIds),
		countryCodes:            getProviderEgressCountryCodes(ctx, clientIds),
	}
	f.healthCounts, f.healthWindowEnd = getProviderEgressHealthCountsSnapshot(ctx, clientIds)
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, providerCountFilterClientSql(), clientIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				var risk, nonQuality, reliabilityFailed bool
				server.Raise(rows.Scan(&clientId, &risk, &nonQuality, &reliabilityFailed))
				if risk {
					f.arinRisk[clientId] = true
				}
				if nonQuality {
					f.arinNonQuality[clientId] = true
				}
				if reliabilityFailed {
					f.reliabilityFailed[clientId] = true
				}
			}
		})
	})
	return f
}

type providerStatusLookback struct {
	reliabilityWeight            float64
	independentReliabilityWeight float64
}

// What the selection reads about one provider client: its pool row, provide
// keys and reliability lookbacks.
type providerStatusFacts struct {
	clientId       server.Id
	networkId      server.Id
	providePublic  bool
	provideNetwork bool
	// the provider has a location rollup row
	hasRollup                bool
	connected                bool
	valid                    bool
	netTypeScore             int
	netTypeScoreSpeed        int
	minRelativeLatencyMillis int
	maxBytesPerSecond        ByteCount
	hasLatencyTest           bool
	hasSpeedTest             bool
	egressIndex              *int
	egressQuality            *bool
	publishedCountryCode     *string
	countryName              string
	// lookback index to its weights, as the pool query reads them: without
	// score rows, one neutral lookback 0 (hasReliabilityHistory false)
	lookbacks             map[int]providerStatusLookback
	hasReliabilityHistory bool
}

// The neutral lookback the pool query gives a provider with no score rows.
func providerStatusNeutralLookbacks() map[int]providerStatusLookback {
	return map[int]providerStatusLookback{
		0: {reliabilityWeight: 1, independentReliabilityWeight: 1},
	}
}

// Loads each of clientIds that belongs to networkId, in clientIds order.
func loadProviderStatusFacts(ctx context.Context, networkId server.Id, clientIds []server.Id) []*providerStatusFacts {
	clientFacts := map[server.Id]*providerStatusFacts{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client.client_id,
				network_client.network_id,
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = network_client.client_id AND provide_key.provide_mode = $3
				),
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = network_client.client_id AND provide_key.provide_mode = $4
				),
				network_client_location_reliability.client_id IS NOT NULL,
				COALESCE(network_client_location_reliability.connected, false),
				COALESCE(network_client_location_reliability.valid, false),
				COALESCE(network_client_location_reliability.max_net_type_score, 0),
				COALESCE(network_client_location_reliability.max_net_type_score_speed, 0),
				COALESCE(network_client_location_reliability.min_relative_latency_ms, 0),
				COALESCE(network_client_location_reliability.max_bytes_per_second, 0),
				COALESCE(network_client_location_reliability.has_latency_test, false),
				COALESCE(network_client_location_reliability.has_speed_test, false),
				network_client_location_reliability.egress_index,
				network_client_location_reliability.egress_quality,
				country_location.country_code,
				COALESCE(country_location.location_name, '')
			FROM network_client
			LEFT JOIN network_client_location_reliability ON
				network_client_location_reliability.client_id = network_client.client_id
			LEFT JOIN location AS country_location ON
				country_location.location_id = network_client_location_reliability.country_location_id
			WHERE
				network_client.client_id = ANY($1) AND
				network_client.network_id = $2 AND
				network_client.active = true AND
				network_client.source_client_id IS NULL
			`,
			clientIds,
			networkId,
			ProvideModePublic,
			ProvideModeNetwork,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				facts := &providerStatusFacts{
					lookbacks: providerStatusNeutralLookbacks(),
				}
				server.Raise(result.Scan(
					&facts.clientId,
					&facts.networkId,
					&facts.providePublic,
					&facts.provideNetwork,
					&facts.hasRollup,
					&facts.connected,
					&facts.valid,
					&facts.netTypeScore,
					&facts.netTypeScoreSpeed,
					&facts.minRelativeLatencyMillis,
					&facts.maxBytesPerSecond,
					&facts.hasLatencyTest,
					&facts.hasSpeedTest,
					&facts.egressIndex,
					&facts.egressQuality,
					&facts.publishedCountryCode,
					&facts.countryName,
				))
				clientFacts[facts.clientId] = facts
			}
		})
		if len(clientFacts) == 0 {
			return
		}

		result, err = conn.Query(
			ctx,
			`
			SELECT
				client_id,
				lookback_index,
				reliability_weight,
				independent_reliability_weight
			FROM client_connection_reliability_score
			WHERE client_id = ANY($1)
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var lookbackIndex int
				var lookback providerStatusLookback
				server.Raise(result.Scan(
					&clientId,
					&lookbackIndex,
					&lookback.reliabilityWeight,
					&lookback.independentReliabilityWeight,
				))
				facts, ok := clientFacts[clientId]
				if !ok {
					continue
				}
				if !facts.hasReliabilityHistory {
					facts.hasReliabilityHistory = true
					facts.lookbacks = map[int]providerStatusLookback{}
				}
				facts.lookbacks[lookbackIndex] = lookback
			}
		})
	})

	orderedFacts := []*providerStatusFacts{}
	for _, clientId := range clientIds {
		if facts, ok := clientFacts[clientId]; ok {
			orderedFacts = append(orderedFacts, facts)
			delete(clientFacts, clientId)
		}
	}
	return orderedFacts
}

// One provider as FindProviders2's rules see it.
type providerStatusEvaluation struct {
	reason      string
	egressFacts *providerEgressFacts
	decision    providerEgressDecision
	// the common gates UpdateClientScores drops a provider for
	hardExcluded      bool
	passesReliability bool
	// the pool score as UpdateClientScores ranks it
	clientScore *ClientScore
}

// Decides one provider with the rules and ranking of UpdateClientScores.
func evaluateProviderStatus(
	facts *providerStatusFacts,
	countFilter providerCountFilter,
	egressSettings *EgressIndexSettings,
	egressTestEnabled bool,
	minIndependentReliabilityWeights map[int]float64,
) *providerStatusEvaluation {
	clientScore := &ClientScore{
		ClientId:             facts.clientId,
		NetworkId:            facts.networkId,
		NetworkOnly:          !facts.providePublic,
		LookbackClientScores: map[int]*ClientScore{},
	}
	rankModeBases, rankModeMinimumBases := clientScoreRankModeBases(facts.netTypeScore, facts.netTypeScoreSpeed, facts.egressIndex)
	weights := map[int]float64{}
	for lookbackIndex, lookback := range facts.lookbacks {
		lookbackClientScore := &ClientScore{
			ClientId:                     facts.clientId,
			LookbackIndex:                lookbackIndex,
			NetworkId:                    facts.networkId,
			NetworkOnly:                  !facts.providePublic,
			ReliabilityWeight:            lookback.reliabilityWeight,
			IndependentReliabilityWeight: lookback.independentReliabilityWeight,
			MinRelativeLatencyMillis:     facts.minRelativeLatencyMillis,
			MaxBytesPerSecond:            facts.maxBytesPerSecond,
			HasLatencyTest:               facts.hasLatencyTest,
			HasSpeedTest:                 facts.hasSpeedTest,
			Scores:                       map[string]int{},
			Tiers:                        map[string]int{},
		}
		setClientScoreRanks(
			lookbackClientScore,
			rankModeBases,
			rankModeMinimumBases,
			facts.minRelativeLatencyMillis,
			facts.maxBytesPerSecond,
			facts.hasLatencyTest,
			facts.hasSpeedTest,
		)
		clientScore.LookbackClientScores[lookbackIndex] = lookbackClientScore
		weights[lookbackIndex] = lookback.independentReliabilityWeight
	}

	egressFacts := countFilter.egressFacts(
		facts.clientId,
		facts.publishedCountryCode,
		facts.egressIndex,
		facts.egressQuality,
		egressSettings,
	)
	decision := decideProviderEgress(egressFacts, egressTestEnabled)
	rankClientScore(clientScore, decision, countFilter, egressSettings, minIndependentReliabilityWeights)

	evaluation := &providerStatusEvaluation{
		egressFacts:       egressFacts,
		decision:          decision,
		hardExcluded:      countFilter.hasHardEgressFailure(facts.clientId),
		passesReliability: providerReliabilityPasses(weights, minIndependentReliabilityWeights),
		clientScore:       clientScore,
	}

	switch {
	case !facts.providePublic && !facts.provideNetwork:
		evaluation.reason = ProviderStatusReasonNotProviding
	case !facts.hasRollup || !facts.connected:
		evaluation.reason = ProviderStatusReasonNotConnected
	case !facts.valid:
		evaluation.reason = ProviderStatusReasonLocationInvalid
	case !facts.providePublic:
		evaluation.reason = ProviderStatusReasonNetworkOnly
	case decision.reason == ProviderExcludedReliability || !evaluation.passesReliability:
		shortWeight, observed := weights[providerStatusShortLookbackIndex]
		if !facts.hasReliabilityHistory || !observed || minIndependentReliabilityWeights[providerStatusShortLookbackIndex] <= shortWeight {
			evaluation.reason = ProviderStatusReasonReliabilityWarmingUp
		} else {
			evaluation.reason = ProviderStatusReasonReliabilityLow
		}
	case decision.hardExcluded || evaluation.hardExcluded:
		// ARIN risk or the URL security quarantine
		evaluation.reason = ProviderStatusReasonNotEligible
	case decision.reason == ProviderExcludedUnprobed:
		evaluation.reason = ProviderStatusReasonEgressUnprobed
	case decision.reason == ProviderExcludedHealth:
		evaluation.reason = ProviderStatusReasonEgressFailing
	case decision.reason == ProviderExcludedArinNonQuality:
		evaluation.reason = ProviderStatusReasonNotEligible
	case !facts.hasSpeedTest || !facts.hasLatencyTest:
		evaluation.reason = ProviderStatusReasonSpeedTestMissing
	case clientScore.Tiers[RankModeQuality] == ClientScoreCutoffTier:
		// past the quality latency or throughput cutoff
		evaluation.reason = ProviderStatusReasonSlow
	default:
		evaluation.reason = ProviderStatusReasonNone
	}
	return evaluation
}

func providerEgressVerdict(egressQuality *bool) string {
	switch {
	case egressQuality == nil:
		return ProviderEgressUnprobed
	case *egressQuality:
		return ProviderEgressPass
	default:
		return ProviderEgressFail
	}
}

// Megabytes per second with one decimal, for the explanations.
func formatProviderStatusBytesPerSecond(bytesPerSecond ByteCount) string {
	return fmt.Sprintf("%.1f MB/s", float64(bytesPerSecond)/1e6)
}

// The reasons that keep a provider out of every pool. The mode-level numbers
// do not apply to them: for not_eligible they would also tell the anti-abuse
// signals apart.
var providerStatusOutOfPoolReasons = map[string]bool{
	ProviderStatusReasonNotProviding:    true,
	ProviderStatusReasonNotConnected:    true,
	ProviderStatusReasonLocationInvalid: true,
	ProviderStatusReasonNotEligible:     true,
}

// The wire status of one evaluated provider. Its admission, numbers and
// texts read only facts a provider may see: the anti-abuse signals enter only
// as the generic reason, with the mode-level numbers left out.
func newProviderStatus(
	facts *providerStatusFacts,
	evaluation *providerStatusEvaluation,
	countFilter providerCountFilter,
	egressSettings *EgressIndexSettings,
	minIndependentReliabilityWeights map[int]float64,
	now time.Time,
) *ProviderStatus {
	healthCounts, hasHealthCounts := countFilter.healthCounts[facts.clientId]
	hasHealthCounts = hasHealthCounts && 0 < healthCounts.Total
	egress := providerEgressVerdict(evaluation.egressFacts.egressQuality)

	status := &ProviderStatus{
		ClientId:   facts.clientId,
		Reason:     evaluation.reason,
		ReasonText: providerStatusReasonTexts[evaluation.reason],
		Admission: &ProviderAdmission{
			Connected:     facts.hasRollup && facts.connected,
			LocationValid: facts.hasRollup && facts.valid,
			ProvidePublic: facts.providePublic,
			ReliabilityOk: evaluation.passesReliability && evaluation.decision.reason != ProviderExcludedReliability,
			SpeedTestDone: facts.hasSpeedTest && facts.hasLatencyTest,
			Egress:        egress,
		},
		Ranking:      []*ProviderRankingNumber{},
		EvaluateTime: now,
	}
	if egress != ProviderEgressUnprobed && hasHealthCounts {
		measuredAt := healthCounts.MeasuredAt.UTC()
		status.Admission.EgressMeasuredAt = &measuredAt
	}

	// reliability, per lookback
	reliabilityNames := map[int]bool{}
	for _, reliabilityNumber := range providerStatusReliabilityNumbers {
		reliabilityNames[reliabilityNumber.lookbackIndex] = true
		number := &ProviderRankingNumber{
			Name:   reliabilityNumber.name,
			Passes: true,
		}
		minimum, hasMinimum := minIndependentReliabilityWeights[reliabilityNumber.lookbackIndex]
		if hasMinimum {
			number.HasMinimum = true
			number.Minimum = minimum
		}
		lookback, observed := facts.lookbacks[reliabilityNumber.lookbackIndex]
		switch {
		case !facts.hasReliabilityHistory || !observed:
			number.Explanation = fmt.Sprintf("No reliability history for the last %s yet. Missing history doesn't count against this device.", reliabilityNumber.span)
		case hasMinimum:
			number.HasValue = true
			number.Value = lookback.independentReliabilityWeight
			number.Passes = minimum <= lookback.independentReliabilityWeight
			number.Explanation = fmt.Sprintf("How steadily this device stayed connected over the last %s: %.2f. At least %.2f is needed to be offered to clients; it rises with steady uptime and disconnects lower it.", reliabilityNumber.span, lookback.independentReliabilityWeight, minimum)
		default:
			number.HasValue = true
			number.Value = lookback.independentReliabilityWeight
			number.Passes = 0 <= lookback.independentReliabilityWeight
			number.Explanation = fmt.Sprintf("How steadily this device stayed connected over the last %s: %.2f. It scales how often clients are offered this device; staying connected raises it.", reliabilityNumber.span, lookback.independentReliabilityWeight)
		}
		status.Ranking = append(status.Ranking, number)
	}
	if facts.hasReliabilityHistory {
		otherLookbackIndexes := []int{}
		for lookbackIndex := range facts.lookbacks {
			if !reliabilityNames[lookbackIndex] {
				otherLookbackIndexes = append(otherLookbackIndexes, lookbackIndex)
			}
		}
		slices.Sort(otherLookbackIndexes)
		for _, lookbackIndex := range otherLookbackIndexes {
			weight := facts.lookbacks[lookbackIndex].independentReliabilityWeight
			minimum := minIndependentReliabilityWeights[lookbackIndex]
			status.Ranking = append(status.Ranking, &ProviderRankingNumber{
				Name:        fmt.Sprintf("reliability_lookback_%d", lookbackIndex),
				HasValue:    true,
				Value:       weight,
				HasMinimum:  true,
				Minimum:     minimum,
				Passes:      minimum <= weight,
				Explanation: fmt.Sprintf("How steadily this device stayed connected over a longer window: %.2f. At least %.2f is needed to be offered to clients.", weight, minimum),
			})
		}
	}

	// the URL checks
	urlChecks := &ProviderRankingNumber{
		Name:       ProviderStatusNumberUrlChecks,
		HasMinimum: true,
		Minimum:    float64(egressSettings.QualityOkNumerator) / float64(egressSettings.QualityOkDenominator),
		Passes:     egress == ProviderEgressPass,
	}
	if hasHealthCounts && egress != ProviderEgressUnprobed {
		urlChecks.HasValue = true
		urlChecks.Value = float64(healthCounts.OKCount) / float64(healthCounts.Total)
		urlChecks.Count = healthCounts.OKCount
		urlChecks.Total = healthCounts.Total
		urlChecks.Explanation = fmt.Sprintf("Test sites that loaded through this connection in the last %d hours: %d of %d. At least %d in %d must load; filtering by your internet provider lowers it.", int(min(egressSettings.EvidenceMaxAge, ProviderEgressHealthMaxAge)/time.Hour), healthCounts.OKCount, healthCounts.Total, egressSettings.QualityOkNumerator, egressSettings.QualityOkDenominator)
	} else {
		urlChecks.Explanation = fmt.Sprintf("No network checks in the last %d hours yet. Until a check passes, clients are offered this device only when checked providers run out.", int(min(egressSettings.EvidenceMaxAge, ProviderEgressHealthMaxAge)/time.Hour))
	}
	status.Ranking = append(status.Ranking, urlChecks)

	// the performance tests, against the quality cutoffs
	qualityTarget := clientScorePerformanceTargets[RankModeQuality]
	speedTest := &ProviderRankingNumber{
		Name:       ProviderStatusNumberSpeedTest,
		HasMinimum: true,
		Minimum:    float64(qualityTarget.bytesPerSecondCutoff),
	}
	if facts.hasSpeedTest {
		speedTest.HasValue = true
		speedTest.Value = float64(facts.maxBytesPerSecond)
		speedTest.Passes = qualityTarget.bytesPerSecondCutoff <= facts.maxBytesPerSecond
		speedTest.Explanation = fmt.Sprintf("Speed measured through this device: %s. Faster connections are offered more often; below %s clients prefer other providers.", formatProviderStatusBytesPerSecond(facts.maxBytesPerSecond), formatProviderStatusBytesPerSecond(qualityTarget.bytesPerSecondCutoff))
	} else {
		speedTest.Explanation = "No speed test yet. Until one completes, clients are offered this device much less often."
	}
	status.Ranking = append(status.Ranking, speedTest)

	latency := &ProviderRankingNumber{
		Name:       ProviderStatusNumberLatency,
		HasMaximum: true,
		Maximum:    float64(qualityTarget.relativeLatencyMillisCutoff),
	}
	if facts.hasLatencyTest {
		latency.HasValue = true
		latency.Value = float64(facts.minRelativeLatencyMillis)
		latency.Passes = facts.minRelativeLatencyMillis <= qualityTarget.relativeLatencyMillisCutoff
		latency.Explanation = fmt.Sprintf("Delay measured above what is expected for this location: %d ms. Lower delay is offered more often; above %d ms clients prefer other providers.", facts.minRelativeLatencyMillis, qualityTarget.relativeLatencyMillisCutoff)
	} else {
		latency.Explanation = "No delay measurement yet. Until one completes, clients are offered this device much less often."
	}
	status.Ranking = append(status.Ranking, latency)

	// what each mode draws and orders the provider by
	if !providerStatusOutOfPoolReasons[evaluation.reason] {
		clientScore := evaluation.clientScore
		for _, mode := range []struct {
			rankMode   RankMode
			weightName string
			tierName   string
			label      string
		}{
			{rankMode: RankModeQuality, weightName: ProviderStatusNumberWeightQuality, tierName: ProviderStatusNumberTierQuality, label: "quality"},
			{rankMode: RankModeSpeed, weightName: ProviderStatusNumberWeightSpeed, tierName: ProviderStatusNumberTierSpeed, label: "speed"},
		} {
			native := clientScore.PassesMinimums[mode.rankMode]
			weight := float64(clientScore.ScaledWeights[mode.rankMode])
			weightNumber := &ProviderRankingNumber{
				Name:     mode.weightName,
				HasValue: true,
				Value:    weight,
				Passes:   native,
			}
			switch {
			case native:
				weightNumber.Explanation = fmt.Sprintf("Selection weight for %s requests: %.3f. It multiplies reliability, speed and passed checks; clients are offered providers with a higher weight more often.", mode.label, weight)
			case clientScore.Online:
				weightNumber.Explanation = fmt.Sprintf("Not in the %s pool right now, so %s requests reach this device only when that pool runs out.", mode.label, mode.label)
			default:
				weightNumber.Explanation = fmt.Sprintf("Not offered for %s requests right now.", mode.label)
			}
			status.Ranking = append(status.Ranking, weightNumber)

			tier := clientScore.Tiers[mode.rankMode]
			status.Ranking = append(status.Ranking, &ProviderRankingNumber{
				Name:        mode.tierName,
				HasValue:    true,
				Value:       float64(tier),
				HasMaximum:  true,
				Maximum:     float64(MaxNativeClientScoreTier),
				Passes:      tier <= MaxNativeClientScoreTier,
				Explanation: fmt.Sprintf("Tier for %s requests: %d. Clients try lower tiers first; 0 is best and %d means past the speed or delay cutoff.", mode.label, tier, ClientScoreCutoffTier),
			})
		}
	}

	// where clients find it
	if countryCode := normalizeCountryCode(facts.publishedCountryCode); countryCode != "" {
		country := &ProviderStatusCountry{
			CountryCode:         countryCode,
			Country:             facts.countryName,
			ObservedCountryCode: countFilter.countryCodes[facts.clientId],
		}
		name := facts.countryName
		if name == "" {
			name = countryCode
		}
		country.Explanation = fmt.Sprintf("Clients who choose %s can be offered this device.", name)
		if country.ObservedCountryCode != "" && country.ObservedCountryCode != countryCode {
			country.Explanation = fmt.Sprintf("Clients who choose %s can be offered this device. The latest network check saw this connection exit in %s.", name, country.ObservedCountryCode)
		}
		status.Country = country
	}
	return status
}

// Lists the network's provider clients (active, top level, any provide key)
// in client id order: at most limit, and whether there were more.
func getNetworkProviderClientIds(ctx context.Context, networkId server.Id, limit int) (clientIds []server.Id, truncated bool) {
	clientIds = []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_client.client_id
			FROM network_client
			WHERE
				network_client.network_id = $1 AND
				network_client.active = true AND
				network_client.source_client_id IS NULL AND
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = network_client.client_id
				)
			ORDER BY network_client.client_id
			LIMIT $2
			`,
			networkId,
			limit+1,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	if limit < len(clientIds) {
		clientIds = clientIds[:limit]
		truncated = true
	}
	return
}

// Decides each of clientIds that is a provider client of networkId, with the
// bulk filter scoped to those ids. Others are left out.
func GetProviderStatuses(ctx context.Context, networkId server.Id, clientIds []server.Id) []*ProviderStatus {
	statuses := []*ProviderStatus{}
	if len(clientIds) == 0 {
		return statuses
	}
	allFacts := loadProviderStatusFacts(ctx, networkId, clientIds)
	if len(allFacts) == 0 {
		return statuses
	}
	factClientIds := make([]server.Id, 0, len(allFacts))
	for _, facts := range allFacts {
		factClientIds = append(factClientIds, facts.clientId)
	}

	egressSettings := egressIndexSettings()
	egressTestEnabled := providerEgressTestEnabled()
	minimums := providerReliabilityMinimums()
	countFilter := newProviderCountFilterForClients(ctx, factClientIds)
	now := server.NowUtc()
	for _, facts := range allFacts {
		evaluation := evaluateProviderStatus(facts, countFilter, egressSettings, egressTestEnabled, minimums)
		statuses = append(statuses, newProviderStatus(facts, evaluation, countFilter, egressSettings, minimums, now))
	}
	return statuses
}

// The admission and ranking of the network's provider clients, at most
// ProviderStatusMaxClients of them in client id order. The histograms are
// not part of it.
func GetNetworkProviderStatuses(ctx context.Context, networkId server.Id) *ProviderStatusesResult {
	clientIds, truncated := getNetworkProviderClientIds(ctx, networkId, ProviderStatusMaxClients)
	return &ProviderStatusesResult{
		Providers: GetProviderStatuses(ctx, networkId, clientIds),
		Truncated: truncated,
	}
}

// One provider client of the network, nil when it is not one.
func GetClientProviderStatus(ctx context.Context, networkId server.Id, clientId server.Id) *ProviderStatus {
	statuses := GetProviderStatuses(ctx, networkId, []server.Id{clientId})
	if len(statuses) == 0 {
		return nil
	}
	return statuses[0]
}
