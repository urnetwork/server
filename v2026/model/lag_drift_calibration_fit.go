package model

import (
	"math"
	"slices"
	"sort"

	"github.com/urnetwork/connect/v2026/lagdrift"
)

// The calibration's fit (lag.dev/DRIFTTEST.md V3, V4, K2), pure: no database.
//
// A track gives a visitor's city point c and, for each extender i at a known
// place eᵢ, the 10th percentile yᵢ of its round trips and that estimate's
// standard error σ_meas,i (V1). With rᵢ = yᵢ − K · d(c, eᵢ), the model is
//
//	rᵢ = b − K · uᵢ · δ + ρᵢ + εᵢ
//
// where b is the track's last-mile offset; δ is how far the visitor's
// connection really ends from its city point, 2D with σ = s_c on each axis,
// shared by every path of the track (uᵢ is the unit vector from the city
// toward the extender, so the path's distance changes by −uᵢ · δ); ρᵢ is the
// path's route and place error, N(0, (κ · K · dᵢ)² + (K · s_ext,i)²); and εᵢ
// is N(0, σ_meas,i² + σ₀²) (V3). So r ~ N(b · 1, V) with
//
//	V = diag(σ_meas,i² + σ₀² + (κ K dᵢ)² + (K s_ext,i)²) + K² s_c² U Uᵀ
//
// The city's error enters like the extender's place error, as a variance on
// each path, but as one draw for the whole track, since every path starts at
// the same wrong point. An extender within s_c of the city has no useful
// direction, so its share of the shared term fades with dᵢ / s_c and the rest
// goes on its own diagonal.
//
// b is integrated out (restricted maximum likelihood), which counts the degree
// of freedom b takes, as the drift fit's χ²/dof does. Per track:
//
//	ℓ = −½ (log |V| + log 1ᵀV⁻¹1 + rᵀV⁻¹r − (1ᵀV⁻¹r)² / 1ᵀV⁻¹1)
//
// summed over tracks and maximized over log κ and log σ₀ by Nelder–Mead.
//
// Real tracks hold outliers the model has no term for: a visitor whose city
// point is wrong, as on a VPN outside a hosting network, and a path that took
// a long detour. They are trimmed by fixed points of the fit: from a fit of
// everything, each track drops its one observation whose standardized
// residual is beyond 4, and drops out whole when a second is, when the
// displacement its paths fit from its city point is beyond the 99.9% point of
// χ² with 2 degrees of freedom, when its χ² is beyond the 99.9% point for its
// degrees of freedom, or when its b is negative beyond 3 standard errors; and
// the fit is repeated until the trimmed set stops changing. A track whose city
// point is too far from an extender for its fastest ping even at the speed of
// light, allowing the extender's place error and 3 s_c, has a wrong city point
// and never enters (V5's check).
//
// What is kept is aggregate (V4, R1): κ and σ₀ with their standard errors; by
// continent, histograms of the standardized residuals and of b by network
// type; and the relay thresholds, the 99th percentile of b by continent and
// network type with a floor. b is the drift fit's own (lagdrift.FitPosition
// under the calibrated model, from the city point), so a threshold compares
// like with like when the processor flags relay_suspected (K2, Q12). A group
// with too few tracks keeps no histogram or threshold, so no aggregate is of
// one visitor.

// The calibration's settings. The defaults are the design's.
type LagDriftCalibrationSettings struct {
	// how far a visitor's connection ends from its city point, as a 1σ error
	// on each axis, km: city points are off by tens of km (V4, A5)
	CityErrorKm float64
	// an extender's round trip is usable from this many pings, as a drift
	// run's report is (E5)
	MinRoundTrips int
	// a track needs this many usable extenders
	MinTrackExtenders int
	// a calibration needs this many tracks in its fit to be applied
	MinTracks int
	// the share of b that is normal: the relay threshold is its quantile
	RelayQuantile float64
	// no relay threshold is below this, ms
	RelayFloorMs float64
	// a relay threshold needs this many tracks' b
	RelayMinTracks int
	// a histogram needs this many tracks
	HistogramMinTracks int
	// a track's fit counts toward the region's size (V11) with this many
	// extenders, as a drift run counts with this many usable ones (E5)
	KMinExtenders int
}

// The design's settings (lag.dev/DRIFTTEST.md V4, K2, E5).
func DefaultLagDriftCalibrationSettings() *LagDriftCalibrationSettings {
	return &LagDriftCalibrationSettings{
		CityErrorKm:        30,
		MinRoundTrips:      12,
		MinTrackExtenders:  4,
		MinTracks:          100,
		RelayQuantile:      0.99,
		RelayFloorMs:       LagDriftRelayFloorMs,
		RelayMinTracks:     200,
		HistogramMinTracks: 20,
		KMinExtenders:      6,
	}
}

// One track as the calibration fits it: the visitor's city point and its
// round trip to each extender at a known place, with the continent and
// network type its aggregates are kept by. Only ever in memory (R1).
type LagDriftCalibrationTrack struct {
	// the city's point (lag_track's client_latitude and client_longitude)
	City lagdrift.Point
	// ContinentCodeForCountry of the track's country, "" when unknown
	ContinentCode string
	// LagDriftNetworkType of the track's network
	NetworkType string
	// one per extender, all of run 0
	Observations []lagdrift.Observation
}

// The edges of the standardized residuals' histogram: −6 to 6 by 0.5. A
// histogram has a bin below the first edge and one from the last up.
var LagDriftResidualEdges = func() []float64 {
	edges := []float64{}
	for i := -12; i <= 12; i += 1 {
		edges = append(edges, float64(i)/2)
	}
	return edges
}()

// The edges of the region size's histogram (V11): the Mahalanobis distance of
// a track's city point from its fit, 1.5 to 12 by 0.05. Below 1.5 lies about
// two thirds of an honest population, which no confidence V11 allows reaches
// down to; from 12 up is one bin.
var LagDriftKEdges = func() []float64 {
	edges := []float64{}
	for i := 30; i <= 240; i += 1 {
		edges = append(edges, float64(i)/20)
	}
	return edges
}()

// The edges of b's histogram, ms.
var LagDriftLastMileEdgesMs = []float64{
	0, 1, 2, 3, 4, 5, 6, 8, 10, 12, 15, 20, 25, 30, 40, 50, 60, 80, 100,
	120, 150, 200, 250, 300, 400, 500, 600, 800, 1000,
}

// A histogram over a set of edges: Counts[0] is below the first edge,
// Counts[k] is in [edge k−1, edge k), and the last is from the last edge up.
type LagDriftHistogram struct {
	// the tracks the values come from
	Tracks int     `json:"tracks"`
	Counts []int64 `json:"counts"`
}

func newLagDriftHistogram(edges []float64) *LagDriftHistogram {
	return &LagDriftHistogram{
		Counts: make([]int64, len(edges)+1),
	}
}

func (self *LagDriftHistogram) add(edges []float64, value float64) {
	if math.IsNaN(value) {
		return
	}
	bin := sort.Search(len(edges), func(i int) bool {
		return value < edges[i]
	})
	self.Counts[bin] += 1
}

func (self *LagDriftHistogram) addHistogram(other *LagDriftHistogram) {
	self.Tracks += other.Tracks
	for i, count := range other.Counts {
		self.Counts[i] += count
	}
}

// One continent's aggregates.
type LagDriftCalibrationContinent struct {
	// the continent's tracks and observations, and those the fit kept
	Tracks          int `json:"tracks"`
	Observations    int `json:"observations"`
	FitTracks       int `json:"fit_tracks"`
	FitObservations int `json:"fit_observations"`
	// every observation's standardized residual under the calibrated model,
	// trimmed ones included, so the tails show
	Residuals *LagDriftHistogram `json:"residuals"`
	// b by network type, ms
	LastMileMs map[string]*LagDriftHistogram `json:"last_mile_ms"`
}

// What a calibration found: the model, how it was fitted, and the
// aggregates. Nothing in it is of one visitor (R1).
type LagDriftCalibrationResult struct {
	Kappa    float64 `json:"kappa"`
	Sigma0Ms float64 `json:"sigma0_ms"`
	// the standard errors from the likelihood's curvature, nil when it has
	// none. They count every track as independent, though tracks share
	// extenders, so they are a floor
	KappaSe    *float64 `json:"kappa_se"`
	Sigma0MsSe *float64 `json:"sigma0_ms_se"`
	// the relay threshold over every continent and network type, ms
	RelayThresholdMs float64 `json:"relay_threshold_ms"`
	// by continent code, "" for every continent, then by network type, ""
	// for every type, ms: where there were RelayMinTracks tracks
	RelayThresholdsMs map[string]map[string]float64 `json:"relay_thresholds_ms"`

	// whether the fit settled, had enough tracks and is inside its bounds:
	// only an applied calibration is stored
	Applied bool `json:"applied"`
	// why not, when not applied
	Reason string `json:"reason,omitempty"`
	// the trimming passes, and whether the trimmed set settled
	Passes    int  `json:"passes"`
	Converged bool `json:"converged"`
	// whether κ or σ₀ is at a bound of its search
	AtBound bool `json:"at_bound"`

	// every track and observation given, and those in the final fit
	Tracks          int `json:"tracks"`
	Observations    int `json:"observations"`
	FitTracks       int `json:"fit_tracks"`
	FitObservations int `json:"fit_observations"`
	// tracks whose city point the speed of light rules out
	ImpossibleTracks int `json:"impossible_tracks"`
	// tracks the drift fit could not place, so they have no b
	UnfittedTracks int `json:"unfitted_tracks"`

	// the settings it ran with
	CityErrorKm   float64 `json:"city_error_km"`
	PlaceErrorKm  float64 `json:"place_error_km"`
	RelayQuantile float64 `json:"relay_quantile"`
	RelayFloorMs  float64 `json:"relay_floor_ms"`

	// the histograms' edges, and the histograms by continent code (those with
	// HistogramMinTracks tracks), with "" for their sum
	ResidualEdges   []float64                                `json:"residual_edges"`
	LastMileEdgesMs []float64                                `json:"last_mile_edges_ms"`
	Continents      map[string]*LagDriftCalibrationContinent `json:"continents"`

	// the region size's histograms (V11), by continent code where it had
	// HistogramMinTracks tracks: each track's Mahalanobis distance of its city
	// point from its single-run fit, under the fit's covariance alone, over
	// KEdges. The 26 weeks before a calibration sum to its k (GetLagDriftCalibration).
	// Beside each, the tracks left out because the speed of light rules out
	// their city point from two or more extenders: a place the test exists to
	// reject, such as a VPN's virtual location, rather than an honest fit's tail
	KEdges            []float64                     `json:"k_edges"`
	KHistograms       map[string]*LagDriftHistogram `json:"k_histograms"`
	KImpossibleTracks map[string]int                `json:"k_impossible_tracks"`
}

// The model the calibration found, for lagdrift's fits.
func (self *LagDriftCalibrationResult) Model() lagdrift.Model {
	return lagdrift.Model{
		Kappa:    self.Kappa,
		Sigma0Ms: self.Sigma0Ms,
	}
}

// The search's bounds. A fit at an upper bound is not applied: the data are
// then mostly what the model has no term for. One at a lower bound is: that
// term is negligible beside the others.
const (
	lagDriftMinKappa    = 0.02
	lagDriftMaxKappa    = 2.0
	lagDriftMinSigma0Ms = 0.1
	lagDriftMaxSigma0Ms = 20.0
)

// The trimming's thresholds.
const (
	// an observation's standardized residual beyond this is trimmed
	lagDriftTrimZ = 4.0
	// the normal quantile of a track's χ² beyond which it is trimmed: 99.9%
	lagDriftTrimChi2Z = 3.090232
	// the χ² of a track's displacement from its city point beyond which it is
	// trimmed: 99.9% with 2 degrees of freedom, −2 ln 0.001
	lagDriftTrimDisplacementChi2 = 13.815511
	// a track's b more than this many standard errors below 0 is trimmed
	lagDriftTrimNegativeLastMileSe = 3.0
	// the trimming stops after this many passes, settled or not
	lagDriftMaxPasses = 20
)

// One path of a track, as the likelihood needs it.
type lagDriftCalibrationPath struct {
	// rᵢ = yᵢ − K · d(c, eᵢ), ms
	residualMs float64
	// σ_meas², ms²
	measVar float64
	// K · dᵢ, ms: the route error is κ times this
	pathMs float64
	// (K · s_ext,i)², ms²
	placeVar float64
	// the city error's shared part along this path, ms: K · s_c · gᵢ · uᵢ
	cityEast  float64
	cityNorth float64
	// and the rest of it, on the diagonal, ms²: K² s_c² (1 − gᵢ²)
	cityVar float64
}

// A track's paths, and which of them the fit keeps.
type lagDriftCalibrationTrackPaths struct {
	track *LagDriftCalibrationTrack
	// s_c, km
	cityErrorKm float64
	paths       []lagDriftCalibrationPath
	// whether its city point is possible at all (V5): within reach of every
	// extender
	possible bool
	// the extenders whose reach the city point is beyond
	outOfReach int
	// the paths the current pass keeps; nil when the track is trimmed
	kept []int
}

// The paths of a track under the model's geometry, and whether its city point
// is physically possible: within reach of every extender at the speed of
// light, from its fastest ping, allowing the extender's place error and
// 3 s_c of the city's (V5).
func newLagDriftCalibrationTrackPaths(track *LagDriftCalibrationTrack, cityErrorKm float64) *lagDriftCalibrationTrackPaths {
	self := &lagDriftCalibrationTrackPaths{
		track:       track,
		cityErrorKm: cityErrorKm,
		paths:       make([]lagDriftCalibrationPath, 0, len(track.Observations)),
	}
	k := lagdrift.MsPerKm
	for _, observation := range track.Observations {
		dKm := lagdrift.DistanceKm(track.City, observation.At)
		placeKm := lagdrift.HeldPlaceErrorKm(observation.PlaceErrorKm)
		fastestMs := observation.RoundTrip.MinMs
		if !(0 < fastestMs) {
			fastestMs = observation.RoundTrip.Ms
		}
		if fastestMs/2*lagdrift.VacuumKmPerMs+placeKm+3*cityErrorKm < dKm {
			self.outOfReach += 1
		}
		// the shared part fades for an extender within s_c of the city,
		// where the direction to it says little
		g := math.Min(1, dKm/cityErrorKm)
		bearing := lagdrift.BearingDeg(track.City, observation.At) * math.Pi / 180
		shared := k * cityErrorKm * g
		self.paths = append(self.paths, lagDriftCalibrationPath{
			residualMs: observation.RoundTrip.Ms - k*dKm,
			measVar:    observation.RoundTrip.SigmaMs * observation.RoundTrip.SigmaMs,
			pathMs:     k * dKm,
			placeVar:   (k * placeKm) * (k * placeKm),
			cityEast:   shared * math.Sin(bearing),
			cityNorth:  shared * math.Cos(bearing),
			cityVar:    k * k * cityErrorKm * cityErrorKm * (1 - g*g),
		})
	}
	self.possible = self.outOfReach == 0
	return self
}

// The Mahalanobis distance of p from a fit under the fit's position
// covariance alone, as the region test measures it (V6): p in the azimuthal
// equidistant projection centered on the fit, whitened by the covariance's
// Cholesky factor with lagdrift.Judge's variance floors. False when the
// covariance gives no number.
func lagDriftMahalanobis(fit *lagdrift.Fit, p lagdrift.Point) (float64, bool) {
	const minVarianceKm2 = 1e-9
	s00 := math.Max(minVarianceKm2, fit.Cov[0][0])
	s01 := (fit.Cov[0][1] + fit.Cov[1][0]) / 2
	l11 := math.Sqrt(s00)
	l21 := s01 / l11
	l22 := math.Sqrt(math.Max(minVarianceKm2, fit.Cov[1][1]-l21*l21))
	eastKm, northKm := lagdrift.AzimuthalKm(fit.At, p)
	x := eastKm / l11
	y := (northKm - l21*x) / l22
	d := math.Hypot(x, y)
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return 0, false
	}
	return d, true
}

// Reused buffers for one track's evaluation.
type lagDriftCalibrationScratch struct {
	v        []float64
	diagonal []float64
	a        []float64
	c        []float64
}

func (self *lagDriftCalibrationScratch) size(n int) {
	if cap(self.v) < n*n {
		self.v = make([]float64, n*n)
		self.diagonal = make([]float64, n)
		self.a = make([]float64, n)
		self.c = make([]float64, n)
	}
	self.v = self.v[:n*n]
	self.diagonal = self.diagonal[:n]
	self.a = self.a[:n]
	self.c = self.c[:n]
}

// Solves L Lᵀ out = out in place, L in the lower triangle of the n × n v.
func lagDriftCholeskySolve(v []float64, n int, out []float64) {
	for i := range n {
		sum := out[i]
		for k := range i {
			sum -= v[i*n+k] * out[k]
		}
		out[i] = sum / v[i*n+i]
	}
	for i := n - 1; 0 <= i; i -= 1 {
		sum := out[i]
		for k := i + 1; k < n; k += 1 {
			sum -= v[k*n+i] * out[k]
		}
		out[i] = sum / v[i*n+i]
	}
}

// One track's terms under a model, over the paths given.
type lagDriftCalibrationEvaluation struct {
	// the track's restricted log-likelihood, up to a constant
	logLikelihood float64
	// b's estimate and its standard error, ms
	lastMileMs   float64
	lastMileSeMs float64
	// rᵀPr, χ² with n − 1 degrees of freedom under the model
	chi2 float64
	// each path's standardized residual, when asked for
	z []float64
}

// Evaluates a track's paths under κ and σ₀ by the Cholesky factor of V.
// False when V is not positive definite, which only a degenerate input makes.
func (self *lagDriftCalibrationTrackPaths) evaluate(
	kappa float64,
	sigma0Ms float64,
	indexes []int,
	scratch *lagDriftCalibrationScratch,
	withZ bool,
) (lagDriftCalibrationEvaluation, bool) {
	n := len(indexes)
	scratch.size(n)
	v := scratch.v
	sigma0Var := sigma0Ms * sigma0Ms
	for row, i := range indexes {
		p := &self.paths[i]
		for col := 0; col <= row; col += 1 {
			q := &self.paths[indexes[col]]
			value := p.cityEast*q.cityEast + p.cityNorth*q.cityNorth
			if col == row {
				route := kappa * p.pathMs
				value += p.measVar + sigma0Var + route*route + p.placeVar + p.cityVar
			}
			v[row*n+col] = value
		}
		scratch.diagonal[row] = v[row*n+row]
	}
	// V = L Lᵀ, L in the lower triangle
	logDet := 0.0
	for j := range n {
		sum := v[j*n+j]
		for k := range j {
			sum -= v[j*n+k] * v[j*n+k]
		}
		if !(0 < sum) {
			return lagDriftCalibrationEvaluation{}, false
		}
		ljj := math.Sqrt(sum)
		v[j*n+j] = ljj
		logDet += 2 * math.Log(ljj)
		for i := j + 1; i < n; i += 1 {
			sum := v[i*n+j]
			for k := range j {
				sum -= v[i*n+k] * v[j*n+k]
			}
			v[i*n+j] = sum / ljj
		}
	}
	// a = V⁻¹1 and c = V⁻¹r, by forward and back substitution
	for row, i := range indexes {
		scratch.a[row] = 1
		scratch.c[row] = self.paths[i].residualMs
	}
	lagDriftCholeskySolve(v, n, scratch.a)
	lagDriftCholeskySolve(v, n, scratch.c)
	s11 := 0.0
	s1r := 0.0
	srr := 0.0
	for i := range n {
		s11 += scratch.a[i]
		s1r += scratch.c[i]
		srr += self.paths[indexes[i]].residualMs * scratch.c[i]
	}
	if !(0 < s11) {
		return lagDriftCalibrationEvaluation{}, false
	}
	evaluation := lagDriftCalibrationEvaluation{
		lastMileMs:   s1r / s11,
		lastMileSeMs: 1 / math.Sqrt(s11),
		chi2:         srr - s1r*s1r/s11,
	}
	evaluation.logLikelihood = -0.5 * (logDet + math.Log(s11) + evaluation.chi2)
	if withZ {
		// the residual's own variance, b's estimate taken out:
		// Var(r − 1b̂) = V − 1 1ᵀ / 1ᵀV⁻¹1
		evaluation.z = make([]float64, n)
		for row, i := range indexes {
			variance := scratch.diagonal[row] - 1/s11
			evaluation.z[row] = (self.paths[i].residualMs - evaluation.lastMileMs) / math.Sqrt(math.Max(variance, 1e-12))
		}
	}
	return evaluation, true
}

// How far the paths say the visitor's connection ends from the city point, as
// χ² with 2 degrees of freedom: the displacement δ̂ fitted by generalized
// least squares beside b, with each path's own variance and none shared,
// against its covariance plus the city's own error, s_c² on each axis. A city
// point that is right puts δ̂ within that; a wrong one, as for a visitor on a
// VPN outside the hosting list, moves every path at once, which this sees
// with 2 degrees of freedom where the track's χ² spreads it over n − 1. 0 when
// the paths' directions can't place δ̂.
func (self *lagDriftCalibrationTrackPaths) displacementChi2(kappa float64, sigma0Ms float64, indexes []int) float64 {
	// XᵀD⁻¹X and XᵀD⁻¹r for X = [1, −K g u_east, −K g u_north], in km
	var a [3][3]float64
	var g [3]float64
	sigma0Var := sigma0Ms * sigma0Ms
	for _, i := range indexes {
		p := &self.paths[i]
		route := kappa * p.pathMs
		w := 1 / (p.measVar + sigma0Var + route*route + p.placeVar + p.cityVar)
		x := [3]float64{1, -p.cityEast / self.cityErrorKm, -p.cityNorth / self.cityErrorKm}
		for row := range 3 {
			g[row] += w * x[row] * p.residualMs
			for col := range 3 {
				a[row][col] += w * x[row] * x[col]
			}
		}
	}
	inverse, ok := lagDriftInvert3(a)
	if !ok {
		return 0
	}
	var beta [3]float64
	for row := range 3 {
		for col := range 3 {
			beta[row] += inverse[row][col] * g[col]
		}
	}
	// δ̂'s covariance, with the city's own error added
	c := [2][2]float64{
		{inverse[1][1] + self.cityErrorKm*self.cityErrorKm, inverse[1][2]},
		{inverse[2][1], inverse[2][2] + self.cityErrorKm*self.cityErrorKm},
	}
	det := c[0][0]*c[1][1] - c[0][1]*c[1][0]
	if !(0 < det) {
		return 0
	}
	east := beta[1]
	north := beta[2]
	return (c[1][1]*east*east - (c[0][1]+c[1][0])*east*north + c[0][0]*north*north) / det
}

// The inverse of a symmetric 3 × 3 matrix by cofactors; false when it is
// singular for its scale.
func lagDriftInvert3(m [3][3]float64) ([3][3]float64, bool) {
	var inverse [3][3]float64
	c00 := m[1][1]*m[2][2] - m[1][2]*m[2][1]
	c01 := m[1][2]*m[2][0] - m[1][0]*m[2][2]
	c02 := m[1][0]*m[2][1] - m[1][1]*m[2][0]
	det := m[0][0]*c00 + m[0][1]*c01 + m[0][2]*c02
	scale := math.Abs(m[0][0]*m[1][1]*m[2][2]) + 1e-300
	if !(1e-12*scale < math.Abs(det)) {
		return inverse, false
	}
	inverse[0][0] = c00 / det
	inverse[1][0] = c01 / det
	inverse[2][0] = c02 / det
	inverse[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) / det
	inverse[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) / det
	inverse[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) / det
	inverse[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) / det
	inverse[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) / det
	inverse[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) / det
	return inverse, true
}

// The upper (1 − α) point of χ² with dof degrees of freedom, for the normal
// quantile z of 1 − α (Wilson–Hilferty: within 2% from 1 degree of freedom).
func lagDriftChi2Quantile(dof int, z float64) float64 {
	nu := float64(dof)
	c := 2 / (9 * nu)
	t := 1 - c + z*math.Sqrt(c)
	return nu * t * t * t
}

// One pass's trimming of a track under κ and σ₀. Its worst observation goes
// when its standardized residual is beyond lagDriftTrimZ: one detoured path.
// The whole track goes when a second is beyond it too, when its χ² or its
// negative b is beyond belief, or when too few observations are left. A wrong
// city point moves every path, the nearest the most, so trimming path after
// path would leave its farthest, noisiest paths looking well. Sets kept, nil
// when the track is trimmed.
func (self *lagDriftCalibrationTrackPaths) trim(
	kappa float64,
	sigma0Ms float64,
	minObservations int,
	scratch *lagDriftCalibrationScratch,
) {
	self.kept = nil
	if !self.possible {
		return
	}
	kept := make([]int, len(self.paths))
	for i := range kept {
		kept[i] = i
	}
	for trimmed := 0; minObservations <= len(kept); {
		evaluation, ok := self.evaluate(kappa, sigma0Ms, kept, scratch, true)
		if !ok {
			return
		}
		worst := -1
		for row, z := range evaluation.z {
			if lagDriftTrimZ < math.Abs(z) && (worst < 0 || math.Abs(evaluation.z[worst]) < math.Abs(z)) {
				worst = row
			}
		}
		if 0 <= worst {
			if 0 < trimmed {
				return
			}
			kept = slices.Delete(kept, worst, worst+1)
			trimmed += 1
			continue
		}
		if lagDriftTrimDisplacementChi2 < self.displacementChi2(kappa, sigma0Ms, kept) {
			return
		}
		if lagDriftChi2Quantile(len(kept)-1, lagDriftTrimChi2Z) < evaluation.chi2 {
			return
		}
		if evaluation.lastMileMs+lagDriftTrimNegativeLastMileSe*evaluation.lastMileSeMs < 0 {
			return
		}
		self.kept = kept
		return
	}
}

// The restricted log-likelihood of every kept track under κ and σ₀.
func lagDriftCalibrationLogLikelihood(
	tracks []*lagDriftCalibrationTrackPaths,
	kappa float64,
	sigma0Ms float64,
	scratch *lagDriftCalibrationScratch,
) float64 {
	sum := 0.0
	for _, track := range tracks {
		if track.kept == nil {
			continue
		}
		evaluation, ok := track.evaluate(kappa, sigma0Ms, track.kept, scratch, false)
		if !ok {
			return math.Inf(-1)
		}
		sum += evaluation.logLikelihood
	}
	return sum
}

// Minimizes f over the box [lo, hi] by Nelder–Mead from start, each vertex
// held to the box. Answers the best point, its value, and whether the simplex
// settled within maxIterations.
func lagDriftNelderMead(
	f func([2]float64) float64,
	start [2]float64,
	step float64,
	lo [2]float64,
	hi [2]float64,
	maxIterations int,
) (best [2]float64, fBest float64, settled bool) {
	clamp := func(x [2]float64) [2]float64 {
		for i := range x {
			x[i] = math.Max(lo[i], math.Min(hi[i], x[i]))
		}
		return x
	}
	type vertex struct {
		x [2]float64
		f float64
	}
	at := func(x [2]float64) vertex {
		x = clamp(x)
		return vertex{x: x, f: f(x)}
	}
	start = clamp(start)
	simplex := []vertex{
		at(start),
		at([2]float64{start[0] + step, start[1]}),
		at([2]float64{start[0], start[1] + step}),
	}
	// a vertex pushed onto a bound can coincide with the start
	for i := 1; i < 3; i += 1 {
		if simplex[i].x == simplex[0].x {
			x := simplex[0].x
			x[i-1] -= step
			simplex[i] = at(x)
		}
	}
	combine := func(a [2]float64, b [2]float64, t float64) [2]float64 {
		return [2]float64{a[0] + t*(b[0]-a[0]), a[1] + t*(b[1]-a[1])}
	}
	for range maxIterations {
		slices.SortStableFunc(simplex, func(a vertex, b vertex) int {
			switch {
			case a.f < b.f:
				return -1
			case b.f < a.f:
				return 1
			default:
				return 0
			}
		})
		size := 0.0
		for _, v := range simplex[1:] {
			size = math.Max(size, math.Max(math.Abs(v.x[0]-simplex[0].x[0]), math.Abs(v.x[1]-simplex[0].x[1])))
		}
		if size < 1e-6 && simplex[2].f-simplex[0].f <= 1e-9*(1+math.Abs(simplex[0].f)) {
			return simplex[0].x, simplex[0].f, true
		}
		centroid := combine(simplex[0].x, simplex[1].x, 0.5)
		reflected := at(combine(centroid, simplex[2].x, -1))
		switch {
		case reflected.f < simplex[0].f:
			expanded := at(combine(centroid, simplex[2].x, -2))
			if expanded.f < reflected.f {
				simplex[2] = expanded
			} else {
				simplex[2] = reflected
			}
		case reflected.f < simplex[1].f:
			simplex[2] = reflected
		default:
			var contracted vertex
			if reflected.f < simplex[2].f {
				contracted = at(combine(centroid, reflected.x, 0.5))
			} else {
				contracted = at(combine(centroid, simplex[2].x, 0.5))
			}
			if contracted.f < math.Min(reflected.f, simplex[2].f) {
				simplex[2] = contracted
			} else {
				// shrink toward the best
				for i := 1; i < 3; i += 1 {
					simplex[i] = at(combine(simplex[0].x, simplex[i].x, 0.5))
				}
			}
		}
	}
	slices.SortStableFunc(simplex, func(a vertex, b vertex) int {
		switch {
		case a.f < b.f:
			return -1
		case b.f < a.f:
			return 1
		default:
			return 0
		}
	})
	return simplex[0].x, simplex[0].f, false
}

// Standard errors of κ and σ₀ from the curvature of f, the negative
// log-likelihood over log κ and log σ₀, at its minimum x. Nil when the
// curvature there is not positive definite.
func lagDriftCalibrationStandardErrors(f func([2]float64) float64, x [2]float64) (kappaSe *float64, sigma0Se *float64) {
	const h = 1e-3
	at := func(dx float64, dy float64) float64 {
		return f([2]float64{x[0] + dx, x[1] + dy})
	}
	f0 := at(0, 0)
	hxx := (at(h, 0) - 2*f0 + at(-h, 0)) / (h * h)
	hyy := (at(0, h) - 2*f0 + at(0, -h)) / (h * h)
	hxy := (at(h, h) - at(h, -h) - at(-h, h) + at(-h, -h)) / (4 * h * h)
	det := hxx*hyy - hxy*hxy
	finite := func(values ...float64) bool {
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return false
			}
		}
		return true
	}
	if !finite(hxx, hyy, hxy, det) || !(0 < hxx && 0 < det) {
		return nil, nil
	}
	// the inverse's diagonal, then from log κ and log σ₀ to κ and σ₀
	varX := hyy / det
	varY := hxx / det
	kappa := math.Exp(x[0]) * math.Sqrt(varX)
	sigma0 := math.Exp(x[1]) * math.Sqrt(varY)
	return &kappa, &sigma0
}

// The q quantile of sorted values, interpolated between the closest ranks.
func lagDriftQuantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	h := float64(len(sorted)-1) * q
	lo := int(math.Floor(h))
	if len(sorted)-1 <= lo {
		return sorted[len(sorted)-1]
	}
	return sorted[lo] + (h-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// The relay thresholds from each group's values of b, by continent code and
// network type, "" for every one: the quantile of b where a group has enough
// tracks, never below the floor (K2, Q12). The threshold over everything is
// the default without enough tracks.
func lagDriftRelayThresholds(
	lastMileMs map[string]map[string][]float64,
	settings *LagDriftCalibrationSettings,
) (thresholdMs float64, thresholdsMs map[string]map[string]float64) {
	thresholdsMs = map[string]map[string]float64{}
	for continentCode, types := range lastMileMs {
		for networkType, values := range types {
			if len(values) < settings.RelayMinTracks {
				continue
			}
			sorted := slices.Clone(values)
			slices.Sort(sorted)
			if thresholdsMs[continentCode] == nil {
				thresholdsMs[continentCode] = map[string]float64{}
			}
			thresholdsMs[continentCode][networkType] = math.Max(settings.RelayFloorMs, lagDriftQuantile(sorted, settings.RelayQuantile))
		}
	}
	thresholdMs, ok := thresholdsMs[""][""]
	if !ok {
		thresholdMs = LagDriftDefaultRelayThresholdMs
	}
	return
}

// CalibrateLagDriftTracks fits κ and σ₀ to the tracks by restricted maximum
// likelihood, trimming what the model has no term for, and keeps the
// aggregates (lag.dev/DRIFTTEST.md V4, K2). The result is applied only when
// the trimming settled with at least MinTracks tracks in the fit and neither
// constant at an upper bound.
func CalibrateLagDriftTracks(tracks []*LagDriftCalibrationTrack, settings *LagDriftCalibrationSettings) *LagDriftCalibrationResult {
	result := &LagDriftCalibrationResult{
		Kappa:             LagDriftDefaultKappa,
		Sigma0Ms:          LagDriftDefaultSigma0Ms,
		RelayThresholdMs:  LagDriftDefaultRelayThresholdMs,
		RelayThresholdsMs: map[string]map[string]float64{},
		CityErrorKm:       settings.CityErrorKm,
		PlaceErrorKm:      lagdrift.HeldPlaceErrorKm(0),
		RelayQuantile:     settings.RelayQuantile,
		RelayFloorMs:      settings.RelayFloorMs,
		ResidualEdges:     LagDriftResidualEdges,
		LastMileEdgesMs:   LagDriftLastMileEdgesMs,
		Continents:        map[string]*LagDriftCalibrationContinent{},
		KEdges:            LagDriftKEdges,
		KHistograms:       map[string]*LagDriftHistogram{},
		KImpossibleTracks: map[string]int{},
	}

	all := make([]*lagDriftCalibrationTrackPaths, 0, len(tracks))
	for _, track := range tracks {
		if len(track.Observations) < settings.MinTrackExtenders {
			continue
		}
		paths := newLagDriftCalibrationTrackPaths(track, settings.CityErrorKm)
		all = append(all, paths)
		result.Tracks += 1
		result.Observations += len(paths.paths)
		if !paths.possible {
			result.ImpossibleTracks += 1
		}
	}

	scratch := &lagDriftCalibrationScratch{}
	lo := [2]float64{math.Log(lagDriftMinKappa), math.Log(lagDriftMinSigma0Ms)}
	hi := [2]float64{math.Log(lagDriftMaxKappa), math.Log(lagDriftMaxSigma0Ms)}
	negativeLogLikelihood := func(x [2]float64) float64 {
		return -lagDriftCalibrationLogLikelihood(all, math.Exp(x[0]), math.Exp(x[1]), scratch)
	}
	keptKey := func() []int {
		key := []int{}
		for _, track := range all {
			if track.kept == nil {
				key = append(key, -1)
				continue
			}
			key = append(key, len(track.kept))
			key = append(key, track.kept...)
		}
		return key
	}

	// the first fit keeps every possible track whole
	for _, track := range all {
		track.kept = nil
		if track.possible {
			track.kept = make([]int, len(track.paths))
			for i := range track.kept {
				track.kept[i] = i
			}
		}
	}
	x := [2]float64{math.Log(LagDriftDefaultKappa), math.Log(LagDriftDefaultSigma0Ms)}
	settled := false
	var previous []int
	for pass := 1; pass <= lagDriftMaxPasses; pass += 1 {
		result.Passes = pass
		x, _, settled = lagDriftNelderMead(negativeLogLikelihood, x, 0.4, lo, hi, 400)
		for _, track := range all {
			track.trim(math.Exp(x[0]), math.Exp(x[1]), settings.MinTrackExtenders, scratch)
		}
		key := keptKey()
		if previous != nil && slices.Equal(key, previous) {
			result.Converged = settled
			break
		}
		previous = key
	}
	result.Kappa = math.Exp(x[0])
	result.Sigma0Ms = math.Exp(x[1])
	result.KappaSe, result.Sigma0MsSe = lagDriftCalibrationStandardErrors(negativeLogLikelihood, x)
	result.AtBound = hi[0]-1e-6 <= x[0] || hi[1]-1e-6 <= x[1] || x[0] <= lo[0]+1e-6 || x[1] <= lo[1]+1e-6
	for _, track := range all {
		if track.kept != nil {
			result.FitTracks += 1
			result.FitObservations += len(track.kept)
		}
	}

	switch {
	case result.FitTracks < settings.MinTracks:
		result.Reason = "too_few_tracks"
	case !result.Converged:
		result.Reason = "not_converged"
	case hi[0]-1e-6 <= x[0] || hi[1]-1e-6 <= x[1]:
		result.Reason = "at_upper_bound"
	default:
		result.Applied = true
	}

	// the aggregates, under the calibrated model, or the defaults when it is
	// not applied
	model := result.Model()
	if !result.Applied {
		model = lagdrift.DefaultModel()
	}
	type continentValues struct {
		continent *LagDriftCalibrationContinent
		lastMile  map[string]*LagDriftHistogram
		// the region size's (V11)
		k           *LagDriftHistogram
		kOutOfReach int
	}
	continents := map[string]*continentValues{}
	lastMileMs := map[string]map[string][]float64{}
	addLastMile := func(continentCode string, networkType string, value float64) {
		if lastMileMs[continentCode] == nil {
			lastMileMs[continentCode] = map[string][]float64{}
		}
		lastMileMs[continentCode][networkType] = append(lastMileMs[continentCode][networkType], value)
	}
	for _, track := range all {
		continentCode := track.track.ContinentCode
		var values *continentValues
		if continentCode != "" {
			values = continents[continentCode]
			if values == nil {
				values = &continentValues{
					continent: &LagDriftCalibrationContinent{
						Residuals:  newLagDriftHistogram(LagDriftResidualEdges),
						LastMileMs: map[string]*LagDriftHistogram{},
					},
					lastMile: map[string]*LagDriftHistogram{},
					k:        newLagDriftHistogram(LagDriftKEdges),
				}
				continents[continentCode] = values
			}
			values.continent.Tracks += 1
			values.continent.Observations += len(track.paths)
			if track.kept != nil {
				values.continent.FitTracks += 1
				values.continent.FitObservations += len(track.kept)
			}
			every := make([]int, len(track.paths))
			for i := range every {
				every[i] = i
			}
			if evaluation, ok := track.evaluate(model.Kappa, model.Sigma0Ms, every, scratch, true); ok {
				values.continent.Residuals.Tracks += 1
				for _, z := range evaluation.z {
					values.continent.Residuals.add(LagDriftResidualEdges, z)
				}
			}
		}

		fit, err := lagdrift.FitPosition(model, track.track.City, track.track.Observations, nil)
		if err != nil || len(fit.LastMileMs) != 1 || math.IsNaN(fit.LastMileMs[0]) || math.IsInf(fit.LastMileMs[0], 0) {
			result.UnfittedTracks += 1
			continue
		}
		b := fit.LastMileMs[0]
		networkType := track.track.NetworkType
		if networkType == "" {
			networkType = LagDriftNetworkTypeUnknown
		}
		addLastMile("", "", b)
		addLastMile("", networkType, b)
		if values != nil {
			addLastMile(continentCode, "", b)
			addLastMile(continentCode, networkType, b)
			histogram := values.lastMile[networkType]
			if histogram == nil {
				histogram = newLagDriftHistogram(LagDriftLastMileEdgesMs)
				values.lastMile[networkType] = histogram
			}
			histogram.Tracks += 1
			histogram.add(LagDriftLastMileEdgesMs, b)

			// the region size (V11), from a fit the drift test would judge:
			// how far, in its own σ, the city point lies from it. A city
			// point light rules out from two or more extenders, so that one
			// misplaced extender can't, is a place the test exists to reject
			// and is counted apart
			if settings.KMinExtenders <= fit.Extenders {
				if track.outOfReach <= 1 {
					if d, ok := lagDriftMahalanobis(fit, track.track.City); ok {
						values.k.Tracks += 1
						values.k.add(LagDriftKEdges, d)
					}
				} else {
					values.kOutOfReach += 1
				}
			}
		}
	}

	// only groups of enough tracks are kept, and the sum over continents is
	// of the kept ones, so no histogram can be taken from another to leave a
	// few visitors' values
	sum := &LagDriftCalibrationContinent{
		Residuals:  newLagDriftHistogram(LagDriftResidualEdges),
		LastMileMs: map[string]*LagDriftHistogram{},
	}
	for continentCode, values := range continents {
		// the region size's by its own tracks, which are fewer
		if settings.HistogramMinTracks <= values.k.Tracks {
			result.KHistograms[continentCode] = values.k
			result.KImpossibleTracks[continentCode] = values.kOutOfReach
		}
		if values.continent.Tracks < settings.HistogramMinTracks {
			continue
		}
		for networkType, histogram := range values.lastMile {
			if histogram.Tracks < settings.HistogramMinTracks {
				continue
			}
			values.continent.LastMileMs[networkType] = histogram
			if sum.LastMileMs[networkType] == nil {
				sum.LastMileMs[networkType] = newLagDriftHistogram(LagDriftLastMileEdgesMs)
			}
			sum.LastMileMs[networkType].addHistogram(histogram)
		}
		result.Continents[continentCode] = values.continent
		sum.Tracks += values.continent.Tracks
		sum.Observations += values.continent.Observations
		sum.FitTracks += values.continent.FitTracks
		sum.FitObservations += values.continent.FitObservations
		sum.Residuals.addHistogram(values.continent.Residuals)
	}
	if 0 < len(result.Continents) {
		result.Continents[""] = sum
	}

	if result.Applied {
		result.RelayThresholdMs, result.RelayThresholdsMs = lagDriftRelayThresholds(lastMileMs, settings)
	}
	return result
}
