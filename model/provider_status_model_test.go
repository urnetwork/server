package model

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The provider status without a database: the reason order, the ranking
// numbers and their agreement with the client-score ranking, and that the
// anti-abuse signals stay one generic reason.

var testingProviderStatusNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

// A connected public provider that passes every rule: steady, checked, fast.
func testingProviderStatusFacts() *providerStatusFacts {
	countryCode := "us"
	egressIndex := 0
	egressQuality := true
	return &providerStatusFacts{
		clientId:                 server.NewId(),
		networkId:                server.NewId(),
		providePublic:            true,
		provideNetwork:           true,
		hasRollup:                true,
		connected:                true,
		valid:                    true,
		minRelativeLatencyMillis: 10,
		maxBytesPerSecond:        100 * Mib,
		hasLatencyTest:           true,
		hasSpeedTest:             true,
		egressIndex:              &egressIndex,
		egressQuality:            &egressQuality,
		publishedCountryCode:     &countryCode,
		countryName:              "United States",
		lookbacks: map[int]providerStatusLookback{
			0: {reliabilityWeight: 1, independentReliabilityWeight: 1},
			1: {reliabilityWeight: 1, independentReliabilityWeight: 1},
			2: {reliabilityWeight: 1, independentReliabilityWeight: 1},
		},
		hasReliabilityHistory: true,
	}
}

// The bulk evidence for the provider: 20 of 20 URL checks an hour ago, the
// exit seen in its published country, no exception.
func testingProviderStatusFilter(facts *providerStatusFacts) providerCountFilter {
	return providerCountFilter{
		now:                     testingProviderStatusNow,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		healthCounts: map[server.Id]ProviderEgressHealthCounts{
			facts.clientId: {
				MeasuredAt:      testingProviderStatusNow.Add(-time.Hour),
				FirstMeasuredAt: testingProviderStatusNow.Add(-2 * time.Hour),
				OKCount:         20,
				Total:           20,
			},
		},
		healthWindowEnd: testingProviderStatusNow,
		countryCodes: map[server.Id]string{
			facts.clientId: "us",
		},
	}
}

var testingProviderStatusMinimums = map[int]float64{1: 0.95, 2: 0.7, 3: 0.6}

func testingProviderStatus(facts *providerStatusFacts, countFilter providerCountFilter) (*providerStatusEvaluation, *ProviderStatus) {
	settings := DefaultEgressIndexSettings()
	evaluation := evaluateProviderStatus(facts, countFilter, settings, false, testingProviderStatusMinimums)
	return evaluation, newProviderStatus(facts, evaluation, countFilter, settings, testingProviderStatusMinimums, testingProviderStatusNow)
}

func testingProviderStatusNumber(status *ProviderStatus, name string) *ProviderRankingNumber {
	for _, number := range status.Ranking {
		if number.Name == name {
			return number
		}
	}
	return nil
}

// The first failing rule names the reason, in the order the selection
// applies them.
func TestProviderStatusReasonOrder(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		modify func(*providerStatusFacts, *providerCountFilter)
		reason string
	}{
		{name: "passes", modify: func(*providerStatusFacts, *providerCountFilter) {}, reason: ProviderStatusReasonNone},
		{name: "no provide key", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.providePublic = false
			f.provideNetwork = false
			f.connected = false
		}, reason: ProviderStatusReasonNotProviding},
		{name: "no rollup row", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.hasRollup = false
			f.connected = false
			f.valid = false
		}, reason: ProviderStatusReasonNotConnected},
		{name: "disconnected", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.connected = false
			c.arinRisk[f.clientId] = true
		}, reason: ProviderStatusReasonNotConnected},
		{name: "two addresses", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.valid = false
		}, reason: ProviderStatusReasonLocationInvalid},
		{name: "network only", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.providePublic = false
			c.arinRisk[f.clientId] = true
		}, reason: ProviderStatusReasonNetworkOnly},
		{name: "warming up", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.lookbacks[2] = providerStatusLookback{reliabilityWeight: 0.3, independentReliabilityWeight: 0.3}
			c.reliabilityFailed[f.clientId] = true
		}, reason: ProviderStatusReasonReliabilityWarmingUp},
		{name: "unstable", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.lookbacks[1] = providerStatusLookback{reliabilityWeight: 0.5, independentReliabilityWeight: 0.5}
			f.lookbacks[2] = providerStatusLookback{reliabilityWeight: 0.3, independentReliabilityWeight: 0.3}
			c.reliabilityFailed[f.clientId] = true
		}, reason: ProviderStatusReasonReliabilityLow},
		{name: "reliability before arin risk, as decided", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.lookbacks[1] = providerStatusLookback{reliabilityWeight: 0.5, independentReliabilityWeight: 0.5}
			c.reliabilityFailed[f.clientId] = true
			c.arinRisk[f.clientId] = true
		}, reason: ProviderStatusReasonReliabilityLow},
		{name: "arin risk", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinRisk[f.clientId] = true
		}, reason: ProviderStatusReasonNotEligible},
		{name: "tls quarantine", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			c.tlsAuthenticationFailed[f.clientId] = true
		}, reason: ProviderStatusReasonNotEligible},
		{name: "unprobed", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			delete(c.healthCounts, f.clientId)
			c.arinNonQuality[f.clientId] = true
		}, reason: ProviderStatusReasonEgressUnprobed},
		{name: "stale evidence is unprobed", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			counts := c.healthCounts[f.clientId]
			counts.MeasuredAt = testingProviderStatusNow.Add(-ProviderEgressHealthMaxAge)
			c.healthCounts[f.clientId] = counts
		}, reason: ProviderStatusReasonEgressUnprobed},
		{name: "failing checks", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			c.healthCounts[f.clientId] = ProviderEgressHealthCounts{MeasuredAt: testingProviderStatusNow, FirstMeasuredAt: testingProviderStatusNow, OKCount: 15, Total: 20}
		}, reason: ProviderStatusReasonEgressFailing},
		{name: "arin non-quality", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinNonQuality[f.clientId] = true
		}, reason: ProviderStatusReasonNotEligible},
		{name: "no speed test", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.hasSpeedTest = false
		}, reason: ProviderStatusReasonSpeedTestMissing},
		{name: "no latency test", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.hasLatencyTest = false
		}, reason: ProviderStatusReasonSpeedTestMissing},
		{name: "past the throughput cutoff", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.maxBytesPerSecond = 500 * Kib
		}, reason: ProviderStatusReasonSlow},
		{name: "past the latency cutoff", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.minRelativeLatencyMillis = 250
		}, reason: ProviderStatusReasonSlow},
		{name: "past only the speed cutoff", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			f.maxBytesPerSecond = 2 * Mib
		}, reason: ProviderStatusReasonNone},
		{name: "a legacy dark verdict decides nothing", modify: func(f *providerStatusFacts, c *providerCountFilter) {
			c.blackholed = map[server.Id]bool{f.clientId: true}
		}, reason: ProviderStatusReasonNone},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			facts := testingProviderStatusFacts()
			countFilter := testingProviderStatusFilter(facts)
			testCase.modify(facts, &countFilter)
			_, status := testingProviderStatus(facts, countFilter)
			if status.Reason != testCase.reason {
				t.Fatalf("reason = %s, want %s", status.Reason, testCase.reason)
			}
			if status.ReasonText != providerStatusReasonTexts[testCase.reason] || status.ReasonText == "" {
				t.Fatalf("reason text = %q", status.ReasonText)
			}
			if !slices.Contains(ProviderStatusReasons, status.Reason) {
				t.Fatalf("reason %s is not listed", status.Reason)
			}
		})
	}
}

// Every anti-abuse signal reads as the one generic reason with identical
// status JSON, so none can be told apart; no field names a signal, and the
// legacy dark verdict, which the selection ignores, changes nothing.
func TestProviderStatusAntiAbuseSignalsAreIndistinguishable(t *testing.T) {
	statusJson := func(modify func(*providerStatusFacts, *providerCountFilter)) (string, *ProviderStatus) {
		facts := testingProviderStatusFacts()
		facts.clientId = server.Id{1}
		countFilter := testingProviderStatusFilter(facts)
		modify(facts, &countFilter)
		_, status := testingProviderStatus(facts, countFilter)
		statusBytes, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		return string(statusBytes), status
	}

	signals := map[string]func(*providerStatusFacts, *providerCountFilter){
		"arin_risk": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinRisk[f.clientId] = true
		},
		"arin_non_quality": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinNonQuality[f.clientId] = true
		},
		"tls": func(f *providerStatusFacts, c *providerCountFilter) {
			c.tlsAuthenticationFailed[f.clientId] = true
		},
		"arin_risk_and_tls": func(f *providerStatusFacts, c *providerCountFilter) {
			c.arinRisk[f.clientId] = true
			c.tlsAuthenticationFailed[f.clientId] = true
		},
	}
	var firstJson string
	for name, signal := range signals {
		signalJson, status := statusJson(signal)
		if status.Reason != ProviderStatusReasonNotEligible || status.ReasonText != "This connection isn't eligible to provide right now." {
			t.Fatalf("%s: reason %s %q", name, status.Reason, status.ReasonText)
		}
		for _, number := range status.Ranking {
			if strings.HasPrefix(number.Name, "weight_") || strings.HasPrefix(number.Name, "tier_") {
				t.Fatalf("%s: the mode-level number %s tells the signals apart", name, number.Name)
			}
		}
		lower := strings.ToLower(signalJson)
		for _, leak := range []string{"arin", "tls", "blackhole", "risk", "quarantin", "abuse", "verified"} {
			if strings.Contains(lower, leak) {
				t.Fatalf("%s: status names a signal (%q): %s", name, leak, signalJson)
			}
		}
		if firstJson == "" {
			firstJson = signalJson
		} else if signalJson != firstJson {
			t.Fatalf("%s reads differently from another signal:\n%s\n%s", name, signalJson, firstJson)
		}
	}

	baseJson, _ := statusJson(func(*providerStatusFacts, *providerCountFilter) {})
	darkJson, _ := statusJson(func(f *providerStatusFacts, c *providerCountFilter) {
		c.blackholed = map[server.Id]bool{f.clientId: true}
	})
	if darkJson != baseJson {
		t.Fatal("the legacy dark verdict changed the status")
	}
}

// The numbers a passing provider is shown, in order, with their floors and
// explanations.
func TestProviderStatusNumbers(t *testing.T) {
	facts := testingProviderStatusFacts()
	_, status := testingProviderStatus(facts, testingProviderStatusFilter(facts))

	names := []string{}
	for _, number := range status.Ranking {
		names = append(names, number.Name)
		if number.Explanation == "" {
			t.Fatalf("%s has no explanation", number.Name)
		}
	}
	wantNames := []string{
		ProviderStatusNumberReliability5m,
		ProviderStatusNumberReliability1h,
		ProviderStatusNumberReliability12h,
		ProviderStatusNumberUrlChecks,
		ProviderStatusNumberSpeedTest,
		ProviderStatusNumberLatency,
		ProviderStatusNumberWeightQuality,
		ProviderStatusNumberTierQuality,
		ProviderStatusNumberWeightSpeed,
		ProviderStatusNumberTierSpeed,
	}
	if !slices.Equal(names, wantNames) {
		t.Fatalf("names = %v", names)
	}

	reliability := testingProviderStatusNumber(status, ProviderStatusNumberReliability1h)
	if !reliability.HasValue || reliability.Value != 1 || !reliability.HasMinimum || reliability.Minimum != 0.95 || !reliability.Passes {
		t.Fatalf("reliability_1h = %+v", reliability)
	}
	if fiveMinutes := testingProviderStatusNumber(status, ProviderStatusNumberReliability5m); fiveMinutes.HasMinimum {
		t.Fatal("the 5 minute lookback has no floor")
	}
	urlChecks := testingProviderStatusNumber(status, ProviderStatusNumberUrlChecks)
	if !urlChecks.HasValue || urlChecks.Count != 20 || urlChecks.Total != 20 || urlChecks.Value != 1 || urlChecks.Minimum != 0.8 || !urlChecks.Passes {
		t.Fatalf("url_checks = %+v", urlChecks)
	}
	speedTest := testingProviderStatusNumber(status, ProviderStatusNumberSpeedTest)
	if !speedTest.HasValue || speedTest.Value != float64(100*Mib) || speedTest.Minimum != float64(800*Kib) || !speedTest.Passes {
		t.Fatalf("speed_test = %+v", speedTest)
	}
	latency := testingProviderStatusNumber(status, ProviderStatusNumberLatency)
	if !latency.HasValue || latency.Value != 10 || !latency.HasMaximum || latency.Maximum != 200 || !latency.Passes {
		t.Fatalf("latency = %+v", latency)
	}
	if tier := testingProviderStatusNumber(status, ProviderStatusNumberTierQuality); tier.Value != 0 || !tier.Passes {
		t.Fatalf("tier_quality = %+v", tier)
	}

	admission := status.Admission
	if !admission.Connected || !admission.LocationValid || !admission.ProvidePublic || !admission.ReliabilityOk || !admission.SpeedTestDone || admission.Egress != ProviderEgressPass {
		t.Fatalf("admission = %+v", admission)
	}
	if admission.EgressMeasuredAt == nil || !admission.EgressMeasuredAt.Equal(testingProviderStatusNow.Add(-time.Hour)) {
		t.Fatalf("egress measured at = %v", admission.EgressMeasuredAt)
	}
	if status.Country == nil || status.Country.CountryCode != "us" || status.Country.Country != "United States" || status.Country.ObservedCountryCode != "us" {
		t.Fatalf("country = %+v", status.Country)
	}
}

// Missing reliability history is neutral and says so; a provider in no pool
// is shown no mode-level numbers; unprobed checks have no value.
func TestProviderStatusNumbersWithoutEvidence(t *testing.T) {
	facts := testingProviderStatusFacts()
	facts.lookbacks = providerStatusNeutralLookbacks()
	facts.hasReliabilityHistory = false
	countFilter := testingProviderStatusFilter(facts)
	delete(countFilter.healthCounts, facts.clientId)
	_, status := testingProviderStatus(facts, countFilter)
	for _, name := range []string{ProviderStatusNumberReliability5m, ProviderStatusNumberReliability1h, ProviderStatusNumberReliability12h} {
		if number := testingProviderStatusNumber(status, name); number.HasValue || !number.Passes {
			t.Fatalf("%s = %+v, want no value and no hold", name, number)
		}
	}
	if !status.Admission.ReliabilityOk || status.Admission.Egress != ProviderEgressUnprobed || status.Admission.EgressMeasuredAt != nil {
		t.Fatalf("admission = %+v", status.Admission)
	}
	if urlChecks := testingProviderStatusNumber(status, ProviderStatusNumberUrlChecks); urlChecks.HasValue || urlChecks.Passes {
		t.Fatalf("url_checks = %+v", urlChecks)
	}
	if weight := testingProviderStatusNumber(status, ProviderStatusNumberWeightQuality); weight == nil || weight.Passes || weight.Value != 0 {
		t.Fatalf("an unprobed provider's quality weight = %+v", weight)
	}

	for _, modify := range []func(*providerStatusFacts){
		func(f *providerStatusFacts) { f.connected = false },
		func(f *providerStatusFacts) { f.valid = false },
		func(f *providerStatusFacts) { f.providePublic = false; f.provideNetwork = false },
	} {
		facts := testingProviderStatusFacts()
		modify(facts)
		_, status := testingProviderStatus(facts, testingProviderStatusFilter(facts))
		for _, number := range status.Ranking {
			if strings.HasPrefix(number.Name, "weight_") || strings.HasPrefix(number.Name, "tier_") {
				t.Fatalf("%s: a provider in no pool was shown %s", status.Reason, number.Name)
			}
		}
	}
}

// The status ranks with the client-score export's math: the weights and
// tiers below are the export's formulas worked by hand.
func TestProviderStatusMatchesExportRanking(t *testing.T) {
	facts := testingProviderStatusFacts()
	facts.maxBytesPerSecond = 10 * Mib
	facts.lookbacks = map[int]providerStatusLookback{
		0: {reliabilityWeight: 0.9, independentReliabilityWeight: 0.9},
		1: {reliabilityWeight: 0.97, independentReliabilityWeight: 0.97},
		2: {reliabilityWeight: 0.8, independentReliabilityWeight: 0.8},
	}
	// 19 of 20 checks: passing, with a URL weight of 0.1 + 0.9·0.95
	countFilter := testingProviderStatusFilter(facts)
	countFilter.healthCounts[facts.clientId] = ProviderEgressHealthCounts{MeasuredAt: testingProviderStatusNow, FirstMeasuredAt: testingProviderStatusNow, OKCount: 19, Total: 20}
	urlWeight := 0.1 + 0.9*0.95
	evaluation, status := testingProviderStatus(facts, countFilter)
	clientScore := evaluation.clientScore

	// quality: base 0 (the index), no adjust inside both thresholds
	if clientScore.Scores[RankModeQuality] != 0 || clientScore.Tiers[RankModeQuality] != 0 {
		t.Fatalf("quality score %d tier %d", clientScore.Scores[RankModeQuality], clientScore.Tiers[RankModeQuality])
	}
	// speed: 30 MiB/s short of the 40 MiB/s threshold at 1 MiB/s a point
	if clientScore.Scores[RankModeSpeed] != 30 || clientScore.Tiers[RankModeSpeed] != 1 {
		t.Fatalf("speed score %d tier %d", clientScore.Scores[RankModeSpeed], clientScore.Tiers[RankModeSpeed])
	}
	// lowest lookback 0: u = 0.9 (no floor), reliability scale 0.1·0.1 + 0.9,
	// times its weight 0.9 and the URL weight
	reliabilityPart := (0.1*0.1 + 0.9*1.0) * 0.9 * urlWeight
	wantQuality := reliabilityPart * 1.0
	// speed: v = (40 - 30) / 40, score scale 0.75·0.1 + 0.25
	wantSpeed := reliabilityPart * (0.75*0.1 + 0.25*1.0)
	for rankMode, want := range map[RankMode]float64{RankModeQuality: wantQuality, RankModeSpeed: wantSpeed} {
		if got := float64(clientScore.ScaledWeights[rankMode]); math.Abs(got-want) > 1e-6 || !clientScore.PassesMinimums[rankMode] {
			t.Fatalf("%s weight %f, want %f", rankMode, got, want)
		}
	}
	if !clientScore.Online || clientScore.ReliabilityWeight != 0.9 || math.Abs(clientScore.UrlProbeSuccessWeight-urlWeight) > 1e-9 {
		t.Fatalf("online %t reliability %f url %f", clientScore.Online, clientScore.ReliabilityWeight, clientScore.UrlProbeSuccessWeight)
	}
	if weight := testingProviderStatusNumber(status, ProviderStatusNumberWeightSpeed); math.Abs(weight.Value-wantSpeed) > 1e-6 || !weight.Passes {
		t.Fatalf("weight_speed = %+v", weight)
	}
	if tier := testingProviderStatusNumber(status, ProviderStatusNumberTierSpeed); tier.Value != 1 {
		t.Fatalf("tier_speed = %+v", tier)
	}

	// a failing ratio leaves only the online bucket, still ordered by the
	// URL weight 0.1 + 0.9·(15/20)
	countFilter = testingProviderStatusFilter(facts)
	countFilter.healthCounts[facts.clientId] = ProviderEgressHealthCounts{MeasuredAt: testingProviderStatusNow, FirstMeasuredAt: testingProviderStatusNow, OKCount: 15, Total: 20}
	evaluation, _ = testingProviderStatus(facts, countFilter)
	if len(evaluation.clientScore.PassesMinimums) != 0 || !evaluation.clientScore.Online || math.Abs(evaluation.clientScore.UrlProbeSuccessWeight-(0.1+0.9*0.75)) > 1e-9 {
		t.Fatalf("failing: passes %v online %t url %f", evaluation.clientScore.PassesMinimums, evaluation.clientScore.Online, evaluation.clientScore.UrlProbeSuccessWeight)
	}
}

// The reliability numbers name ClientLookbacks' first three lookbacks.
func TestProviderStatusReliabilityNumbersFollowClientLookbacks(t *testing.T) {
	want := []time.Duration{5 * time.Minute, time.Hour, 12 * time.Hour}
	if len(ClientLookbacks) < len(providerStatusReliabilityNumbers) {
		t.Fatalf("%d lookbacks", len(ClientLookbacks))
	}
	for i, reliabilityNumber := range providerStatusReliabilityNumbers {
		if reliabilityNumber.lookbackIndex != i || ClientLookbacks[i] != want[i] {
			t.Fatalf("lookback %d is %s, named %s", i, ClientLookbacks[i], reliabilityNumber.name)
		}
	}
}

// The scoped filter reads with the fleet filter's predicates, limited to the
// requested providers in every relation it scans.
func TestProviderStatusScopedFilterSql(t *testing.T) {
	scoped := providerCountFilterClientSql()
	fleet := providerCountFilterCommonSql()
	if !strings.Contains(scoped, providerReliabilityEligibilitySql("observed_reliability.client_id")) || !strings.Contains(fleet, providerReliabilityEligibilitySql("observed_reliability.client_id")) {
		t.Fatal("the scoped and fleet filters read different reliability predicates")
	}
	if strings.Count(scoped, "client_id = ANY($1)") != 2 {
		t.Fatal("the scoped filter does not limit both of its scans to the requested providers")
	}
	if !strings.Contains(scoped, "AND (arin_risk OR arin_non_quality)") || !strings.Contains(fleet, "WHERE arin_risk OR arin_non_quality") {
		t.Fatal("the scoped and fleet filters read different ARIN predicates")
	}
}
