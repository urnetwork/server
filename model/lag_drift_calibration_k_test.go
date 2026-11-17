package model

import (
	"context"
	"math"
	mathrand "math/rand/v2"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
)

// Tests of the calibrated region size (lag.dev/DRIFTTEST.md V11): the
// histograms the weekly calibration keeps, the k a calibration gives each
// continent from 26 weeks of them, and lagdrift.DefaultK where it has too
// few.

// A histogram's quantile, interpolated within its bin, is the sample's to
// the bin width: for a Rayleigh population, which a city point's Mahalanobis
// distance is under a calibrated fit, the textbook k times its scale. Pure.
func TestLagDriftHistogramQuantile(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(7, 7))
	for _, scale := range []float64{1.0, 1.3, 2.4} {
		histogram := newLagDriftHistogram(LagDriftKEdges)
		for range 200000 {
			histogram.add(LagDriftKEdges, scale*math.Sqrt(-2*math.Log(1-rng.Float64())))
		}
		for _, confidence := range LagDriftKConfidences {
			want := scale * lagdrift.KForConfidence(confidence)
			got := lagDriftHistogramQuantile(LagDriftKEdges, histogram.Counts, confidence)
			if !(math.Abs(got-want) < 0.03*want) {
				t.Fatalf("scale %g at %g: quantile %.4f, want %.4f", scale, confidence, got, want)
			}
		}
	}

	counts := make([]int64, len(LagDriftKEdges)+1)
	// everything below the first edge: inside it, from 0
	counts[0] = 100
	if got := lagDriftHistogramQuantile(LagDriftKEdges, counts, 0.99); !(math.Abs(got-0.99*LagDriftKEdges[0]) < 1e-12) {
		t.Fatalf("quantile below the first edge %v, want %v", got, 0.99*LagDriftKEdges[0])
	}
	// from the last edge up: the last edge, the most the histogram can tell
	counts[len(counts)-1] = 900
	connect.AssertEqual(t, lagDriftHistogramQuantile(LagDriftKEdges, counts, 0.99), LagDriftKEdges[len(LagDriftKEdges)-1])
	// nothing: no quantile
	connect.AssertEqual(t, math.IsNaN(lagDriftHistogramQuantile(LagDriftKEdges, make([]int64, len(LagDriftKEdges)+1), 0.99)), true)
}

// A continent's k is the larger of the textbook k and its calibrated
// quantile once its summed histograms hold 10,000 tracks, and else
// lagdrift.DefaultK; the defaults always answer DefaultK, as the drift test
// judges while calibration is switched off. Pure.
func TestLagDriftCalibrationK(t *testing.T) {
	textbook95 := lagdrift.KForConfidence(0.95)
	textbook99 := lagdrift.KForConfidence(0.99)

	calibration := DefaultLagDriftCalibration()
	for _, continentCode := range append([]string{""}, ContinentCodes...) {
		for _, confidence := range LagDriftKConfidences {
			connect.AssertEqual(t, calibration.K(confidence, continentCode), lagdrift.DefaultK(confidence, continentCode))
		}
	}
	// V11's defaults, from T3
	connect.AssertEqual(t, calibration.K(0.99, "OC"), 7.88)
	connect.AssertEqual(t, calibration.K(0.99, "EU"), textbook99)
	connect.AssertEqual(t, calibration.K(0.95, "AF"), 2.50)

	calibration.CalibratedK = map[string]*LagDriftCalibratedK{
		// enough tracks: the quantile, never below the textbook k
		"EU": {Tracks: 12000, Quantiles: []float64{2.3, 3.4}},
		// one short: the default
		"OC": {Tracks: LagDriftKMinTracks - 1, Quantiles: []float64{3.0, 9.5}},
		// enough, and narrower than T3 found: the calibration wins
		"AF": {Tracks: LagDriftKMinTracks, Quantiles: []float64{2.6, 3.3}},
	}
	connect.AssertEqual(t, calibration.K(0.95, "EU"), textbook95)
	connect.AssertEqual(t, calibration.K(0.99, "EU"), 3.4)
	connect.AssertEqual(t, calibration.K(0.99, "eu"), 3.4)
	connect.AssertEqual(t, calibration.K(0.99, "OC"), lagdrift.DefaultK(0.99, "OC"))
	connect.AssertEqual(t, calibration.K(0.95, "OC"), lagdrift.DefaultK(0.95, "OC"))
	connect.AssertEqual(t, calibration.K(0.95, "AF"), 2.6)
	connect.AssertEqual(t, calibration.K(0.99, "AF"), 3.3)
	// a continent with none: the default
	connect.AssertEqual(t, calibration.K(0.99, "NA"), lagdrift.DefaultK(0.99, "NA"))
	// a confidence V11 doesn't calibrate: the default, which is its textbook k
	connect.AssertEqual(t, calibration.K(0.9, "EU"), lagdrift.DefaultK(0.9, "EU"))
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
		return lagDriftHistogramQuantile(LagDriftKEdges, histogram.Counts, 0.95),
			lagDriftHistogramQuantile(LagDriftKEdges, histogram.Counts, 0.99)
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

// A calibration's k sums the region-size histograms of the calibrations in
// force over the 26 weeks up to it: a withdrawn one's count for nothing, and
// one older than the window for nothing. A continent counts from 10,000
// tracks; below, it has DefaultK.
func TestGetLagDriftCalibrationRegionSize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)

		// a week's histogram for a continent: tracks at d, the rest of them
		// below the first edge
		week := func(tracks int, d float64, share float64) *LagDriftHistogram {
			histogram := newLagDriftHistogram(LagDriftKEdges)
			atD := int(share * float64(tracks))
			for range atD {
				histogram.add(LagDriftKEdges, d)
			}
			histogram.Counts[0] += int64(tracks - atD)
			histogram.Tracks = tracks
			return histogram
		}
		add := func(calibratedAt time.Time, histograms map[string]*LagDriftHistogram) server.Id {
			result := testLagDriftCalibrationResult(0.25, 1.0, 70)
			result.KEdges = LagDriftKEdges
			result.KHistograms = histograms
			return AddLagDriftCalibration(ctx, calibratedAt, calibratedAt.Add(-7*24*time.Hour), calibratedAt, result)
		}

		// OC: 4,000 tracks a week, 2% of them at a distance of 9
		add(now.Add(-27*7*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 11, 0.5)})
		add(now.Add(-14*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02)})
		middle := add(now.Add(-7*24*time.Hour), map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02), "EU": week(500, 4, 0.02)})
		add(now, map[string]*LagDriftHistogram{"OC": week(4000, 9, 0.02)})

		calibration := GetLagDriftCalibration(ctx)
		// the three weeks in the window, not the one 27 weeks old
		connect.AssertEqual(t, calibration.CalibratedK["OC"].Tracks, int64(12000))
		// 2% at 9: the 99% quantile is in that bin
		if k := calibration.K(0.99, "OC"); !(9 <= k && k < 9.05) {
			t.Fatalf("OC's k at 99%% is %.3f, want in [9, 9.05)", k)
		}
		// the 95% quantile is below the first edge: the textbook k
		connect.AssertEqual(t, calibration.K(0.95, "OC"), lagdrift.KForConfidence(0.95))
		// EU's 500 tracks are too few: its default
		connect.AssertEqual(t, calibration.CalibratedK["EU"].Tracks, int64(500))
		connect.AssertEqual(t, calibration.K(0.99, "EU"), lagdrift.DefaultK(0.99, "EU"))

		// withdrawing the middle week takes its histograms with it: 8,000
		// tracks are too few, and OC goes back to its default
		connect.AssertEqual(t, WithdrawLagDriftCalibration(ctx, middle, now), true)
		calibration = GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, calibration.CalibratedK["OC"].Tracks, int64(8000))
		connect.AssertEqual(t, calibration.K(0.99, "OC"), lagdrift.DefaultK(0.99, "OC"))
		_, ok := calibration.CalibratedK["EU"]
		connect.AssertEqual(t, ok, false)

		// a calibration read by its id sums its own 26 weeks
		byId := GetLagDriftCalibrationById(ctx, middle)
		connect.AssertEqual(t, byId.CalibratedK["OC"].Tracks, int64(4000))

		// every calibration withdrawn: the defaults, DefaultK everywhere
		WithdrawLagDriftCalibrations(ctx, now)
		calibration = GetLagDriftCalibration(ctx)
		connect.AssertEqual(t, len(calibration.CalibratedK), 0)
		connect.AssertEqual(t, calibration.K(0.99, "OC"), lagdrift.DefaultK(0.99, "OC"))
	})
}
