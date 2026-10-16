package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The native URL-run minimums (egress_native_min_runs.go), pure: the facts a
// pass gathers from its loaded evidence, the decision every pass makes and the
// score UpdateClientScores publishes from it. Every expectation is written out
// rather than read from the minimums, and the tests read only what the rules
// had before them, so they run against the code without the minimums too.

// When the DB tests' seeded run index of total lands: the runs spread evenly
// over the forty minutes that end at measuredAt, the first earliest, as the
// probe paces real runs apart. Runs seeded at one instant span nothing, and
// Quality reads no verdict from them (egress_native_min_runs.go).
func egressTestRunAt(measuredAt time.Time, index int, total int) time.Time {
	if total <= 1 {
		return measuredAt
	}
	return measuredAt.Add(-time.Duration(total-1-index) * (40 * time.Minute) / time.Duration(total-1))
}

// `total` accepted measured URL runs, `ok` of them successes, the latest a
// minute before onlineSuccessTestNow and the earliest span before the latest.
func nativeMinRunsTestCounts(ok int, total int, span time.Duration) ProviderEgressHealthCounts {
	measuredAt := onlineSuccessTestNow.Add(-time.Minute)
	return ProviderEgressHealthCounts{
		MeasuredAt:      measuredAt,
		FirstMeasuredAt: measuredAt.Add(-span),
		OKCount:         ok,
		Total:           total,
	}
}

// The probe's pacing (DefaultProviderEgressRules): after a success the next
// run comes one success interval later, after a failure, or after a success of
// a cycle past its four hours whose quota is short, one failure interval
// later, each jittered by -10% to +10%.
func nativeMinRunsTestIntervals() (successInterval time.Duration, failureInterval time.Duration) {
	rules := DefaultProviderEgressRules()
	return time.Duration(rules.UrlSuccessIntervalSeconds) * time.Second, time.Duration(rules.UrlFailureIntervalSeconds) * time.Second
}

// The decision for one provider with counts, beside another provider measured
// well, so the pass's guard has a measured population.
func nativeMinRunsTestDecide(t *testing.T, counts ProviderEgressHealthCounts, settings *EgressIndexSettings) (providerCountFilter, server.Id, providerEgressDecision) {
	t.Helper()
	clientId, other := server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{clientId: counts, other: nativeMinRunsTestCounts(10, 10, 3*time.Hour)},
		map[server.Id]string{clientId: "us", other: "us"},
		settings,
	)
	return filter, clientId, onlineSuccessTestDecide(filter, clientId, "us", settings)
}

// The 2026-10-15 21:31 UTC defect: a provider of a fleet batch admitted to URL
// probing at 21:27:45 had exactly one accepted run, a success. 1/1 passes the
// 4/5 ratio, so it was published to Quality, at index 0, and to Speed on that
// one sample. One run is no native verdict: the provider stays online and
// counted, out of both native buckets as unprobed, in the decision and in the
// published score.
func TestNativeMinRunsOneSuccessIsNoQualityOrSpeedVerdict(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	filter, fleet, decision := nativeMinRunsTestDecide(t, nativeMinRunsTestCounts(1, 1, 0), settings)

	// the ratio alone still passes, at index 0: the minimums do not move it
	counts, urlIndex := filter.urlIndex(fleet, settings)
	if !urlIndex.Evidence || !urlIndex.Quality || urlIndex.Index != 0 || counts.Total != 1 {
		t.Fatalf("1/1 index %+v over %d runs, want evidence passing the ratio at index 0", urlIndex, counts.Total)
	}

	if decision.quality || decision.speed {
		t.Fatalf("1/1 provider admitted to quality=%t speed=%t on one URL run: %+v", decision.quality, decision.speed, decision)
	}
	if decision.reason != ProviderExcludedUnprobed || !decision.online || !decision.counted || decision.hardExcluded {
		t.Fatalf("1/1 provider decision %+v, want online and counted, unprobed, no hard exclusion", decision)
	}
	if filter.hasHardEgressFailure(fleet) {
		t.Fatal("the minimums became a common hard exclusion, which stops probes and explicit requests")
	}

	score := onlineSuccessTestRankedScore(filter, fleet, decision, settings)
	if !score.Online || score.PassesMinimums[RankModeQuality] || score.PassesMinimums[RankModeSpeed] {
		t.Fatalf("1/1 provider published online=%t native=%v, want online only", score.Online, score.PassesMinimums)
	}
	// the measured URL weight still ranks it within Online
	connect.AssertEqual(t, score.UrlProbeSuccessWeight, 1.0)
}

// A fleet batch beside measured providers, as one pass decides it: 437
// providers with one accepted run each, a success, and 236 measured over four
// hours, each passing the ratio. Without the minimums Quality and Speed take
// all 673; with them the batch is online and counted as unprobed, and the
// native buckets keep the measured providers alone.
func TestNativeMinRunsFleetBatchStaysOutOfQualityAndSpeed(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	clientIdCounts := map[server.Id]ProviderEgressHealthCounts{}
	clientIdCountryCodes := map[server.Id]string{}
	clientIds := []server.Id{}
	for range 437 {
		clientId := server.NewId()
		clientIdCounts[clientId] = nativeMinRunsTestCounts(1, 1, 0)
		clientIdCountryCodes[clientId] = "us"
		clientIds = append(clientIds, clientId)
	}
	for i := range 236 {
		clientId := server.NewId()
		// 8/10 to 10/10 over four hours, every one passing the ratio
		clientIdCounts[clientId] = nativeMinRunsTestCounts(8+i%3, 10, 4*time.Hour)
		clientIdCountryCodes[clientId] = "us"
		clientIds = append(clientIds, clientId)
	}
	filter := onlineSuccessTestFilter(clientIdCounts, clientIdCountryCodes, settings)

	quality, speed, online, counted := 0, 0, 0, 0
	reasons := map[string]int{}
	for _, clientId := range clientIds {
		decision := onlineSuccessTestDecide(filter, clientId, "us", settings)
		if decision.quality {
			quality += 1
		}
		if decision.speed {
			speed += 1
		}
		if decision.online {
			online += 1
		}
		if decision.counted {
			counted += 1
		}
		reasons[decision.reason] += 1
	}
	if quality != 236 || speed != 236 {
		t.Fatalf("quality %d speed %d, want 236 236: the single-success batch entered the native buckets", quality, speed)
	}
	connect.AssertEqual(t, online, 673)
	connect.AssertEqual(t, counted, 673)
	connect.AssertEqual(t, reasons[ProviderExcludedUnprobed], 437)
	connect.AssertEqual(t, reasons[""], 236)
}

// The run-count and span boundaries, every ratio passing: Speed needs two
// runs at any span; Quality needs three spanning thirty minutes, the boundary
// admitted. A failing ratio stays health at any count, and expired runs stay
// unprobed, as before the minimums.
func TestNativeMinRunsBoundaries(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	for _, c := range []struct {
		name           string
		counts         ProviderEgressHealthCounts
		quality, speed bool
		reason         string
	}{
		{name: "one run", counts: nativeMinRunsTestCounts(1, 1, 0), reason: ProviderExcludedUnprobed},
		{name: "one run, old enough", counts: nativeMinRunsTestCounts(1, 1, time.Hour), reason: ProviderExcludedUnprobed},
		{name: "two runs at once", counts: nativeMinRunsTestCounts(2, 2, 0), speed: true, reason: ProviderExcludedUnprobed},
		{name: "two runs over an hour", counts: nativeMinRunsTestCounts(2, 2, time.Hour), speed: true, reason: ProviderExcludedUnprobed},
		{name: "three runs at once", counts: nativeMinRunsTestCounts(3, 3, 0), speed: true, reason: ProviderExcludedUnprobed},
		{name: "three runs a nanosecond short of thirty minutes", counts: nativeMinRunsTestCounts(3, 3, 30*time.Minute-time.Nanosecond), speed: true, reason: ProviderExcludedUnprobed},
		{name: "three runs over thirty minutes", counts: nativeMinRunsTestCounts(3, 3, 30*time.Minute), quality: true, speed: true},
		{name: "ten runs in ten minutes", counts: nativeMinRunsTestCounts(10, 10, 10*time.Minute), speed: true, reason: ProviderExcludedUnprobed},
		{name: "four of five over an hour", counts: nativeMinRunsTestCounts(4, 5, time.Hour), quality: true, speed: true},
		// the ratio decides first: a failing share is health at any count
		{name: "one failed run", counts: nativeMinRunsTestCounts(0, 1, 0), reason: ProviderExcludedHealth},
		{name: "one of two", counts: nativeMinRunsTestCounts(1, 2, time.Hour), reason: ProviderExcludedHealth},
		{name: "two of three", counts: nativeMinRunsTestCounts(2, 3, time.Hour), reason: ProviderExcludedHealth},
		{name: "runs past the evidence window", counts: ProviderEgressHealthCounts{
			MeasuredAt:      onlineSuccessTestNow.Add(-ProviderEgressHealthMaxAge),
			FirstMeasuredAt: onlineSuccessTestNow.Add(-ProviderEgressHealthMaxAge - time.Hour),
			OKCount:         5,
			Total:           5,
		}, reason: ProviderExcludedUnprobed},
	} {
		t.Run(c.name, func(t *testing.T) {
			filter, clientId, decision := nativeMinRunsTestDecide(t, c.counts, settings)
			if decision.quality != c.quality || decision.speed != c.speed || decision.reason != c.reason || !decision.online || !decision.counted || decision.hardExcluded {
				t.Fatalf("decision %+v, want quality=%t speed=%t reason=%q, online and counted", decision, c.quality, c.speed, c.reason)
			}
			score := onlineSuccessTestRankedScore(filter, clientId, decision, settings)
			if score.PassesMinimums[RankModeQuality] != c.quality || score.PassesMinimums[RankModeSpeed] != c.speed || !score.Online {
				t.Fatalf("published native %v online %t, want quality=%t speed=%t online", score.PassesMinimums, score.Online, c.quality, c.speed)
			}
		})
	}
}

// The span boundary against the probe's pacing: three paced successes of a
// new provider reach Quality even at the shortest jitter, while one paced turn
// with its quick retries, or a burst of quick retries, never does, at the
// longest jitter.
func TestNativeMinRunsSpanFollowsTheProbePacing(t *testing.T) {
	settings := DefaultEgressIndexSettings()
	successInterval, failureInterval := nativeMinRunsTestIntervals()
	shortest := func(interval time.Duration) time.Duration { return interval * 9 / 10 }
	longest := func(interval time.Duration) time.Duration { return interval * 11 / 10 }
	for _, c := range []struct {
		name           string
		counts         ProviderEgressHealthCounts
		quality, speed bool
	}{
		// a new provider's cycle: each success paces the next by a success interval
		{name: "two paced successes", counts: nativeMinRunsTestCounts(2, 2, longest(successInterval)), speed: true},
		{name: "three paced successes", counts: nativeMinRunsTestCounts(3, 3, 2*shortest(successInterval)), quality: true, speed: true},
		// one paced turn and a quick retry after it, as a cycle past its four
		// hours with a short quota is retried even after a success
		{name: "a paced turn and a quick retry", counts: nativeMinRunsTestCounts(3, 3, longest(successInterval)+longest(failureInterval)), speed: true},
		{name: "a burst of quick retries", counts: nativeMinRunsTestCounts(10, 10, 9*longest(failureInterval)), speed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, _, decision := nativeMinRunsTestDecide(t, c.counts, settings)
			if decision.quality != c.quality || decision.speed != c.speed || !decision.online {
				t.Fatalf("decision %+v, want quality=%t speed=%t, online", decision, c.quality, c.speed)
			}
		})
	}
}

// Providers with enough runs are decided as without the minimums: by the
// ratio, ARIN non-quality and the modern provider gate.
func TestNativeMinRunsKeepMeasuredProvidersUnchanged(t *testing.T) {
	for _, c := range []struct {
		name           string
		counts         ProviderEgressHealthCounts
		arinNonQuality bool
		modernGate     bool
		modern         bool
		quality, speed bool
		reason         string
	}{
		{name: "a full rolling quota", counts: nativeMinRunsTestCounts(10, 10, 4*time.Hour), quality: true, speed: true},
		{name: "eight of ten", counts: nativeMinRunsTestCounts(8, 10, 4*time.Hour), quality: true, speed: true},
		{name: "seven of ten", counts: nativeMinRunsTestCounts(7, 10, 4*time.Hour), reason: ProviderExcludedHealth},
		{name: "a day's runs", counts: nativeMinRunsTestCounts(19, 20, 7*time.Hour), quality: true, speed: true},
		{name: "three runs over forty minutes", counts: nativeMinRunsTestCounts(3, 3, 40*time.Minute), quality: true, speed: true},
		{name: "arin non-quality", counts: nativeMinRunsTestCounts(10, 10, 4*time.Hour), arinNonQuality: true, speed: true, reason: ProviderExcludedArinNonQuality},
		{name: "modern gate, no evidence", counts: nativeMinRunsTestCounts(10, 10, 4*time.Hour), modernGate: true, reason: ProviderExcludedNotModern},
		{name: "modern gate, modern", counts: nativeMinRunsTestCounts(10, 10, 4*time.Hour), modernGate: true, modern: true, quality: true, speed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			settings := modernGateTestSettings(c.modernGate)
			clientId := server.NewId()
			filter := onlineSuccessTestFilter(
				map[server.Id]ProviderEgressHealthCounts{clientId: c.counts},
				map[server.Id]string{clientId: "us"},
				settings,
			)
			filter.arinNonQuality[clientId] = c.arinNonQuality
			filter.modernEvidence = map[server.Id]*ProviderModernEvidenceRecord{}
			if c.modern {
				record := modernGateTestRecord(true, true, 0)
				record.MeasuredAt = onlineSuccessTestNow.Add(-time.Hour)
				record.FirstMeasuredAt = onlineSuccessTestNow.Add(-2 * time.Hour)
				filter.modernEvidence[clientId] = record
			}
			decision := onlineSuccessTestDecide(filter, clientId, "us", settings)
			if decision.quality != c.quality || decision.speed != c.speed || decision.reason != c.reason || !decision.online || !decision.counted || decision.hardExcluded {
				t.Fatalf("decision %+v, want quality=%t speed=%t reason=%q, online and counted", decision, c.quality, c.speed, c.reason)
			}
			// a passing ratio puts it in the gate's population, as before
			if c.reason != ProviderExcludedHealth && (!decision.speedBeforeModernGate || decision.qualityBeforeModernGate != !c.arinNonQuality || decision.modernOutcome == "") {
				t.Fatalf("decision %+v, want the modern gate's population as without the minimums", decision)
			}
		})
	}
}

// The modern provider gate reads only the providers the other rules admit: a
// provider short for both buckets is outside the gate's population, with no
// gate outcome, and stays unprobed with the gate on. Two runs admit it to
// Speed before the gate, and from there the gate reads it like any Speed
// provider.
func TestNativeMinRunsComeBeforeTheModernProviderGate(t *testing.T) {
	settings := modernGateTestSettings(true)
	single, double, measured := server.NewId(), server.NewId(), server.NewId()
	filter := onlineSuccessTestFilter(
		map[server.Id]ProviderEgressHealthCounts{
			single:   nativeMinRunsTestCounts(1, 1, 0),
			double:   nativeMinRunsTestCounts(2, 2, 20*time.Minute),
			measured: nativeMinRunsTestCounts(10, 10, 4*time.Hour),
		},
		map[server.Id]string{single: "us", double: "us", measured: "us"},
		settings,
	)
	filter.modernEvidence = map[server.Id]*ProviderModernEvidenceRecord{}

	singleDecision := onlineSuccessTestDecide(filter, single, "us", settings)
	if singleDecision.reason != ProviderExcludedUnprobed || singleDecision.qualityBeforeModernGate || singleDecision.speedBeforeModernGate || singleDecision.modernOutcome != "" || !singleDecision.online || !singleDecision.counted {
		t.Fatalf("one run with the gate on: %+v, want unprobed outside the gate's population", singleDecision)
	}
	doubleDecision := onlineSuccessTestDecide(filter, double, "us", settings)
	if doubleDecision.reason != ProviderExcludedNotModern || doubleDecision.qualityBeforeModernGate || !doubleDecision.speedBeforeModernGate || doubleDecision.quality || doubleDecision.speed || doubleDecision.modernOutcome != ProviderModernOutcomeNoEvidence {
		t.Fatalf("two runs with the gate on and no evidence: %+v, want Speed before the gate, then not_modern", doubleDecision)
	}

	shadow := newProviderModernGateShadowCounts()
	for _, clientId := range []server.Id{single, double, measured} {
		countProviderModernGateShadow(shadow, onlineSuccessTestDecide(filter, clientId, "us", settings))
	}
	connect.AssertEqual(t, shadow[RankModeQuality][ProviderModernOutcomeNoEvidence], int64(1))
	connect.AssertEqual(t, shadow[RankModeSpeed][ProviderModernOutcomeNoEvidence], int64(2))
}

// The minimums fail closed: facts that say nothing of the URL runs behind a
// passing ratio, the zero value, admit the provider to neither native bucket.
// It stays online and counted as unprobed, outside the modern provider gate's
// population, until a pass gathers runs that meet a minimum.
func TestNativeMinRunsFailClosedWithoutRuns(t *testing.T) {
	passing := true
	decision := decideProviderEgress(&providerEgressFacts{egressQuality: &passing}, false)
	if decision.quality || decision.speed || decision.qualityBeforeModernGate || decision.speedBeforeModernGate || decision.modernOutcome != "" {
		t.Fatalf("a passing ratio with no runs stated was admitted: %+v", decision)
	}
	if decision.reason != ProviderExcludedUnprobed || !decision.online || !decision.counted || decision.hardExcluded {
		t.Fatalf("decision %+v, want online and counted, unprobed, no hard exclusion", decision)
	}
}

// Each minimum met admits its own bucket alone: the Speed minimum met keeps
// the provider out of Quality as unprobed, and both met admit both.
func TestNativeMinRunsEachMinimumAdmitsItsBucket(t *testing.T) {
	passing := true
	speedOnly := decideProviderEgress(&providerEgressFacts{egressQuality: &passing, speedRunsMet: true}, false)
	if speedOnly.quality || !speedOnly.speed || speedOnly.reason != ProviderExcludedUnprobed || !speedOnly.online || !speedOnly.counted {
		t.Fatalf("the Speed minimum met: %+v, want Speed alone, unprobed", speedOnly)
	}
	both := decideProviderEgress(&providerEgressFacts{egressQuality: &passing, qualityRunsMet: true, speedRunsMet: true}, false)
	if !both.quality || !both.speed || both.reason != "" || !both.online || !both.counted {
		t.Fatalf("both minimums met: %+v, want Quality and Speed", both)
	}
}
