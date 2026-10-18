package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The modern provider gate against the database: the evidence upsert, and the
// gate through the passes in the order the taskworker runs them, the
// published samples, FindProviders2, the dashboard and location counts, the
// provider status and the inspection.

// Sets the gate's keys in the fixture's provider.yml until the test ends, a
// nil value removing its key, keeping the fixture's other provider settings.
func pushModernProviderGateConfig(t testing.TB, keyValues map[string]any) {
	t.Helper()
	config := map[string]any{}
	if resource, err := server.Config.SimpleResource(providerConfigResourceName); err == nil && resource != nil {
		parsed, err := resource.ParseE()
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range parsed {
			config[key] = value
		}
	}
	for key, value := range keyValues {
		if value == nil {
			delete(config, key)
		} else {
			config[key] = value
		}
	}
	encoded, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Config.PushSimpleResource(providerConfigResourceName, encoded))
}

// Switches the gate on in the fixture's provider.yml until the test ends,
// whatever the time, keeping the fixture's other provider settings.
func enableModernProviderGate(t testing.TB) {
	t.Helper()
	pushModernProviderGateConfig(t, map[string]any{"quality_speed_requires_modern_provider": true})
}

// The stored row of the provider, nil without one.
func modernGateTestRow(ctx context.Context, clientId server.Id) *ProviderModernEvidenceRecord {
	return getProviderModernEvidenceRecords(ctx, []server.Id{clientId})[clientId]
}

// Fails unless the stored row is want, times compared as instants.
func assertModernGateTestRow(t testing.TB, where string, row *ProviderModernEvidenceRecord, want ProviderModernEvidenceRecord) {
	t.Helper()
	if row == nil {
		t.Fatalf("%s: no row", where)
	}
	if row.PostQuantum != want.PostQuantum ||
		row.KeyExchange != want.KeyExchange ||
		row.WindowResizable != want.WindowResizable ||
		row.MaxReceiveWindowByteCount != want.MaxReceiveWindowByteCount ||
		!row.MeasuredAt.Equal(want.MeasuredAt) ||
		!row.FirstMeasuredAt.Equal(want.FirstMeasuredAt) ||
		row.RunCount != want.RunCount {
		t.Fatalf("%s: row %+v, want %+v", where, *row, want)
	}
}

// The evidence of a session observed at observedAt with both capabilities as
// given.
func modernGateTestEvidence(postQuantum bool, windowResizable bool, observedAt time.Time) ProviderModernEvidence {
	evidence := ProviderModernEvidence{
		E2eNegotiated:    true,
		KeyExchange:      "X25519",
		PostQuantum:      postQuantum,
		WindowAdvertised: windowResizable,
		ObservedAt:       observedAt,
	}
	if postQuantum {
		evidence.KeyExchange = "X25519MLKEM768"
	}
	if windowResizable {
		evidence.MaxReceiveWindowByteCount = 4 * 1024 * 1024
	}
	return evidence
}

// One row per provider keeps the latest observation: the first inserts it, a
// newer one replaces it and counts a run, a retried call changes nothing, an
// older one only moves first_measured_at earlier, a conflicting one at the
// same time keeps the less modern reading of each capability, and an
// inconsistent one is stored as the less modern reading. Evidence that
// observed nothing, or that is refused, writes nothing.
func TestRecordProviderModernEvidenceKeepsTheLatestObservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId, other := server.NewId(), server.NewId()
		// the column's precision, so the read-back compares exactly
		base := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)
		record := func(evidence ProviderModernEvidence) {
			t.Helper()
			connect.AssertEqual(t, RecordProviderModernEvidence(ctx, clientId, evidence), nil)
		}

		record(ProviderModernEvidence{E2eNegotiated: true, PostQuantum: true, WindowAdvertised: true})
		if row := modernGateTestRow(ctx, clientId); row != nil {
			t.Fatalf("evidence that observed nothing wrote %+v", *row)
		}

		first := ProviderModernEvidence{E2eNegotiated: true, KeyExchange: "X25519", WindowAdvertised: true, MaxReceiveWindowByteCount: 2 * 1024 * 1024, ObservedAt: base}
		record(first)
		wantFirst := ProviderModernEvidenceRecord{KeyExchange: "X25519", WindowResizable: true, MaxReceiveWindowByteCount: 2 * 1024 * 1024, MeasuredAt: base, FirstMeasuredAt: base, RunCount: 1}
		assertModernGateTestRow(t, "first", modernGateTestRow(ctx, clientId), wantFirst)
		record(first)
		assertModernGateTestRow(t, "retried", modernGateTestRow(ctx, clientId), wantFirst)

		newer := modernGateTestEvidence(true, true, base.Add(10*time.Minute))
		record(newer)
		wantNewer := ProviderModernEvidenceRecord{PostQuantum: true, KeyExchange: "X25519MLKEM768", WindowResizable: true, MaxReceiveWindowByteCount: 4 * 1024 * 1024, MeasuredAt: base.Add(10 * time.Minute), FirstMeasuredAt: base, RunCount: 2}
		assertModernGateTestRow(t, "newer", modernGateTestRow(ctx, clientId), wantNewer)
		connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRow(ctx, clientId), server.NowUtc()), ProviderModernOutcomeModern)

		// delayed behind the newer one
		record(ProviderModernEvidence{ObservedAt: base.Add(-30 * time.Minute)})
		wantOlder := wantNewer
		wantOlder.FirstMeasuredAt = base.Add(-30 * time.Minute)
		assertModernGateTestRow(t, "older", modernGateTestRow(ctx, clientId), wantOlder)

		// a different session observed at the same instant without
		// post-quantum and with a smaller window
		conflicting := ProviderModernEvidence{E2eNegotiated: true, KeyExchange: "X25519", WindowAdvertised: true, MaxReceiveWindowByteCount: 1024 * 1024, ObservedAt: base.Add(10 * time.Minute)}
		record(conflicting)
		wantConflicting := wantOlder
		wantConflicting.PostQuantum = false
		wantConflicting.KeyExchange = "X25519"
		wantConflicting.MaxReceiveWindowByteCount = 1024 * 1024
		assertModernGateTestRow(t, "conflicting", modernGateTestRow(ctx, clientId), wantConflicting)
		// the modern reading at the same instant again changes nothing
		record(newer)
		assertModernGateTestRow(t, "modern again at the same instant", modernGateTestRow(ctx, clientId), wantConflicting)

		// post-quantum needs an end-to-end session, and a window needs the
		// provider to advertise one
		record(ProviderModernEvidence{KeyExchange: "X25519MLKEM768", PostQuantum: true, MaxReceiveWindowByteCount: 8 * 1024 * 1024, ObservedAt: base.Add(20 * time.Minute)})
		assertModernGateTestRow(t, "inconsistent", modernGateTestRow(ctx, clientId), ProviderModernEvidenceRecord{
			MeasuredAt:      base.Add(20 * time.Minute),
			FirstMeasuredAt: base.Add(-30 * time.Minute),
			RunCount:        3,
		})
		connect.AssertEqual(t, classifyProviderModernEvidence(modernGateTestRow(ctx, clientId), server.NowUtc()), ProviderModernOutcomeNotPostQuantum)

		// refused evidence writes nothing, for this provider or another
		future := modernGateTestEvidence(true, true, server.NowUtc().Add(time.Hour))
		if RecordProviderModernEvidence(ctx, clientId, future) == nil || RecordProviderModernEvidence(ctx, other, future) == nil {
			t.Fatal("evidence observed an hour ahead was accepted")
		}
		connect.AssertEqual(t, modernGateTestRow(ctx, clientId).RunCount, 3)
		if row := modernGateTestRow(ctx, other); row != nil {
			t.Fatalf("refused evidence wrote %+v", *row)
		}

		// the fleet read holds every row, a scoped one only its own
		record(newer)
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, other, modernGateTestEvidence(false, false, base)), nil)
		connect.AssertEqual(t, len(getProviderModernEvidenceRecords(ctx, nil)), 2)
		connect.AssertEqual(t, len(getProviderModernEvidenceRecords(ctx, []server.Id{other, server.NewId()})), 1)
		connect.AssertEqual(t, len(getProviderModernEvidenceRecords(ctx, []server.Id{})), 0)
	})
}

// Shadow first, then on: before the gate's start every provider keeps its
// buckets and the dashboard counts say what the gate would take out. From the
// start, with the switch absent (on by time), and with
// `quality_speed_requires_modern_provider: true`, the providers the probe
// has not shown to be modern leave the native Quality and Speed samples
// under not_modern and answer only from the Online tier; they stay in the
// Online pool and the location counts, and force_minimum and an explicit
// client id still reach them. `quality_speed_requires_modern_provider:
// false` after the start turns the gate off again. The shadow reads the same
// throughout, and the provider status and inspection say why. The start is
// pinned in provider.yml, so the test reads the same whatever day it runs.
func TestModernProviderGateShadowFirstThenOnAcrossThePasses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the switch absent and the start far ahead: the passes decide before
		// the start
		pushModernProviderGateConfig(t, map[string]any{
			"quality_speed_requires_modern_provider": nil,
			"quality_speed_modern_provider_start":    "2100-01-01T00:00:00Z",
		})
		city := egressTestCity(ctx, "Palo Alto", "California", "United States", "us")

		modern := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, modern, city, 0, "us")
		legacy := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, legacy, city, 0, "us")
		unmeasured := egressTestConnect(ctx, t, city, egressTestFast, nil, nil)
		egressTestProbed(ctx, unmeasured, city, 0, "us")
		// the fixture's providers are modern; these two are not
		egressTestForgetModernEvidence(ctx, legacy.clientId, unmeasured.clientId)
		observedAt := server.NowUtc().Add(-time.Minute)
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, modern.clientId, modernGateTestEvidence(true, true, observedAt)), nil)
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, legacy.clientId, modernGateTestEvidence(false, false, observedAt)), nil)
		all := []*egressTestProvider{modern, legacy, unmeasured}
		notModern := []*egressTestProvider{legacy, unmeasured}

		wantShadow := newProviderModernGateShadowCounts()
		for _, bucket := range ProviderModernGateBuckets {
			wantShadow[bucket][ProviderModernOutcomeModern] = 1
			wantShadow[bucket][ProviderModernOutcomeNotPostQuantum] = 1
			wantShadow[bucket][ProviderModernOutcomeNoEvidence] = 1
		}
		bucketTotal := func(counts *ProviderEgressCounts, bucket string) int64 {
			total := int64(0)
			for _, count := range counts.BucketIndexCounts[bucket] {
				total += count
			}
			return total
		}

		// every provider in every bucket, and nothing of the gate in the
		// legacy provider's status
		assertGateOff := func(phase string) {
			t.Helper()
			counts := CountProviderEgress(ctx)
			connect.AssertEqual(t, counts.ModernGateShadowCounts, wantShadow)
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNotModern], int64(0))
			for _, bucket := range ProviderEgressBuckets {
				if total := bucketTotal(counts, bucket); total != 3 {
					t.Errorf("%s: %s counts %d providers, want 3", phase, bucket, total)
				}
			}
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
				for _, provider := range all {
					if score := clientScores[provider.clientId]; score == nil || !score.PassesMinimums[rankMode] {
						t.Errorf("%s, %s: with the gate off a provider left the native sample", phase, rankMode)
					}
				}
				for _, provider := range egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId()) {
					if provider.Tier != 0 {
						t.Errorf("%s, %s: with the gate off a provider answered from tier %d", phase, rankMode, provider.Tier)
					}
				}
			}
			connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 3)
			statuses := GetProviderStatuses(ctx, legacy.networkId, []server.Id{legacy.clientId})
			if len(statuses) != 1 || statuses[0].Reason == ProviderStatusReasonNotModern {
				t.Errorf("%s: status with the gate off: %+v", phase, statuses)
			}
			if inspection := InspectProviderEgress(ctx, legacy.clientId); inspection.ModernGate || inspection.Reason == ProviderExcludedNotModern || !inspection.Quality || !inspection.Speed {
				t.Errorf("%s: inspection with the gate off %+v", phase, inspection)
			}
		}

		// the providers the gate takes out are online-only and say why
		assertGateOn := func(phase string) {
			t.Helper()
			counts := CountProviderEgress(ctx)
			connect.AssertEqual(t, counts.ModernGateShadowCounts, wantShadow)
			connect.AssertEqual(t, counts.ReasonCounts[ProviderExcludedNotModern], int64(2))
			connect.AssertEqual(t, bucketTotal(counts, RankModeQuality), int64(1))
			connect.AssertEqual(t, bucketTotal(counts, RankModeSpeed), int64(1))
			connect.AssertEqual(t, bucketTotal(counts, ProviderEgressBucketOnline), int64(3))
			connect.AssertEqual(t, egressTestLocationCount(ctx, t, city), 3)

			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				clientScores := egressTestCachedScores(ctx, t, city, rankMode, false)
				if score := clientScores[modern.clientId]; score == nil || !score.PassesMinimums[rankMode] {
					t.Errorf("%s, %s: the modern provider left the native sample", phase, rankMode)
				}
				for _, provider := range notModern {
					if score := clientScores[provider.clientId]; score == nil || score.PassesMinimums[rankMode] || !score.Online {
						t.Errorf("%s, %s: a provider the gate takes out is not online-only in the sample", phase, rankMode)
					}
				}
				forced := egressTestCachedScores(ctx, t, city, rankMode, true)
				for _, provider := range all {
					if _, ok := forced[provider.clientId]; !ok {
						t.Errorf("%s, %s: the forced sample lost a provider", phase, rankMode)
					}
				}

				// native first, the rest from the Online tier
				providers := egressTestFind(ctx, t, egressTestLocationSpec(city), rankMode, 10, false, server.NewId())
				if len(providers) != 3 || providers[0].ClientId != modern.clientId || providers[0].Tier != 0 {
					t.Fatalf("%s, %s: answer %v, want the modern provider native and first", phase, rankMode, egressTestIds(providers))
				}
				for _, provider := range providers[1:] {
					connect.AssertEqual(t, provider.Tier, 2*egressTestBackfillOffset())
				}
			}
			online := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeOnline, 10, false, server.NewId()))
			for _, provider := range all {
				if !slices.Contains(online, provider.clientId) {
					t.Errorf("%s: an Online answer lost a provider the gate takes out of quality and speed", phase)
				}
			}
			forced := egressTestIds(egressTestFind(ctx, t, egressTestLocationSpec(city), RankModeSpeed, 10, true, server.NewId()))
			for _, provider := range all {
				if !slices.Contains(forced, provider.clientId) {
					t.Errorf("%s: force_minimum lost a provider the gate takes out", phase)
				}
			}
			named := egressTestFind(ctx, t, []*ProviderSpec{{ClientId: &legacy.clientId}}, RankModeSpeed, 1, false, server.NewId())
			connect.AssertEqual(t, egressTestIds(named), []server.Id{legacy.clientId})

			statuses := GetProviderStatuses(ctx, legacy.networkId, []server.Id{legacy.clientId})
			if len(statuses) != 1 || statuses[0].Reason != ProviderStatusReasonNotModern {
				t.Fatalf("%s: status of a provider the gate takes out: %+v", phase, statuses)
			}
			inspection := InspectProviderEgress(ctx, legacy.clientId)
			if inspection.ModernEvidence == nil || inspection.ModernOutcome != ProviderModernOutcomeNotPostQuantum || inspection.Reason != ProviderExcludedNotModern || inspection.Quality || inspection.Speed || !inspection.Online || !inspection.ModernGate {
				t.Fatalf("%s: inspection %+v, want the evidence, its outcome and the not_modern decision", phase, inspection)
			}
			inspection = InspectProviderEgress(ctx, unmeasured.clientId)
			if inspection.ModernEvidence != nil || inspection.ModernOutcome != ProviderModernOutcomeNoEvidence || inspection.Reason != ProviderExcludedNotModern {
				t.Fatalf("%s: inspection of the unmeasured provider %+v, want no evidence", phase, inspection)
			}
		}

		// before the start
		egressTestPasses(ctx, t)
		assertGateOff("before the start")
		offPolicyKey := ProviderEgressCountsPolicyKey(0)

		// on by time: the start moved to an instant the passes below come
		// after, the switch still absent
		start := server.NowUtc()
		pushModernProviderGateConfig(t, map[string]any{"quality_speed_modern_provider_start": start.Format(time.RFC3339Nano)})
		egressTestPasses(ctx, t)
		assertGateOn("on by time")
		if inspection := InspectProviderEgress(ctx, legacy.clientId); inspection.Settings.QualitySpeedRequiresModernProvider != nil || !inspection.Settings.QualitySpeedModernProviderStart.Equal(start) || inspection.ModernGateTime.Before(start) {
			t.Fatalf("on by time: inspection settings %+v at %s, want the switch absent and the start %s", inspection.Settings, inspection.ModernGateTime, start)
		}
		if ProviderEgressCountsPolicyKey(0) == offPolicyKey {
			t.Fatal("on by time: the dashboard counts kept the key they had before the start")
		}

		// the kill switch, after the start
		pushModernProviderGateConfig(t, map[string]any{"quality_speed_requires_modern_provider": false})
		egressTestPasses(ctx, t)
		assertGateOff("switched off after the start")
		connect.AssertEqual(t, ProviderEgressCountsPolicyKey(0), offPolicyKey)

		// switched on
		pushModernProviderGateConfig(t, map[string]any{"quality_speed_requires_modern_provider": true})
		egressTestPasses(ctx, t)
		assertGateOn("switched on")

		// evidence of both capabilities brings the provider back at the next
		// pass
		connect.AssertEqual(t, RecordProviderModernEvidence(ctx, legacy.clientId, modernGateTestEvidence(true, true, server.NowUtc())), nil)
		egressTestPasses(ctx, t)
		for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
			if score := egressTestCachedScores(ctx, t, city, rankMode, false)[legacy.clientId]; score == nil || !score.PassesMinimums[rankMode] {
				t.Errorf("%s: an upgraded provider did not return to the native sample", rankMode)
			}
		}
		connect.AssertEqual(t, CountProviderEgress(ctx).ReasonCounts[ProviderExcludedNotModern], int64(1))
	})
}
