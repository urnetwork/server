package model

import (
	"context"
	"math"
	mathrand "math/rand/v2"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/lagdrift"

	"github.com/urnetwork/server/v2026"
)

// Tests of the calibrated region size (lag.dev/DRIFTTEST.md V11): the
// histograms the weekly calibration keeps, the k a calibration gives each
// continent from 26 weeks of them, and lagdrift.DefaultK where it has too
// few.

// A Rayleigh sample: a city point's Mahalanobis distance from its fit when
// the fit's covariance is right, scaled.
func testLagDriftRayleighHistogram(rng *mathrand.Rand, count int, scale float64) *LagDriftHistogram {
	histogram := newLagDriftHistogram(LagDriftKEdges)
	for range count {
		histogram.add(LagDriftKEdges, scale*math.Sqrt(-2*math.Log(1-rng.Float64())))
	}
	histogram.Tracks = count
	return histogram
}

// A histogram's bound on a quantile is at or above the true quantile, by
// little more than a bin on a large sample: for a Rayleigh population the
// textbook k times its scale (drift-review V-3). Pure.
func TestLagDriftHistogramUpperBound(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(7, 7))
	for _, scale := range []float64{1.0, 1.3, 2.4} {
		histogram := testLagDriftRayleighHistogram(rng, 200000, scale)
		for _, confidence := range LagDriftKConfidences {
			want := scale * lagdrift.KForConfidence(confidence)
			got, ok := lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, confidence)
			connect.AssertEqual(t, ok, true)
			if !(want <= got && got < want+0.1) {
				t.Fatalf("scale %g at %g: bound %.4f, want from %.4f to a bin or two above", scale, confidence, got, want)
			}
		}
	}

	counts := make([]int64, len(LagDriftKEdges)+1)
	// everything below the first edge: that bin's upper edge
	counts[0] = 10000
	bound, ok := lagDriftHistogramUpperBound(LagDriftKEdges, counts, 0.99)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, bound, LagDriftKEdges[0])
	// a rank from the last edge up: unusable, beyond what the histogram tells
	counts[len(counts)-1] = 900
	_, ok = lagDriftHistogramUpperBound(LagDriftKEdges, counts, 0.99)
	connect.AssertEqual(t, ok, false)
	// 100 tracks can't bound a 99% quantile: its rank, 101, is past them
	small := make([]int64, len(LagDriftKEdges)+1)
	small[0] = 100
	_, ok = lagDriftHistogramUpperBound(LagDriftKEdges, small, 0.99)
	connect.AssertEqual(t, ok, false)
	// nothing: no bound
	_, ok = lagDriftHistogramUpperBound(LagDriftKEdges, make([]int64, len(LagDriftKEdges)+1), 0.99)
	connect.AssertEqual(t, ok, false)
}

// Over repeated samples of 10,000 tracks, the size at which a continent's k
// first counts, the bound is at or above the true quantile about 95% of the
// time even with the quantile on a bin's edge, where rounding up to the edge
// helps least; an estimate at the quantile's own rank would be below it about
// half the time (drift-review V-3). 400 samples hold 95% to about ±1%, so the
// test asks for 92%. Pure.
func TestLagDriftHistogramUpperBoundCoverage(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(11, 11))
	const samples = 400
	for _, confidence := range LagDriftKConfidences {
		// the true quantile on a bin's edge, as the worst case for rounding up
		scale := 6 / lagdrift.KForConfidence(confidence)
		want := scale * lagdrift.KForConfidence(confidence)
		covered := 0
		for range samples {
			histogram := testLagDriftRayleighHistogram(rng, LagDriftKMinTracks, scale)
			bound, ok := lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, confidence)
			connect.AssertEqual(t, ok, true)
			if want <= bound {
				covered += 1
			}
		}
		t.Logf("at %g: the bound covers the true quantile in %d of %d samples", confidence, covered, samples)
		if covered < samples*92/100 {
			t.Fatalf("at %g the bound covers the true quantile in %d of %d samples, want about 95%%", confidence, covered, samples)
		}
	}
}

// k is the fixed table, lagdrift.DefaultK, for the defaults and for a
// calibration alike, whatever region size it measured: a k measured on
// lag.dev's tracks doesn't carry over to a drift run (V11, review V-8). Pure.
func TestLagDriftCalibrationK(t *testing.T) {
	calibrationId := server.NewId()
	calibratedAt := time.Now().UTC()
	calibrated := DefaultLagDriftCalibration()
	calibrated.CalibrationId = &calibrationId
	calibrated.CalibratedAt = &calibratedAt
	calibrated.Kappa = 0.31
	calibrated.CalibratedK = map[string]*LagDriftCalibratedK{
		"EU": {Tracks: 50000, Quantiles: []float64{4.1, 6.2}},
		"OC": {Tracks: 50000, Quantiles: []float64{2.2, 2.9}},
		"SA": {Tracks: 20000, Quantiles: []float64{2.7, math.NaN()}},
	}
	for _, calibration := range []*LagDriftCalibration{DefaultLagDriftCalibration(), calibrated} {
		for _, continentCode := range append([]string{"", "eu", "oc", "XX"}, ContinentCodes...) {
			for _, confidence := range append([]float64{0.9}, LagDriftKConfidences...) {
				connect.AssertEqual(t, calibration.K(confidence, continentCode), lagdrift.DefaultK(confidence, continentCode))
			}
		}
	}
	// V11's table, from T3, never below the textbook k
	connect.AssertEqual(t, calibrated.K(0.99, "OC"), 7.88)
	connect.AssertEqual(t, calibrated.K(0.95, "AF"), 2.50)
	connect.AssertEqual(t, calibrated.K(0.99, "EU"), lagdrift.KForConfidence(0.99))
}

// A week's histograms by continent for the alarm tests: tracks at d, the
// rest below the first edge.
func testLagDriftKWeek(tracks int, d float64, share float64) *LagDriftHistogram {
	histogram := newLagDriftHistogram(LagDriftKEdges)
	atD := int(share * float64(tracks))
	for range atD {
		histogram.add(LagDriftKEdges, d)
	}
	histogram.Counts[0] += int64(tracks - atD)
	histogram.Tracks = tracks
	return histogram
}

// The alarms (V11): a week with 2,000 tracks and more than 2% of them beyond
// the trailing 26 weeks' 99% bound is a tail; the 26 weeks' 99% bound more
// than 15% from 26 weeks before, both with 10,000 tracks, is drift, as is one
// of the two beyond the histograms' last edge; the 26 weeks' bound beyond that
// edge is an overflow. Pure.
func TestLagDriftKAlarms(t *testing.T) {
	kinds := func(alarms []*LagDriftKAlarm) map[string][]string {
		out := map[string][]string{}
		for _, alarm := range alarms {
			out[alarm.ContinentCode] = append(out[alarm.ContinentCode], alarm.Kind)
		}
		return out
	}
	// the trailing 26 weeks: 1% at 4, so the 99% bound is 4.05
	trailing := map[string]*LagDriftHistogram{
		"EU": testLagDriftKWeek(26000, 4, 0.01),
		"NA": testLagDriftKWeek(26000, 4, 0.01),
		"AS": testLagDriftKWeek(26000, 4, 0.01),
		"AF": testLagDriftKWeek(26000, 4, 0.01),
	}
	bound, ok := lagDriftHistogramUpperBound(LagDriftKEdges, trailing["EU"].Counts, LagDriftKAlarmConfidence)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, bound, 4.05)
	week := map[string]*LagDriftHistogram{
		// 3% beyond: a tail
		"EU": testLagDriftKWeek(2500, 5, 0.03),
		// 2% beyond, not more than twice the expected: none
		"NA": testLagDriftKWeek(2500, 5, 0.02),
		// 10% beyond, but under 2,000 tracks: none
		"AS": testLagDriftKWeek(1999, 5, 0.10),
		// as far out as the trailing tail, at 4: within the bound, none
		"AF": testLagDriftKWeek(2500, 4, 0.05),
	}
	alarms := lagDriftKAlarms(week, trailing, map[string]*LagDriftHistogram{}, map[string]*LagDriftHistogram{})
	connect.AssertEqual(t, kinds(alarms), map[string][]string{"EU": {LagDriftKAlarmTail}})
	connect.AssertEqual(t, alarms[0].BoundK, 4.05)
	connect.AssertEqual(t, alarms[0].WeekTracks, int64(2500))
	if !(math.Abs(alarms[0].ShareBeyond-0.03) < 1e-9) {
		t.Fatalf("share beyond %v, want 0.03", alarms[0].ShareBeyond)
	}

	// the 26 weeks up to the week against the 26 weeks before them: bounds
	// of 4.05 and 3.05 at 1% beyond, and 12 and up beyond the last edge
	current := map[string]*LagDriftHistogram{
		// 4.05 from 3.6: moved 12.5%, none
		"EU": testLagDriftKWeek(12000, 4, 0.02),
		// 4.05 from 3.05: moved 33%, drift
		"NA": testLagDriftKWeek(12000, 4, 0.02),
		// moved, but the earlier 26 weeks have under 10,000 tracks: none
		"AS": testLagDriftKWeek(12000, 4, 0.02),
		// beyond the last edge from within it: drift, and an overflow
		"AF": testLagDriftKWeek(12000, 13, 0.02),
		// beyond the last edge, both times: an overflow only
		"OC": testLagDriftKWeek(12000, 13, 0.02),
		// beyond it, but under 10,000 tracks: none
		"SA": testLagDriftKWeek(9999, 13, 0.02),
	}
	earlier := map[string]*LagDriftHistogram{
		"EU": testLagDriftKWeek(12000, 3.55, 0.02),
		"NA": testLagDriftKWeek(12000, 3, 0.02),
		"AS": testLagDriftKWeek(9999, 3, 0.02),
		"AF": testLagDriftKWeek(12000, 4, 0.02),
		"OC": testLagDriftKWeek(12000, 13, 0.02),
	}
	for continentCode, histogram := range map[string]*LagDriftHistogram{"EU": current["EU"], "EU earlier": earlier["EU"], "NA earlier": earlier["NA"]} {
		b, _ := lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, LagDriftKAlarmConfidence)
		t.Logf("%s: 99%% bound %.2f", continentCode, b)
	}
	alarms = lagDriftKAlarms(map[string]*LagDriftHistogram{}, map[string]*LagDriftHistogram{}, current, earlier)
	connect.AssertEqual(t, kinds(alarms), map[string][]string{
		"NA": {LagDriftKAlarmDrift},
		"AF": {LagDriftKAlarmDrift, LagDriftKAlarmOverflow},
		"OC": {LagDriftKAlarmOverflow},
	})
	for _, alarm := range alarms {
		if alarm.ContinentCode == "NA" {
			connect.AssertEqual(t, alarm.BoundK, 4.05)
			connect.AssertEqual(t, alarm.EarlierBoundK, 3.05)
			connect.AssertEqual(t, alarm.Tracks, int64(12000))
			connect.AssertEqual(t, alarm.EarlierTracks, int64(12000))
		}
	}

	// nothing to compare: no alarm
	connect.AssertEqual(t, len(lagDriftKAlarms(nil, nil, nil, nil)), 0)
}

// Of a population drawn from the calibrated model, the city points' distances
// from their tracks' fits are near the textbook region's but somewhat wider,
// as T3 found real fits' tails: the fit's covariance carries no city error,
// which leans conservative, and a single run's linearized covariance is short
// in a thin tail. Visitors whose connections end farther from their city
// points than the model allows widen it further, which is what V11
// calibrates. Pure.
func TestCalibrateLagDriftTracksRegionSize(t *testing.T) {
	quantiles := func(result *LagDriftCalibrationResult, continentCode string) (float64, float64) {
		histogram := result.KHistograms[continentCode]
		k95, ok95 := lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, 0.95)
		k99, ok99 := lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, 0.99)
		connect.AssertEqual(t, ok95 && ok99, true)
		return k95, k99
	}
	population := func(cityErrorKm float64) *testLagDriftPopulation {
		return &testLagDriftPopulation{
			kappa:        0.25,
			sigma0Ms:     1.0,
			cityErrorKm:  cityErrorKm,
			placeErrorKm: 50,
			lastMileMs: map[string][2]float64{
				LagDriftNetworkTypeFixed: {2, 25},
			},
		}
	}

	rng := mathrand.New(mathrand.NewPCG(8, 8))
	result := CalibrateLagDriftTracks(testLagDriftCalibrationTracks(rng, 3000, population(30)), DefaultLagDriftCalibrationSettings())
	connect.AssertEqual(t, result.Applied, true)
	// every track has 8 to 12 extenders, so every fit counts, by continent
	connect.AssertEqual(t, result.KHistograms["EU"].Tracks+result.KHistograms["NA"].Tracks, 3000-result.UnfittedTracks)
	for _, continentCode := range []string{"EU", "NA"} {
		histogram := result.KHistograms[continentCode]
		sum := int64(0)
		for _, count := range histogram.Counts {
			sum += count
		}
		connect.AssertEqual(t, sum, int64(histogram.Tracks))
		connect.AssertEqual(t, result.KImpossibleTracks[continentCode], 0)
	}
	k95, k99 := quantiles(result, "EU")
	t.Logf("calibrated model: k 95%% %.3f, 99%% %.3f", k95, k99)
	textbook95 := lagdrift.KForConfidence(0.95)
	textbook99 := lagdrift.KForConfidence(0.99)
	if !(0.95*textbook95 < k95 && k95 < 1.25*textbook95 && 0.95*textbook99 < k99 && k99 < 1.5*textbook99) {
		t.Fatalf("a calibrated model's region size %.3f, %.3f, want near the textbook's %.3f, %.3f", k95, k99, textbook95, textbook99)
	}

	// connections ending 250 km from their city points, where the model
	// allows 30: the tail widens further
	rng = mathrand.New(mathrand.NewPCG(9, 9))
	wide := CalibrateLagDriftTracks(testLagDriftCalibrationTracks(rng, 3000, population(250)), DefaultLagDriftCalibrationSettings())
	wide95, wide99 := quantiles(wide, "EU")
	t.Logf("city points 250 km off: k 95%% %.3f, 99%% %.3f", wide95, wide99)
	if !(k95+0.4 < wide95 && k99+0.5 < wide99) {
		t.Fatalf("city points 250 km off give region size %.3f, %.3f, not wider than %.3f, %.3f", wide95, wide99, k95, k99)
	}
}

// A track whose city point light rules out from two or more extenders is a
// place the test exists to reject, such as a VPN's virtual location, and is
// counted apart from the region size's histogram; one extender out of reach
// may be misplaced, and the track still counts. Pure.
func TestCalibrateLagDriftTracksRegionSizeLeavesOutUnreachableCities(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(10, 10))
	tracks := testLagDriftCalibrationTracks(rng, 600, &testLagDriftPopulation{
		kappa:        0.25,
		sigma0Ms:     1.0,
		cityErrorKm:  30,
		placeErrorKm: 50,
		lastMileMs: map[string][2]float64{
			LagDriftNetworkTypeFixed: {2, 25},
		},
	})
	// a fastest ping of 1 ms puts the visitor within 150 km of an extender,
	// plus its place error and 3 s_c, 290 km in all: an extender over 500 km
	// from the city point puts it out of reach. Answers how many it changed
	outOfReach := func(track *LagDriftCalibrationTrack, count int) int {
		changed := 0
		for i := range track.Observations {
			if changed == count {
				break
			}
			if 500 < lagdrift.DistanceKm(track.City, track.Observations[i].At) {
				track.Observations[i].RoundTrip.MinMs = 1
				changed += 1
			}
		}
		return changed
	}
	expected := map[string]int{}
	left := 0
	for i, track := range tracks {
		switch i % 10 {
		case 0:
			if outOfReach(track, 2) == 2 {
				expected[track.ContinentCode] += 1
				left += 1
			}
		case 1:
			connect.AssertEqual(t, outOfReach(track, 1), 1)
		}
	}
	connect.AssertEqual(t, 50 < left, true)
	result := CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
	for _, continentCode := range []string{"EU", "NA"} {
		connect.AssertEqual(t, result.KImpossibleTracks[continentCode], expected[continentCode])
	}
	connect.AssertEqual(t, result.UnfittedTracks, 0)
	connect.AssertEqual(t, result.KHistograms["EU"].Tracks+result.KHistograms["NA"].Tracks, 600-left)
}

// Stores a calibration whose week has the given region-size histograms.
func testAddLagDriftKCalibration(ctx context.Context, calibratedAt time.Time, histograms map[string]*LagDriftHistogram) server.Id {
	result := testLagDriftCalibrationResult(0.25, 1.0, 70)
	result.KEdges = LagDriftKEdges
	result.KHistograms = histograms
	return AddLagDriftCalibration(ctx, calibratedAt, calibratedAt.Add(-7*24*time.Hour), calibratedAt, result)
}

// A calibration's measured region size sums the region-size histograms of
// the calibrations in force over the 26 weeks up to it, at each confidence's
// upper bound: a withdrawn one's count for nothing, and one older than the
// window for nothing. k stays the fixed table throughout.
func TestGetLagDriftCalibrationRegionSize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		week := testLagDriftKWeek
		add := func(calibratedAt time.Time, histograms map[string]*LagDriftHistogram) server.Id {
			return testAddLagDriftKCalibration(ctx, calibratedAt, histograms)
		}

		// OC: 4,000 tracks a week, 2% of them at a distance of 9. SA: as
		// many, 3% of them beyond the last edge
		add(now.Add(-27*7*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 11, 0.5)})
		add(now.Add(-14*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02), "SA": week(4000, 13, 0.03)})
		middle := add(now.Add(-7*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02), "EU": week(500, 4, 0.02), "SA": week(4000, 13, 0.03)})
		add(now, map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02), "SA": week(4000, 13, 0.03)})

		calibration := GetLagDriftCalibration(ctx)
		// the three weeks in the window, not the one 27 weeks old
		connect.AssertEqual(t, calibration.CalibratedK["OC"].Tracks, int64(12000))
		// 2% at 9: the 99% bound's rank, 11,898 of 12,000, is in the bin
		// [9, 9.05), and the bound is its upper edge; the 95% bound is below
		// the first edge, at it
		connect.AssertEqual(t, calibration.CalibratedK["OC"].Quantiles, []float64{1.5, 9.05})
		// SA's 99% bound is beyond the last edge: unusable
		connect.AssertEqual(t, math.IsNaN(calibration.CalibratedK["SA"].Quantiles[1]), true)
		connect.AssertEqual(t, calibration.CalibratedK["EU"].Tracks, int64(500))
		// none of it sets k
		for _, continentCode := range []string{"OC", "SA", "EU"} {
			for _, confidence := range LagDriftKConfidences {
				connect.AssertEqual(t, calibration.K(confidence, continentCode), lagdrift.DefaultK(confidence, continentCode))
			}
		}

		// withdrawing the middle week takes its histograms with it
		connect.AssertEqual(t, WithdrawLagDriftCalibration(ctx, middle, now), true)
		calibration = GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, calibration.CalibratedK["OC"].Tracks, int64(8000))
		_, ok := calibration.CalibratedK["EU"]
		connect.AssertEqual(t, ok, false)

		// a calibration read by its id sums its own 26 weeks
		byId := GetLagDriftCalibrationById(ctx, middle)
		connect.AssertEqual(t, byId.CalibratedK["OC"].Tracks, int64(4000))

		// every calibration withdrawn: the defaults
		WithdrawLagDriftCalibrations(ctx, now)
		calibration = GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, len(calibration.CalibratedK), 0)
		connect.AssertEqual(t, calibration.K(0.99, "OC"), lagdrift.DefaultK(0.99, "OC"))
	})
}

// A stored week's alarms compare it with the stored weeks: its tail against
// the 26 weeks before it, and the 26 weeks up to it against the 26 before
// those. A withdrawn calibration counts for nothing (V11).
func TestGetLagDriftKAlarms(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		weekAgo := func(weeks int) time.Time {
			return now.Add(-time.Duration(weeks) * 7 * 24 * time.Hour)
		}
		week := testLagDriftKWeek

		// 26 to 52 weeks ago: NA's tail at 3, EU's at 4
		testAddLagDriftKCalibration(ctx, weekAgo(40), map[string]*LagDriftHistogram{
			"NA": week(12000, 3, 0.01),
			"EU": week(12000, 4, 0.01),
		})
		// the 26 weeks before this one: both tails at 4
		testAddLagDriftKCalibration(ctx, weekAgo(10), map[string]*LagDriftHistogram{
			"NA": week(12000, 4, 0.01),
			"EU": week(12000, 4, 0.01),
		})
		// a withdrawn week, with a tail at 10, counts for nothing
		withdrawn := testAddLagDriftKCalibration(ctx, weekAgo(5), map[string]*LagDriftHistogram{
			"EU": week(12000, 10, 0.5),
		})
		connect.AssertEqual(t, WithdrawLagDriftCalibration(ctx, withdrawn, now), true)
		// this week: EU's tail far beyond the trailing bound, NA's within it
		latest := testAddLagDriftKCalibration(ctx, now, map[string]*LagDriftHistogram{
			"NA": week(2500, 4, 0.05),
			"EU": week(2500, 6, 0.05),
		})

		alarms := GetLagDriftKAlarms(ctx, latest)
		kinds := map[string][]string{}
		for _, alarm := range alarms {
			kinds[alarm.ContinentCode] = append(kinds[alarm.ContinentCode], alarm.Kind)
		}
		// EU: 5% of the week beyond the trailing 4.05, a tail. NA: its 26
		// weeks' bound moved from 3.05 to 4.05, drift; its week is at the
		// trailing tail, not beyond it
		connect.AssertEqual(t, kinds, map[string][]string{
			"EU": {LagDriftKAlarmTail},
			"NA": {LagDriftKAlarmDrift},
		})
		connect.AssertEqual(t, len(GetLagDriftKAlarms(ctx, server.NewId())), 0)
	})
}
