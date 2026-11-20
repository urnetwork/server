package model

import (
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/lagdrift"
)

// Tests of the calibration's fit (lag.dev/DRIFTTEST.md V3, V4, K2): synthetic
// tracks drawn from the drift model with a known κ and σ₀ give them back, with
// or without the outliers the model has no term for, and the relay thresholds
// are the 99th percentile of b with a floor. Pure.

// The point distanceKm from p along an initial bearing, on the sphere.
func testLagDriftDestination(p lagdrift.Point, bearingDeg float64, distanceKm float64) lagdrift.Point {
	lat1 := p.Lat * math.Pi / 180
	lon1 := p.Lon * math.Pi / 180
	theta := bearingDeg * math.Pi / 180
	delta := distanceKm / lagdrift.EarthRadiusKm
	lat2 := math.Asin(math.Sin(lat1)*math.Cos(delta) + math.Cos(lat1)*math.Sin(delta)*math.Cos(theta))
	lon2 := lon1 + math.Atan2(math.Sin(theta)*math.Sin(delta)*math.Cos(lat1), math.Cos(delta)-math.Sin(lat1)*math.Sin(lat2))
	lon := math.Mod(lon2*180/math.Pi+540, 360) - 180
	return lagdrift.Point{Lat: lat2 * 180 / math.Pi, Lon: lon}
}

// How a synthetic population is drawn.
type testLagDriftPopulation struct {
	kappa    float64
	sigma0Ms float64
	// the visitors' connections end this far from their city points, 1σ on
	// each axis, km
	cityErrorKm float64
	// the extenders are this far from their listed places, 1σ on each axis
	placeErrorKm float64
	// the share of tracks whose city point is wrong by wrongCityKm
	wrongCityShare float64
	wrongCityKm    float64
	// the share of paths with a detour of detourMs
	detourShare float64
	detourMs    float64
	// b, uniform in [min, max), ms, by network type
	lastMileMs map[string][2]float64
}

// Tracks drawn from the drift model (V3): each visitor's connection ends near
// its city point, each extender sits near its listed place, and each path's
// round trip is b + K · d + the route error + the measurement's.
func testLagDriftCalibrationTracks(rng *mathrand.Rand, count int, population *testLagDriftPopulation) []*LagDriftCalibrationTrack {
	normal := func(sigma float64) float64 {
		return rng.NormFloat64() * sigma
	}
	networkTypes := slices.Sorted(func(yield func(string) bool) {
		for networkType := range population.lastMileMs {
			if !yield(networkType) {
				return
			}
		}
	})
	tracks := make([]*LagDriftCalibrationTrack, 0, count)
	for t := range count {
		city := lagdrift.Point{Lat: -40 + 100*rng.Float64(), Lon: -120 + 260*rng.Float64()}
		end := lagdrift.MoveKm(city, normal(population.cityErrorKm), normal(population.cityErrorKm))
		if rng.Float64() < population.wrongCityShare {
			end = testLagDriftDestination(city, 360*rng.Float64(), population.wrongCityKm)
		}
		networkType := networkTypes[t%len(networkTypes)]
		bounds := population.lastMileMs[networkType]
		b := bounds[0] + (bounds[1]-bounds[0])*rng.Float64()
		continentCode := "EU"
		if t%3 == 0 {
			continentCode = "NA"
		}
		track := &LagDriftCalibrationTrack{
			City:          city,
			ContinentCode: continentCode,
			NetworkType:   networkType,
		}
		extenders := 8 + rng.IntN(5)
		for i := range extenders {
			// near, mid and far, as a drift run's extenders are (E2)
			var distanceKm float64
			switch i % 10 {
			case 0, 1, 2:
				distanceKm = 100 + 200*rng.Float64()
			case 3, 4, 5, 6:
				distanceKm = 300 + 2200*rng.Float64()
			default:
				distanceKm = 2500 + 4500*rng.Float64()
			}
			listed := testLagDriftDestination(city, 360*rng.Float64(), distanceKm)
			at := lagdrift.MoveKm(listed, normal(population.placeErrorKm), normal(population.placeErrorKm))
			dKm := lagdrift.DistanceKm(end, at)
			sigmaMeasMs := 0.1 + 0.5*rng.Float64()
			// no round trip beats light: the noise is drawn again when it
			// would, as the model's symmetric route error allows on a long
			// path with a small b
			y := 0.0
			for y < 2*dKm/lagdrift.VacuumKmPerMs {
				y = b + lagdrift.MsPerKm*dKm +
					normal(population.kappa*lagdrift.MsPerKm*dKm) +
					normal(math.Sqrt(sigmaMeasMs*sigmaMeasMs+population.sigma0Ms*population.sigma0Ms))
			}
			if rng.Float64() < population.detourShare {
				y += population.detourMs
			}
			track.Observations = append(track.Observations, lagdrift.Observation{
				ExtenderId: fmt.Sprintf("%d-%d", t, i),
				At:         listed,
				Run:        0,
				RoundTrip: lagdrift.RoundTrip{
					Ms:      y,
					SigmaMs: sigmaMeasMs,
					Count:   60,
					// the fastest ping, which can't beat light either
					MinMs: math.Max(y-0.5, 2*dKm/lagdrift.VacuumKmPerMs),
				},
			})
		}
		tracks = append(tracks, track)
	}
	return tracks
}

func testLagDriftOr(value *float64) float64 {
	if value == nil {
		return math.NaN()
	}
	return *value
}

func testLagDriftClose(t *testing.T, name string, got float64, want float64, tolerance float64) {
	t.Helper()
	if !(math.Abs(got-want) <= tolerance*want) {
		t.Fatalf("%s = %.4f, want %.4f within %.0f%%", name, got, want, 100*tolerance)
	}
}

// The estimate is within 3.5 of its standard errors of the truth, and the
// standard error is under maxShare of it.
func testLagDriftWithinSe(t *testing.T, name string, got float64, se *float64, want float64, maxShare float64) {
	t.Helper()
	if se == nil {
		t.Fatalf("%s has no standard error", name)
	}
	if !(*se < maxShare*want) {
		t.Fatalf("%s's standard error %.4f is over %.0f%% of %.4f", name, *se, 100*maxShare, want)
	}
	if !(math.Abs(got-want) < 3.5**se) {
		t.Fatalf("%s = %.4f is %.1f standard errors from %.4f", name, got, math.Abs(got-want) / *se, want)
	}
}

// Clean synthetic tracks with a known κ and σ₀ give them back, for the
// defaults and for a noisier world, with almost nothing trimmed.
func TestCalibrateLagDriftTracksRecoversKappaAndSigma0(t *testing.T) {
	for _, truth := range []struct {
		kappa    float64
		sigma0Ms float64
	}{
		{0.25, 1.0},
		{0.15, 2.5},
		{0.1, 0.6},
	} {
		t.Run(fmt.Sprintf("kappa %g sigma0 %g", truth.kappa, truth.sigma0Ms), func(t *testing.T) {
			rng := mathrand.New(mathrand.NewPCG(1, uint64(1000*truth.kappa)))
			tracks := testLagDriftCalibrationTracks(rng, 2000, &testLagDriftPopulation{
				kappa:        truth.kappa,
				sigma0Ms:     truth.sigma0Ms,
				cityErrorKm:  30,
				placeErrorKm: 50,
				lastMileMs: map[string][2]float64{
					LagDriftNetworkTypeFixed:  {2, 25},
					LagDriftNetworkTypeMobile: {20, 70},
				},
			})
			result := CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
			t.Logf(
				"kappa %.4f ± %.4f, sigma0 %.4f ± %.4f, %d of %d tracks and %d of %d observations in %d passes",
				result.Kappa, testLagDriftOr(result.KappaSe), result.Sigma0Ms, testLagDriftOr(result.Sigma0MsSe),
				result.FitTracks, result.Tracks, result.FitObservations, result.Observations, result.Passes,
			)
			connect.AssertEqual(t, result.Applied, true)
			connect.AssertEqual(t, result.Converged, true)
			connect.AssertEqual(t, result.AtBound, false)
			connect.AssertEqual(t, result.Tracks, 2000)
			connect.AssertEqual(t, result.ImpossibleTracks, 0)
			// κ to a few percent; σ₀, which sits under the place and
			// measurement errors, to its own standard error
			testLagDriftWithinSe(t, "kappa", result.Kappa, result.KappaSe, truth.kappa, 0.03)
			testLagDriftWithinSe(t, "sigma0", result.Sigma0Ms, result.Sigma0MsSe, truth.sigma0Ms, 0.12)
			testLagDriftClose(t, "kappa", result.Kappa, truth.kappa, 0.05)
			testLagDriftClose(t, "sigma0", result.Sigma0Ms, truth.sigma0Ms, 0.25)
			// a clean population loses at most a few tracks and observations
			if result.Tracks-result.FitTracks > result.Tracks/100 {
				t.Fatalf("trimmed %d of %d clean tracks", result.Tracks-result.FitTracks, result.Tracks)
			}
			// the standardized residuals are N(0, 1): about 68% within ±1
			histogram := result.Continents[""].Residuals
			total := int64(0)
			within := int64(0)
			for bin, count := range histogram.Counts {
				total += count
				// bins 11 and 12 and 13 and 14 are [−1, 1)
				if 11 <= bin && bin <= 14 {
					within += count
				}
			}
			connect.AssertEqual(t, total, int64(result.Observations))
			testLagDriftClose(t, "share within 1σ", float64(within)/float64(total), 0.6827, 0.04)
		})
	}
}

// With wrong city points and detoured paths, the trimming takes them out and
// κ and σ₀ still come back.
func TestCalibrateLagDriftTracksTrimsOutliers(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(2, 2))
	tracks := testLagDriftCalibrationTracks(rng, 1500, &testLagDriftPopulation{
		kappa:          0.25,
		sigma0Ms:       1.0,
		cityErrorKm:    30,
		placeErrorKm:   50,
		wrongCityShare: 0.06,
		wrongCityKm:    900,
		detourShare:    0.02,
		detourMs:       40,
		lastMileMs: map[string][2]float64{
			LagDriftNetworkTypeFixed: {2, 25},
		},
	})
	result := CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
	t.Logf(
		"kappa %.4f, sigma0 %.4f, %d of %d tracks (%d impossible) and %d of %d observations in %d passes",
		result.Kappa, result.Sigma0Ms, result.FitTracks, result.Tracks, result.ImpossibleTracks,
		result.FitObservations, result.Observations, result.Passes,
	)
	connect.AssertEqual(t, result.Applied, true)
	connect.AssertEqual(t, result.Converged, true)
	testLagDriftClose(t, "kappa", result.Kappa, 0.25, 0.08)
	testLagDriftClose(t, "sigma0", result.Sigma0Ms, 1.0, 0.25)
	// most of the wrong cities are out, and with them the detours
	if result.Tracks-result.FitTracks < 1500*6/100*2/3 {
		t.Fatalf("trimmed only %d tracks of %d with 90 wrong cities", result.Tracks-result.FitTracks, result.Tracks)
	}
	if result.Observations-result.FitObservations < 200 {
		t.Fatalf("trimmed only %d observations", result.Observations-result.FitObservations)
	}
}

// A fit of too few tracks is not applied, and keeps the defaults.
func TestCalibrateLagDriftTracksTooFew(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(3, 3))
	tracks := testLagDriftCalibrationTracks(rng, 50, &testLagDriftPopulation{
		kappa:        0.4,
		sigma0Ms:     2,
		cityErrorKm:  30,
		placeErrorKm: 50,
		lastMileMs: map[string][2]float64{
			LagDriftNetworkTypeFixed: {2, 25},
		},
	})
	result := CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
	connect.AssertEqual(t, result.Applied, false)
	connect.AssertEqual(t, result.Reason, "too_few_tracks")
	connect.AssertEqual(t, result.RelayThresholdMs, LagDriftDefaultRelayThresholdMs)
	connect.AssertEqual(t, len(result.RelayThresholdsMs), 0)
	// fewer tracks than a histogram needs: no continent is kept
	tracks = tracks[:10]
	result = CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
	connect.AssertEqual(t, len(result.Continents), 0)
}

// A threshold is the 99th percentile of its group's b where the group has
// enough tracks, never below the floor; a group with too few has none, and
// the threshold over everything is then the default.
func TestLagDriftRelayThresholds(t *testing.T) {
	settings := DefaultLagDriftCalibrationSettings()
	values := func(count int, minMs float64, maxMs float64) []float64 {
		out := []float64{}
		for i := range count {
			out = append(out, minMs+(maxMs-minMs)*float64(i)/float64(count-1))
		}
		return out
	}
	thresholdMs, thresholdsMs := lagDriftRelayThresholds(map[string]map[string][]float64{
		"": {
			"":                        values(1001, 0, 100),
			LagDriftNetworkTypeMobile: values(300, 10, 210),
		},
		"EU": {
			// tight: the floor
			LagDriftNetworkTypeFixed: values(500, 0, 20),
			// too few
			LagDriftNetworkTypeSatellite: values(199, 0, 900),
		},
	}, settings)
	// the 99th percentile of 0..100 in 1001 steps is 99
	testLagDriftClose(t, "threshold", thresholdMs, 99, 1e-9)
	testLagDriftClose(t, "every continent's mobile", thresholdsMs[""][LagDriftNetworkTypeMobile], 10+200*0.99, 1e-9)
	connect.AssertEqual(t, thresholdsMs["EU"][LagDriftNetworkTypeFixed], LagDriftRelayFloorMs)
	_, ok := thresholdsMs["EU"][LagDriftNetworkTypeSatellite]
	connect.AssertEqual(t, ok, false)

	thresholdMs, thresholdsMs = lagDriftRelayThresholds(map[string]map[string][]float64{
		"": {"": values(150, 0, 500)},
	}, settings)
	connect.AssertEqual(t, thresholdMs, LagDriftDefaultRelayThresholdMs)
	connect.AssertEqual(t, len(thresholdsMs), 0)
}

// The calibration's thresholds are the 99th percentile of the drift fit's b by
// continent and network type, which is the b the tracks were drawn with.
func TestCalibrateLagDriftTracksRelayThresholds(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(4, 4))
	tracks := testLagDriftCalibrationTracks(rng, 1800, &testLagDriftPopulation{
		kappa:        0.25,
		sigma0Ms:     1.0,
		cityErrorKm:  30,
		placeErrorKm: 50,
		lastMileMs: map[string][2]float64{
			LagDriftNetworkTypeFixed:  {2, 22},
			LagDriftNetworkTypeMobile: {20, 120},
		},
	})
	result := CalibrateLagDriftTracks(tracks, DefaultLagDriftCalibrationSettings())
	connect.AssertEqual(t, result.Applied, true)
	connect.AssertEqual(t, result.UnfittedTracks, 0)
	// fixed lines' b is under 22 ms, so their threshold is the floor
	connect.AssertEqual(t, result.RelayThresholdsMs[""][LagDriftNetworkTypeFixed], LagDriftRelayFloorMs)
	connect.AssertEqual(t, result.RelayThresholdsMs["EU"][LagDriftNetworkTypeFixed], LagDriftRelayFloorMs)
	// mobile b is uniform in [20, 120): its 99th percentile is 119
	if math.Abs(result.RelayThresholdsMs[""][LagDriftNetworkTypeMobile]-119) > 3 {
		t.Fatalf("mobile threshold %.2f, want about 119", result.RelayThresholdsMs[""][LagDriftNetworkTypeMobile])
	}
	if math.Abs(result.RelayThresholdsMs["EU"][LagDriftNetworkTypeMobile]-119) > 4 {
		t.Fatalf("EU mobile threshold %.2f, want about 119", result.RelayThresholdsMs["EU"][LagDriftNetworkTypeMobile])
	}
	// NA has 600 tracks, 300 of each type: enough for its own
	_, ok := result.RelayThresholdsMs["NA"][LagDriftNetworkTypeMobile]
	connect.AssertEqual(t, ok, true)
	// over everything: half fixed, half mobile, so the 99th percentile is in
	// mobile's tail
	if math.Abs(result.RelayThresholdMs-118) > 3 {
		t.Fatalf("threshold %.2f, want about 118", result.RelayThresholdMs)
	}
	connect.AssertEqual(t, result.RelayThresholdsMs[""][""], result.RelayThresholdMs)

	// b's histograms: every track in one, by continent and type, and the sum
	// over continents is theirs
	for _, continentCode := range []string{"EU", "NA"} {
		continent := result.Continents[continentCode]
		tracks := 0
		for _, histogram := range continent.LastMileMs {
			tracks += histogram.Tracks
			sum := int64(0)
			for _, count := range histogram.Counts {
				sum += count
			}
			connect.AssertEqual(t, sum, int64(histogram.Tracks))
		}
		connect.AssertEqual(t, tracks, continent.Tracks)
	}
	connect.AssertEqual(t, result.Continents[""].Tracks, 1800)
	connect.AssertEqual(t, result.Continents[""].LastMileMs[LagDriftNetworkTypeMobile].Tracks, 900)
}
