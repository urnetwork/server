package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The modern provider gate (egress_modern_provider.go), pure: the outcome a
// provider's evidence gives, the decision with the gate off and on, the
// shadow counts, the switch in provider.yml, the evidence the ingest refuses
// and the provider status.

// A fixed evaluation time, so no test reads the clock.
var modernGateTestNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// An evidence row last measured age before modernGateTestNow.
func modernGateTestRecord(postQuantum bool, windowResizable bool, age time.Duration) *ProviderModernEvidenceRecord {
	record := &ProviderModernEvidenceRecord{
		PostQuantum:     postQuantum,
		KeyExchange:     "X25519",
		WindowResizable: windowResizable,
		MeasuredAt:      modernGateTestNow.Add(-age),
		FirstMeasuredAt: modernGateTestNow.Add(-age - time.Hour),
		RunCount:        3,
	}
	if postQuantum {
		record.KeyExchange = "X25519MLKEM768"
	}
	if windowResizable {
		record.MaxReceiveWindowByteCount = 4 * 1024 * 1024
	}
	return record
}

// A pass's loaded evidence: every provider of clientIdRecords has five URL
// successes of five within the hour, so the URL ratio admits it to Quality
// and Speed, and the given evidence row, nil for none. No URL-success guard
// is set, so that rule applies nowhere.
func modernGateTestFilter(clientIdRecords map[server.Id]*ProviderModernEvidenceRecord) providerCountFilter {
	filter := providerCountFilter{
		now:                     modernGateTestNow,
		arinRisk:                map[server.Id]bool{},
		arinNonQuality:          map[server.Id]bool{},
		reliabilityFailed:       map[server.Id]bool{},
		tlsAuthenticationFailed: map[server.Id]bool{},
		countryCodes:            map[server.Id]string{},
		healthCounts:            map[server.Id]ProviderEgressHealthCounts{},
		modernEvidence:          map[server.Id]*ProviderModernEvidenceRecord{},
	}
	for clientId, record := range clientIdRecords {
		filter.healthCounts[clientId] = ProviderEgressHealthCounts{
			MeasuredAt:      modernGateTestNow.Add(-time.Minute),
			FirstMeasuredAt: modernGateTestNow.Add(-time.Hour),
			OKCount:         5,
			Total:           5,
		}
		if record != nil {
			filter.modernEvidence[clientId] = record
		}
	}
	return filter
}

// The default settings with the gate switched as given.
func modernGateTestSettings(enabled bool) *EgressIndexSettings {
	settings := DefaultEgressIndexSettings()
	settings.QualitySpeedRequiresModernProvider = enabled
	return settings
}

// The provider's decision as every pass makes it.
func modernGateTestDecide(filter providerCountFilter, clientId server.Id, settings *EgressIndexSettings) providerEgressDecision {
	countryCode := "us"
	return decideProviderEgress(filter.egressFacts(clientId, &countryCode, nil, nil, settings), false)
}

// Every outcome from the latest observation: none, each capability missing,
// both missing (the first test), both present, and either way too old.
func TestProviderModernEvidenceOutcomes(t *testing.T) {
	for _, c := range []struct {
		name    string
		record  *ProviderModernEvidenceRecord
		outcome string
	}{
		{name: "no row", record: nil, outcome: ProviderModernOutcomeNoEvidence},
		{name: "both", record: modernGateTestRecord(true, true, time.Hour), outcome: ProviderModernOutcomeModern},
		{name: "windows without post-quantum", record: modernGateTestRecord(false, true, time.Hour), outcome: ProviderModernOutcomeNotPostQuantum},
		{name: "post-quantum without windows", record: modernGateTestRecord(true, false, time.Hour), outcome: ProviderModernOutcomeNoResizableWindows},
		{name: "neither", record: modernGateTestRecord(false, false, time.Hour), outcome: ProviderModernOutcomeNotPostQuantum},
		{name: "both, too old", record: modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge+time.Hour), outcome: ProviderModernOutcomeStaleEvidence},
		{name: "neither, too old", record: modernGateTestRecord(false, false, ProviderModernEvidenceMaxAge+time.Hour), outcome: ProviderModernOutcomeStaleEvidence},
	} {
		if outcome := classifyProviderModernEvidence(c.record, modernGateTestNow); outcome != c.outcome {
			t.Errorf("%s: outcome %s, want %s", c.name, outcome, c.outcome)
		}
	}
	connect.AssertEqual(t, ProviderModernOutcomes, []string{"modern", "not_post_quantum", "no_resizable_windows", "no_evidence", "stale_evidence"})
}

// Evidence exactly ProviderModernEvidenceMaxAge old is stale, as a URL
// outcome exactly eight hours old is; a microsecond younger, the smallest
// step the column stores, it still decides.
func TestProviderModernEvidenceStalenessBoundary(t *testing.T) {
	connect.AssertEqual(t, ProviderModernEvidenceMaxAge, 7*24*time.Hour)
	connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge), modernGateTestNow), ProviderModernOutcomeStaleEvidence)
	connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge-time.Microsecond), modernGateTestNow), ProviderModernOutcomeModern)
	connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRecord(false, true, ProviderModernEvidenceMaxAge-time.Microsecond), modernGateTestNow), ProviderModernOutcomeNotPostQuantum)
	// evidence measured just ahead of the evaluating clock is current
	connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRecord(true, true, -time.Second), modernGateTestNow), ProviderModernOutcomeModern)
}

// With the gate off, the default, every provider keeps exactly the buckets
// the other rules give it, whatever its evidence says: Quality, Speed and
// Online for a passing ratio, Speed and Online for an ARIN non-quality one.
func TestModernProviderGateOffLeavesTheBucketsUnchanged(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	connect.AssertEqual(t, settings.QualitySpeedRequiresModernProvider, false)
	records := map[string]*ProviderModernEvidenceRecord{
		ProviderModernOutcomeModern:             modernGateTestRecord(true, true, time.Hour),
		ProviderModernOutcomeNotPostQuantum:     modernGateTestRecord(false, true, time.Hour),
		ProviderModernOutcomeNoResizableWindows: modernGateTestRecord(true, false, time.Hour),
		ProviderModernOutcomeNoEvidence:         nil,
		ProviderModernOutcomeStaleEvidence:      modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge),
	}
	for outcome, record := range records {
		for _, arinNonQuality := range []bool{false, true} {
			clientId := server.NewId()
			filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{clientId: record})
			filter.arinNonQuality[clientId] = arinNonQuality
			decision := modernGateTestDecide(filter, clientId, settings)
			wantReason := ""
			if arinNonQuality {
				wantReason = ProviderExcludedArinNonQuality
			}
			if decision.reason != wantReason || decision.quality == arinNonQuality || !decision.speed || !decision.online || !decision.counted || decision.hardExcluded {
				t.Errorf("%s, arin non-quality %t: decision %+v, want the other rules' buckets", outcome, arinNonQuality, decision)
			}
			connect.AssertEqual(t, decision.modernOutcome, outcome)
			score := onlineSuccessTestRankedScore(filter, clientId, decision, settings)
			if score.PassesMinimums[RankModeQuality] == arinNonQuality || !score.PassesMinimums[RankModeSpeed] || !score.Online {
				t.Errorf("%s, arin non-quality %t: published native %v online %t", outcome, arinNonQuality, score.PassesMinimums, score.Online)
			}
		}
	}
}

// With the gate on, a provider the probe has not shown to be modern leaves
// Quality and Speed under not_modern and stays Online and counted. It is no
// hard exclusion, so probes, explicit client ids and force_minimum still
// reach it. A modern provider keeps its buckets, an ARIN non-quality one
// Speed alone; not_modern comes after the rules that already exclude both
// native buckets and before arin_non_quality.
func TestModernProviderGateOnKeepsOnlyModernProvidersInQualityAndSpeed(t *testing.T) {
	settings := modernGateTestSettings(true)
	for outcome, record := range map[string]*ProviderModernEvidenceRecord{
		ProviderModernOutcomeNotPostQuantum:     modernGateTestRecord(false, true, time.Hour),
		ProviderModernOutcomeNoResizableWindows: modernGateTestRecord(true, false, time.Hour),
		ProviderModernOutcomeNoEvidence:         nil,
		ProviderModernOutcomeStaleEvidence:      modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge),
	} {
		for _, arinNonQuality := range []bool{false, true} {
			clientId := server.NewId()
			filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{clientId: record})
			filter.arinNonQuality[clientId] = arinNonQuality
			decision := modernGateTestDecide(filter, clientId, settings)
			if decision.reason != ProviderExcludedNotModern || decision.quality || decision.speed || !decision.online || !decision.counted || decision.hardExcluded {
				t.Errorf("%s, arin non-quality %t: decision %+v, want online-only for not_modern", outcome, arinNonQuality, decision)
			}
			if filter.hasHardEgressFailure(clientId) {
				t.Errorf("%s: the gate became a common hard exclusion, which stops probes and explicit requests", outcome)
			}
			score := onlineSuccessTestRankedScore(filter, clientId, decision, settings)
			if score.PassesMinimums[RankModeQuality] || score.PassesMinimums[RankModeSpeed] || !score.Online {
				t.Errorf("%s, arin non-quality %t: published native %v online %t, want online only", outcome, arinNonQuality, score.PassesMinimums, score.Online)
			}
		}
	}

	modern, modernNonQuality := server.NewId(), server.NewId()
	filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{
		modern:           modernGateTestRecord(true, true, time.Hour),
		modernNonQuality: modernGateTestRecord(true, true, time.Hour),
	})
	filter.arinNonQuality[modernNonQuality] = true
	if decision := modernGateTestDecide(filter, modern, settings); decision.reason != "" || !decision.quality || !decision.speed || !decision.online {
		t.Errorf("modern provider decision %+v, want every bucket", decision)
	}
	if decision := modernGateTestDecide(filter, modernNonQuality, settings); decision.reason != ProviderExcludedArinNonQuality || decision.quality || !decision.speed || !decision.online {
		t.Errorf("modern ARIN non-quality provider decision %+v, want speed and online", decision)
	}

	// the rules that already take a provider out of both native buckets
	// keep their reasons
	unprobed, failing, unreliable := server.NewId(), server.NewId(), server.NewId()
	filter = modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{failing: nil, unreliable: nil})
	filter.healthCounts[failing] = ProviderEgressHealthCounts{MeasuredAt: modernGateTestNow.Add(-time.Minute), FirstMeasuredAt: modernGateTestNow.Add(-time.Hour), OKCount: 3, Total: 5}
	filter.reliabilityFailed[unreliable] = true
	for clientId, reason := range map[server.Id]string{unprobed: ProviderExcludedUnprobed, failing: ProviderExcludedHealth, unreliable: ProviderExcludedReliability} {
		if decision := modernGateTestDecide(filter, clientId, settings); decision.reason != reason || decision.quality || decision.speed {
			t.Errorf("decision %+v, want %s unchanged by the gate", decision, reason)
		}
	}

	// facts gathered without the evidence know no evidence
	quality := true
	decision := decideProviderEgress(&providerEgressFacts{egressQuality: &quality, modernGate: true}, false)
	if decision.reason != ProviderExcludedNotModern || decision.modernOutcome != ProviderModernOutcomeNoEvidence {
		t.Errorf("facts without evidence decided %+v, want not_modern for no evidence", decision)
	}
}

// The shadow counts, over the providers the other rules admit to each native
// bucket, every outcome, and read the same with the gate off and on. With the
// gate on, everything but modern is what it takes out, under not_modern.
func TestModernProviderGateShadowCountsTheProvidersEachBucketAdmits(t *testing.T) {
	modern, notPostQuantum, noWindows, noEvidence, stale := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	modernNonQuality, notModernNonQuality, unprobed, unreliable := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	filter := modernGateTestFilter(map[server.Id]*ProviderModernEvidenceRecord{
		modern:              modernGateTestRecord(true, true, time.Hour),
		notPostQuantum:      modernGateTestRecord(false, false, time.Hour),
		noWindows:           modernGateTestRecord(true, false, time.Hour),
		noEvidence:          nil,
		stale:               modernGateTestRecord(true, true, ProviderModernEvidenceMaxAge+time.Minute),
		modernNonQuality:    modernGateTestRecord(true, true, time.Hour),
		notModernNonQuality: modernGateTestRecord(false, true, time.Hour),
		unreliable:          modernGateTestRecord(true, true, time.Hour),
	})
	filter.arinNonQuality[modernNonQuality] = true
	filter.arinNonQuality[notModernNonQuality] = true
	filter.reliabilityFailed[unreliable] = true
	clientIds := []server.Id{modern, notPostQuantum, noWindows, noEvidence, stale, modernNonQuality, notModernNonQuality, unprobed, unreliable}

	wantShadow := map[string]map[string]int64{
		RankModeQuality: {
			ProviderModernOutcomeModern:             1,
			ProviderModernOutcomeNotPostQuantum:     1,
			ProviderModernOutcomeNoResizableWindows: 1,
			ProviderModernOutcomeNoEvidence:         1,
			ProviderModernOutcomeStaleEvidence:      1,
		},
		RankModeSpeed: {
			ProviderModernOutcomeModern:             2,
			ProviderModernOutcomeNotPostQuantum:     2,
			ProviderModernOutcomeNoResizableWindows: 1,
			ProviderModernOutcomeNoEvidence:         1,
			ProviderModernOutcomeStaleEvidence:      1,
		},
	}
	for _, c := range []struct {
		enabled   bool
		quality   int
		speed     int
		online    int
		notModern int64
	}{
		// every provider but the unreliable one is online, the unprobed one
		// included
		{enabled: false, quality: 5, speed: 7, online: 8, notModern: 0},
		{enabled: true, quality: 1, speed: 2, online: 8, notModern: 5},
	} {
		settings := modernGateTestSettings(c.enabled)
		shadow := newProviderModernGateShadowCounts()
		reasonCounts := map[string]int64{}
		quality, speed, online := 0, 0, 0
		for _, clientId := range clientIds {
			decision := modernGateTestDecide(filter, clientId, settings)
			countProviderModernGateShadow(shadow, decision)
			reasonCounts[decision.reason] += 1
			if decision.quality {
				quality += 1
			}
			if decision.speed {
				speed += 1
			}
			if decision.online {
				online += 1
			}
		}
		connect.AssertEqual(t, shadow, wantShadow)
		if quality != c.quality || speed != c.speed || online != c.online || reasonCounts[ProviderExcludedNotModern] != c.notModern {
			t.Errorf("gate %t: quality %d speed %d online %d not_modern %d, want %d %d %d %d", c.enabled, quality, speed, online, reasonCounts[ProviderExcludedNotModern], c.quality, c.speed, c.online, c.notModern)
		}
	}
}

// One log line per export: the gate's state and, per bucket, the providers
// admitted, the modern ones and every outcome that would take one out.
func TestModernProviderGateShadowSummary(t *testing.T) {
	shadow := newProviderModernGateShadowCounts()
	shadow[RankModeQuality][ProviderModernOutcomeModern] = 6
	shadow[RankModeQuality][ProviderModernOutcomeNoEvidence] = 3
	shadow[RankModeQuality][ProviderModernOutcomeNotPostQuantum] = 1
	shadow[RankModeSpeed][ProviderModernOutcomeModern] = 7
	shadow[RankModeSpeed][ProviderModernOutcomeStaleEvidence] = 2
	connect.AssertEqual(
		t,
		providerModernGateShadowSummary(shadow, false),
		"off, shadow only, evidence max age 168h0m0s: quality 10 admitted, 6 modern, 4 not (not_post_quantum 1, no_resizable_windows 0, no_evidence 3, stale_evidence 0); speed 9 admitted, 7 modern, 2 not (not_post_quantum 0, no_resizable_windows 0, no_evidence 0, stale_evidence 2)",
	)
	if !strings.HasPrefix(providerModernGateShadowSummary(shadow, true), "on, evidence max age") {
		t.Errorf("the summary does not say the gate is on: %s", providerModernGateShadowSummary(shadow, true))
	}
}

// provider.yml switches the gate with its own key. A value that is not true
// or false, the key given twice, or a file that is not yaml leaves the gate
// off, and a malformed switch resets no other key; a malformed neighbour
// leaves a valid switch on.
func TestEgressIndexSettingsModernProviderGate(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}
	connect.AssertEqual(t, load("").QualitySpeedRequiresModernProvider, false)
	connect.AssertEqual(t, load("quality_speed_requires_modern_provider: true\n").QualitySpeedRequiresModernProvider, true)
	connect.AssertEqual(t, load("quality_speed_requires_modern_provider: false\n").QualitySpeedRequiresModernProvider, false)

	neighbours := "online_requires_url_success: false\nonline_success_country_floor: 0.5\negress_index:\n  native_reader_enabled: true\n  max_failure_index: 9\n"
	for _, config := range []string{
		"quality_speed_requires_modern_provider: \"true\"\n",
		"quality_speed_requires_modern_provider: 1\n",
		"quality_speed_requires_modern_provider: [true]\n",
		"quality_speed_requires_modern_provider: true\nquality_speed_requires_modern_provider: true\n",
	} {
		settings := load(config + neighbours)
		if settings.QualitySpeedRequiresModernProvider {
			t.Errorf("%q switched the gate on", config)
		}
		if settings.OnlineRequiresUrlSuccess || settings.OnlineSuccessCountryFloor != 0.5 || !settings.NativeReaderEnabled || settings.MaxFailureIndex != 9 {
			t.Errorf("%q reset another key: rule %t floor %v native_reader_enabled %t max_failure_index %d", config, settings.OnlineRequiresUrlSuccess, settings.OnlineSuccessCountryFloor, settings.NativeReaderEnabled, settings.MaxFailureIndex)
		}
	}
	// a tab makes the whole file unreadable
	connect.AssertEqual(t, load("egress_index:\n\tnative_reader_enabled: true\nquality_speed_requires_modern_provider: true\n").QualitySpeedRequiresModernProvider, false)
	// malformed neighbours leave a valid switch on
	settings := load("quality_speed_requires_modern_provider: true\nonline_requires_url_success: \"false\"\nonline_success_country_floor: 36%\negress_index:\n  max_failure_index: \"nine\"\n")
	connect.AssertEqual(t, settings.QualitySpeedRequiresModernProvider, true)
}

// The ingest records nothing for evidence that observed nothing, and refuses
// evidence the table must not hold before any database work: a provider
// without an id, an observation from the future past the clock skew, a
// negative window, or a key exchange name the column cannot store.
func TestRecordProviderModernEvidenceRefusesBeforeTheDatabase(t *testing.T) {
	ctx := context.Background()
	clientId := server.NewId()
	if err := RecordProviderModernEvidence(ctx, clientId, ProviderModernEvidence{E2eNegotiated: true, PostQuantum: true, WindowAdvertised: true}); err != nil {
		t.Fatalf("evidence that observed nothing: %v", err)
	}
	observed := ProviderModernEvidence{
		E2eNegotiated:             true,
		KeyExchange:               "X25519MLKEM768",
		PostQuantum:               true,
		WindowAdvertised:          true,
		MaxReceiveWindowByteCount: 4 * 1024 * 1024,
		ObservedAt:                server.NowUtc().Add(time.Hour),
	}
	if RecordProviderModernEvidence(ctx, server.Id{}, observed) == nil {
		t.Error("evidence without a client id was accepted")
	}
	if RecordProviderModernEvidence(ctx, clientId, observed) == nil {
		t.Error("evidence observed an hour ahead was accepted")
	}

	now := modernGateTestNow
	valid := observed
	valid.ObservedAt = now
	for _, c := range []struct {
		name   string
		change func(*ProviderModernEvidence)
		valid  bool
	}{
		{name: "now", change: func(*ProviderModernEvidence) {}, valid: true},
		{name: "at the skew", change: func(e *ProviderModernEvidence) { e.ObservedAt = now.Add(time.Minute) }, valid: true},
		{name: "past the skew", change: func(e *ProviderModernEvidence) { e.ObservedAt = now.Add(time.Minute + time.Microsecond) }, valid: false},
		{name: "negative window", change: func(e *ProviderModernEvidence) { e.MaxReceiveWindowByteCount = -1 }, valid: false},
		{name: "longest key exchange", change: func(e *ProviderModernEvidence) { e.KeyExchange = strings.Repeat("k", 64) }, valid: true},
		{name: "key exchange too long", change: func(e *ProviderModernEvidence) { e.KeyExchange = strings.Repeat("k", 65) }, valid: false},
		{name: "key exchange not utf-8", change: func(e *ProviderModernEvidence) { e.KeyExchange = "X25519\xff" }, valid: false},
	} {
		evidence := valid
		c.change(&evidence)
		if err := evidence.validate(now); (err == nil) != c.valid {
			t.Errorf("%s: validate = %v, want valid %t", c.name, err, c.valid)
		}
	}
}

// A provider the gate takes out is told why and what to do, keeps its mode
// numbers (it is still in Online), and with the gate off or with modern
// evidence is told nothing of it.
func TestProviderStatusExplainsNotModern(t *testing.T) {
	facts := testingProviderStatusFacts()
	filter := testingProviderStatusFilter(facts)
	statusOf := func(settings *EgressIndexSettings, filter providerCountFilter) *ProviderStatus {
		evaluation := evaluateProviderStatus(facts, filter, settings, false, testingProviderStatusMinimums)
		return newProviderStatus(facts, evaluation, filter, settings, testingProviderStatusMinimums, testingProviderStatusNow)
	}

	status := statusOf(modernGateTestSettings(true), filter)
	connect.AssertEqual(t, status.Reason, ProviderStatusReasonNotModern)
	for _, part := range []string{"post-quantum", "resizable transfer windows", "update the app"} {
		if !strings.Contains(status.ReasonText, part) {
			t.Errorf("reason text %q does not say %q", status.ReasonText, part)
		}
	}
	connect.AssertEqual(t, status.Admission.Egress, ProviderEgressPass)
	weight := testingProviderStatusNumber(status, ProviderStatusNumberWeightQuality)
	if weight == nil || weight.Passes || !strings.Contains(weight.Explanation, "Not in the quality pool") {
		t.Errorf("quality weight %+v, want an online-only explanation", weight)
	}

	connect.AssertEqual(t, statusOf(modernGateTestSettings(false), filter).Reason, ProviderStatusReasonNone)
	filter.modernEvidence = map[server.Id]*ProviderModernEvidenceRecord{facts.clientId: {
		PostQuantum:     true,
		KeyExchange:     "X25519MLKEM768",
		WindowResizable: true,
		MeasuredAt:      testingProviderStatusNow.Add(-time.Hour),
		FirstMeasuredAt: testingProviderStatusNow.Add(-time.Hour),
		RunCount:        1,
	}}
	connect.AssertEqual(t, statusOf(modernGateTestSettings(true), filter).Reason, ProviderStatusReasonNone)
}
