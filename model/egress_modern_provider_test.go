package model

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The modern provider gate (egress_modern_provider.go), pure: the outcome a
// provider's evidence gives, the decision with the gate off and on, the flip
// at its start, the shadow counts, the switch and the start in provider.yml,
// the evidence the ingest refuses and the provider status.

// A fixed evaluation time, so no test reads the clock. It is before the
// gate's default start.
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
	return modernGateTestFilterAt(modernGateTestNow, clientIdRecords)
}

// modernGateTestFilter for a pass that decides at now.
func modernGateTestFilterAt(now time.Time, clientIdRecords map[server.Id]*ProviderModernEvidenceRecord) providerCountFilter {
	filter := providerCountFilter{
		now:                     now,
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
			MeasuredAt:      now.Add(-time.Minute),
			FirstMeasuredAt: now.Add(-time.Hour),
			OKCount:         5,
			Total:           5,
		}
		if record != nil {
			filter.modernEvidence[clientId] = record
		}
	}
	return filter
}

// The default settings with the switch set as given, which decides the gate
// whatever the time.
func modernGateTestSettings(enabled bool) *EgressIndexSettings {
	settings := DefaultEgressIndexSettings()
	settings.QualitySpeedRequiresModernProvider = &enabled
	return settings
}

// The default settings with the switch absent and the gate's start at start.
func modernGateTestScheduledSettings(start time.Time) *EgressIndexSettings {
	settings := DefaultEgressIndexSettings()
	settings.QualitySpeedRequiresModernProvider = nil
	settings.QualitySpeedModernProviderStart = start
	return settings
}

// An evidence row measured an hour before at: both capabilities as given.
func modernGateTestRecordAt(at time.Time, postQuantum bool, windowResizable bool) *ProviderModernEvidenceRecord {
	record := modernGateTestRecord(postQuantum, windowResizable, 0)
	record.MeasuredAt = at.Add(-time.Hour)
	record.FirstMeasuredAt = at.Add(-2 * time.Hour)
	return record
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

// With the gate off, as the default is before its start, every provider
// keeps exactly the buckets the other rules give it, whatever its evidence
// says: Quality, Speed and Online for a passing ratio, Speed and Online for an
// ARIN non-quality one.
func TestModernProviderGateOffLeavesTheBucketsUnchanged(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	if settings.QualitySpeedRequiresModernProvider != nil || settings.RequiresModernProvider(modernGateTestNow) {
		t.Fatalf("the default settings hold the gate on at %s: switch %v, start %s", modernGateTestNow, settings.QualitySpeedRequiresModernProvider, settings.QualitySpeedModernProviderStart)
	}
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

	// facts gathered without the evidence know no evidence (the URL runs
	// behind the passing ratio meet both native minimums)
	quality := true
	decision := decideProviderEgress(&providerEgressFacts{egressQuality: &quality, qualityRunsMet: true, speedRunsMet: true, modernGate: true}, false)
	if decision.reason != ProviderExcludedNotModern || decision.modernOutcome != ProviderModernOutcomeNoEvidence {
		t.Errorf("facts without evidence decided %+v, want not_modern for no evidence", decision)
	}
}

// The flip itself (owner decision 2026-10-15). With the switch absent, a
// provider the probe has not shown to be modern keeps Quality and Speed a
// microsecond, the smallest step a stored time takes, before the default
// start, and is not_modern from exactly the start. The switch decides
// whatever the time: false keeps the gate off at and long after the start,
// true holds it on before. A modern provider keeps both buckets throughout.
func TestModernProviderGateFlipsAtItsStart(t *testing.T) {
	start := DefaultQualitySpeedModernProviderStart()
	connect.AssertEqual(t, start, time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, DefaultEgressIndexSettings().QualitySpeedModernProviderStart, start)
	off, on := false, true
	for _, c := range []struct {
		name         string
		modernSwitch *bool
		now          time.Time
		gate         bool
	}{
		{name: "absent, a microsecond before the start", modernSwitch: nil, now: start.Add(-time.Microsecond), gate: false},
		{name: "absent, at the start", modernSwitch: nil, now: start, gate: true},
		{name: "absent, an hour after the start", modernSwitch: nil, now: start.Add(time.Hour), gate: true},
		{name: "false, a microsecond before the start", modernSwitch: &off, now: start.Add(-time.Microsecond), gate: false},
		{name: "false, at the start", modernSwitch: &off, now: start, gate: false},
		{name: "false, a year after the start", modernSwitch: &off, now: start.AddDate(1, 0, 0), gate: false},
		{name: "true, a day before the start", modernSwitch: &on, now: start.AddDate(0, 0, -1), gate: true},
		{name: "true, a microsecond before the start", modernSwitch: &on, now: start.Add(-time.Microsecond), gate: true},
		{name: "true, at the start", modernSwitch: &on, now: start, gate: true},
	} {
		settings := DefaultEgressIndexSettings()
		settings.QualitySpeedRequiresModernProvider = c.modernSwitch
		connect.AssertEqual(t, settings.RequiresModernProvider(c.now), c.gate)

		legacy, modern := server.NewId(), server.NewId()
		filter := modernGateTestFilterAt(c.now, map[server.Id]*ProviderModernEvidenceRecord{
			legacy: modernGateTestRecordAt(c.now, false, true),
			modern: modernGateTestRecordAt(c.now, true, true),
		})
		decision := modernGateTestDecide(filter, legacy, settings)
		connect.AssertEqual(t, decision.modernOutcome, ProviderModernOutcomeNotPostQuantum)
		if c.gate {
			if decision.reason != ProviderExcludedNotModern || decision.quality || decision.speed || !decision.online || !decision.counted || decision.hardExcluded {
				t.Errorf("%s: decision %+v, want online-only for not_modern", c.name, decision)
			}
		} else if decision.reason != "" || !decision.quality || !decision.speed || !decision.online || !decision.counted {
			t.Errorf("%s: decision %+v, want every bucket", c.name, decision)
		}
		if decision := modernGateTestDecide(filter, modern, settings); decision.reason != "" || !decision.quality || !decision.speed || !decision.online {
			t.Errorf("%s: modern provider decision %+v, want every bucket", c.name, decision)
		}
	}
}

// A pass decides at its filter's instant, never at a clock read of its own:
// with the switch absent, a pass whose instant is the start holds the gate
// on and one a microsecond before holds it off. The two starts lie far on
// either side of any clock that runs this test, so a decision read from the
// clock gets one of them wrong whatever the day. The export's log line
// reports the same decision from the same instant.
func TestModernProviderGateDecidesAtThePassInstant(t *testing.T) {
	for _, start := range []time.Time{
		time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		settings := modernGateTestScheduledSettings(start)
		for _, c := range []struct {
			now  time.Time
			gate bool
		}{
			{now: start.Add(-time.Microsecond), gate: false},
			{now: start, gate: true},
		} {
			legacy := server.NewId()
			filter := modernGateTestFilterAt(c.now, map[server.Id]*ProviderModernEvidenceRecord{
				legacy: modernGateTestRecordAt(c.now, false, false),
			})
			decision := modernGateTestDecide(filter, legacy, settings)
			if (decision.reason == ProviderExcludedNotModern) != c.gate || decision.quality == c.gate || decision.speed == c.gate || !decision.online {
				t.Errorf("start %s, pass at %s: decision %+v, want the gate on %t", start, c.now, decision, c.gate)
			}

			shadow := newProviderModernGateShadowCounts()
			countProviderModernGateShadow(shadow, decision)
			wantState := "off, shadow only ("
			if c.gate {
				wantState = "on ("
			}
			if summary := providerModernGateShadowSummary(shadow, settings, filter.now); !strings.HasPrefix(summary, wantState) {
				t.Errorf("start %s, pass at %s: log line %q, want it to begin %q", start, c.now, summary, wantState)
			}
		}
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

// One log line per export: the gate's state at the pass's instant, how
// provider.yml sets it and, per bucket, the providers admitted, the modern
// ones and every outcome that would take one out.
func TestModernProviderGateShadowSummary(t *testing.T) {
	shadow := newProviderModernGateShadowCounts()
	shadow[RankModeQuality][ProviderModernOutcomeModern] = 6
	shadow[RankModeQuality][ProviderModernOutcomeNoEvidence] = 3
	shadow[RankModeQuality][ProviderModernOutcomeNotPostQuantum] = 1
	shadow[RankModeSpeed][ProviderModernOutcomeModern] = 7
	shadow[RankModeSpeed][ProviderModernOutcomeStaleEvidence] = 2
	start := DefaultQualitySpeedModernProviderStart()
	connect.AssertEqual(
		t,
		providerModernGateShadowSummary(shadow, DefaultEgressIndexSettings(), modernGateTestNow),
		"off, shadow only (quality_speed_requires_modern_provider absent, on from 2026-10-21T00:00:00Z), evidence max age 168h0m0s: quality 10 admitted, 6 modern, 4 not (not_post_quantum 1, no_resizable_windows 0, no_evidence 3, stale_evidence 0); speed 9 admitted, 7 modern, 2 not (not_post_quantum 0, no_resizable_windows 0, no_evidence 0, stale_evidence 2)",
	)
	for _, c := range []struct {
		settings *EgressIndexSettings
		now      time.Time
		prefix   string
	}{
		{settings: DefaultEgressIndexSettings(), now: start, prefix: "on (quality_speed_requires_modern_provider absent, on from 2026-10-21T00:00:00Z), evidence max age"},
		{settings: modernGateTestSettings(true), now: modernGateTestNow, prefix: "on (quality_speed_requires_modern_provider: true), evidence max age"},
		{settings: modernGateTestSettings(false), now: start, prefix: "off, shadow only (quality_speed_requires_modern_provider: false), evidence max age"},
	} {
		if summary := providerModernGateShadowSummary(shadow, c.settings, c.now); !strings.HasPrefix(summary, c.prefix) {
			t.Errorf("summary %q, want it to begin %q", summary, c.prefix)
		}
	}
}

// The schedule the stats collector publishes for the monitor: whether the
// gate is on at the given instant and, only while the switch is absent, the
// start it turns on at.
func TestGetModernProviderGateSchedule(t *testing.T) {
	start := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		config         string
		now            time.Time
		on             bool
		scheduledStart *time.Time
	}{
		{config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start.Add(-time.Microsecond), on: false, scheduledStart: &start},
		{config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start, on: true, scheduledStart: &start},
		{config: "quality_speed_requires_modern_provider: false\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", now: start, on: false, scheduledStart: nil},
		{config: "quality_speed_requires_modern_provider: true\n", now: start.AddDate(-1, 0, 0), on: true, scheduledStart: nil},
		{config: "quality_speed_requires_modern_provider: maybe\n", now: start, on: false, scheduledStart: nil},
		{config: "quality_speed_requires_modern_provider: ~\n", now: start, on: false, scheduledStart: nil},
		{config: "quality_speed_modern_provider_start: next week\n", now: start, on: false, scheduledStart: nil},
	} {
		on, scheduledStart := func() (bool, *time.Time) {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
			defer pop()
			return GetModernProviderGateSchedule(c.now)
		}()
		if on != c.on || (scheduledStart == nil) != (c.scheduledStart == nil) || scheduledStart != nil && !scheduledStart.Equal(*c.scheduledStart) {
			t.Errorf("%q at %s: on %t start %v, want %t %v", c.config, c.now, on, scheduledStart, c.on, c.scheduledStart)
		}
	}
}

// A provider.yml that holds the gate off by a failure names it, a fixed text
// without the value given: a file that is not yaml, a switch that cannot be
// read or has no value, and, while the switch is absent, a start that cannot
// be used. The switch set on purpose, a usable start, no gate keys, and an
// unusable start beside a switch, which decides, name none.
func TestGetModernProviderGateFailure(t *testing.T) {
	for _, c := range []struct {
		config  string
		failure string
	}{
		{config: "", failure: ""},
		{config: "quality_speed_requires_modern_provider: false\n", failure: ""},
		{config: "quality_speed_requires_modern_provider: true\n", failure: ""},
		{config: "quality_speed_modern_provider_start: 2026-11-02\n", failure: ""},
		{config: "quality_speed_requires_modern_provider: false\nquality_speed_modern_provider_start: next week\n", failure: ""},
		{config: "egress_index:\n\tmax_failure_index: 3\n", failure: "provider.yml cannot be read"},
		{config: "quality_speed_requires_modern_provider: maybe\n", failure: "quality_speed_requires_modern_provider cannot be read"},
		{config: "quality_speed_requires_modern_provider: ~\n", failure: "quality_speed_requires_modern_provider has no value"},
		{config: "quality_speed_modern_provider_start: next week\n", failure: "quality_speed_modern_provider_start is neither an RFC 3339 instant nor a date"},
		{config: "quality_speed_modern_provider_start:\n", failure: "quality_speed_modern_provider_start has no value"},
		{config: "quality_speed_modern_provider_start: [2026-11-02]\n", failure: "quality_speed_modern_provider_start cannot be read"},
	} {
		failure := func() string {
			pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
			defer pop()
			return GetModernProviderGateFailure()
		}()
		if failure != c.failure {
			t.Errorf("%q: failure %q, want %q", c.config, failure, c.failure)
		}
	}
}

// The switch as a test names it: absent, true or false.
func modernGateTestSwitchText(modernSwitch *bool) string {
	if modernSwitch == nil {
		return "absent"
	}
	return strconv.FormatBool(*modernSwitch)
}

// provider.yml sets the gate three ways (owner decision 2026-10-15): an
// absent switch leaves it to its start, false keeps it off and true holds it
// on. A switch that is not true or false, given twice, or given without a
// value (empty, ~ or null) reads as false: the last is most likely a
// postponement that failed, which read as absent turned the gate on at its
// start. quality_speed_modern_provider_start moves the start with an RFC 3339
// instant or a date, its 00:00 UTC; with the switch absent, a start given
// that cannot be read, given twice or given without a value reads as the
// switch false for the same reason, and beside a switch it changes nothing.
// A malformed key changes no neighbour, and a malformed neighbour leaves
// both. A file that is not yaml, here for a tab, reads as false with the
// default start.
func TestEgressIndexSettingsModernProviderGate(t *testing.T) {
	load := func(config string) *EgressIndexSettings {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(config))
		defer pop()
		return egressIndexSettings()
	}
	defaultStart := DefaultQualitySpeedModernProviderStart()
	moved := time.Date(2026, 11, 2, 12, 30, 0, 0, time.UTC)
	movedDate := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	off, on := false, true
	neighbours := "online_requires_url_success: false\nonline_success_country_floor: 0.5\negress_index:\n  native_reader_enabled: true\n  max_failure_index: 9\n"
	for _, c := range []struct {
		name         string
		config       string
		modernSwitch *bool
		start        time.Time
	}{
		{name: "absent", config: "", modernSwitch: nil, start: defaultStart},
		{name: "true", config: "quality_speed_requires_modern_provider: true\n", modernSwitch: &on, start: defaultStart},
		{name: "false", config: "quality_speed_requires_modern_provider: false\n", modernSwitch: &off, start: defaultStart},
		{name: "a quoted true", config: "quality_speed_requires_modern_provider: \"true\"\n", modernSwitch: &off, start: defaultStart},
		{name: "a number", config: "quality_speed_requires_modern_provider: 1\n", modernSwitch: &off, start: defaultStart},
		{name: "a list", config: "quality_speed_requires_modern_provider: [true]\n", modernSwitch: &off, start: defaultStart},
		{name: "the switch twice", config: "quality_speed_requires_modern_provider: true\nquality_speed_requires_modern_provider: true\n", modernSwitch: &off, start: defaultStart},
		{name: "a start", config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", modernSwitch: nil, start: moved},
		{name: "a quoted start", config: "quality_speed_modern_provider_start: \"2026-11-02T12:30:00Z\"\n", modernSwitch: nil, start: moved},
		{name: "a start with an offset", config: "quality_speed_modern_provider_start: 2026-11-02T14:30:00+02:00\n", modernSwitch: nil, start: moved},
		{name: "false and a start", config: "quality_speed_requires_modern_provider: false\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", modernSwitch: &off, start: moved},
		{name: "a switch without a value", config: "quality_speed_requires_modern_provider:\n", modernSwitch: &off, start: defaultStart},
		{name: "a switch of ~", config: "quality_speed_requires_modern_provider: ~\n", modernSwitch: &off, start: defaultStart},
		{name: "a switch of null", config: "quality_speed_requires_modern_provider: null\n", modernSwitch: &off, start: defaultStart},
		{name: "a switch without a value and a start", config: "quality_speed_requires_modern_provider:\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", modernSwitch: &off, start: moved},
		{name: "a date", config: "quality_speed_modern_provider_start: 2026-11-02\n", modernSwitch: nil, start: movedDate},
		{name: "a quoted date", config: "quality_speed_modern_provider_start: \"2026-11-02\"\n", modernSwitch: nil, start: movedDate},
		{name: "a start that is not a time", config: "quality_speed_modern_provider_start: next week\n", modernSwitch: &off, start: defaultStart},
		{name: "a time without a zone", config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00\n", modernSwitch: &off, start: defaultStart},
		{name: "a start in a list", config: "quality_speed_modern_provider_start: [2026-11-02T12:30:00Z]\n", modernSwitch: &off, start: defaultStart},
		{name: "the start twice", config: "quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", modernSwitch: &off, start: defaultStart},
		{name: "a start without a value", config: "quality_speed_modern_provider_start:\n", modernSwitch: &off, start: defaultStart},
		{name: "a start of null", config: "quality_speed_modern_provider_start: null\n", modernSwitch: &off, start: defaultStart},
		{name: "true and a malformed start", config: "quality_speed_requires_modern_provider: true\nquality_speed_modern_provider_start: soon\n", modernSwitch: &on, start: defaultStart},
		{name: "true and a start without a value", config: "quality_speed_requires_modern_provider: true\nquality_speed_modern_provider_start:\n", modernSwitch: &on, start: defaultStart},
		{name: "a malformed switch and a start", config: "quality_speed_requires_modern_provider: yes please\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n", modernSwitch: &off, start: moved},
	} {
		settings := load(c.config + neighbours)
		if modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider) != modernGateTestSwitchText(c.modernSwitch) || !settings.QualitySpeedModernProviderStart.Equal(c.start) {
			t.Errorf("%s: switch %s start %s, want %s %s", c.name, modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider), settings.QualitySpeedModernProviderStart, modernGateTestSwitchText(c.modernSwitch), c.start)
		}
		if settings.OnlineRequiresUrlSuccess || settings.OnlineSuccessCountryFloor != 0.5 || !settings.NativeReaderEnabled || settings.MaxFailureIndex != 9 {
			t.Errorf("%s reset another key: rule %t floor %v native_reader_enabled %t max_failure_index %d", c.name, settings.OnlineRequiresUrlSuccess, settings.OnlineSuccessCountryFloor, settings.NativeReaderEnabled, settings.MaxFailureIndex)
		}
	}

	// a tab makes the whole file unreadable: the gate is off for good, the
	// start is the default, and the URL-success rule is off as well
	settings := load("egress_index:\n\tnative_reader_enabled: true\nquality_speed_requires_modern_provider: true\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n")
	if modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider) != "false" || !settings.QualitySpeedModernProviderStart.Equal(defaultStart) || settings.RequiresModernProvider(defaultStart.AddDate(1, 0, 0)) || settings.OnlineRequiresUrlSuccess {
		t.Errorf("a file that is not yaml: switch %s start %s rule %t, want false at the default start and the rule off", modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider), settings.QualitySpeedModernProviderStart, settings.OnlineRequiresUrlSuccess)
	}
	// malformed neighbours leave a valid switch and start
	settings = load("quality_speed_requires_modern_provider: true\nquality_speed_modern_provider_start: 2026-11-02T12:30:00Z\nonline_requires_url_success: \"false\"\nonline_success_country_floor: 36%\negress_index:\n  max_failure_index: \"nine\"\n")
	if modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider) != "true" || !settings.QualitySpeedModernProviderStart.Equal(moved) {
		t.Errorf("malformed neighbours: switch %s start %s, want true %s", modernGateTestSwitchText(settings.QualitySpeedRequiresModernProvider), settings.QualitySpeedModernProviderStart, moved)
	}
	// an absent switch decides by the start read, that instant included
	settings = load("quality_speed_modern_provider_start: 2026-11-02T12:30:00Z\n")
	connect.AssertEqual(t, settings.RequiresModernProvider(moved.Add(-time.Microsecond)), false)
	connect.AssertEqual(t, settings.RequiresModernProvider(moved), true)
	connect.AssertEqual(t, settings.RequiresModernProvider(defaultStart), false)
	// a date starts the gate at its 00:00 UTC
	settings = load("quality_speed_modern_provider_start: 2026-11-02\n")
	connect.AssertEqual(t, settings.RequiresModernProvider(movedDate.Add(-time.Microsecond)), false)
	connect.AssertEqual(t, settings.RequiresModernProvider(movedDate), true)
	// a failed postponement holds the gate off past the default start
	for _, config := range []string{"quality_speed_requires_modern_provider:\n", "quality_speed_modern_provider_start: next week\n", "quality_speed_modern_provider_start: ~\n"} {
		if load(config).RequiresModernProvider(defaultStart.AddDate(0, 0, 7)) {
			t.Errorf("%q held the gate on after the default start", config)
		}
	}
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

// A provider status request decides the gate at its own instant, the filter's,
// as a pass does: with the switch absent, not_modern from the start on and
// nothing of it a microsecond before.
func TestProviderStatusDecidesTheModernGateAtItsInstant(t *testing.T) {
	facts := testingProviderStatusFacts()
	filter := testingProviderStatusFilter(facts)
	statusOf := func(settings *EgressIndexSettings) *ProviderStatus {
		evaluation := evaluateProviderStatus(facts, filter, settings, false, testingProviderStatusMinimums)
		return newProviderStatus(facts, evaluation, filter, settings, testingProviderStatusMinimums, testingProviderStatusNow)
	}
	connect.AssertEqual(t, statusOf(modernGateTestScheduledSettings(filter.now)).Reason, ProviderStatusReasonNotModern)
	connect.AssertEqual(t, statusOf(modernGateTestScheduledSettings(filter.now.Add(time.Microsecond))).Reason, ProviderStatusReasonNone)
}
