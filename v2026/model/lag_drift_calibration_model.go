package model

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026/lagdrift"

	"github.com/urnetwork/server/v2026"
)

// The drift model's calibration (lag.dev/DRIFTTEST.md V3, V4, V11, K2, Q12).
//
// Each extender's round trip in a drift fit has the variance
// σ_meas² + σ₀² + (κ · K · d)² + (K · s_ext)² (V3). κ, the route error as a
// share of distance, and σ₀, the floor for the stack's own timing, are fitted
// once a week by maximum likelihood over the last 7 days of lag tracks whose
// city point is known (V4). The same pass keeps what is normal for the
// last-mile offset b by continent and network type, and from it the relay
// threshold: a run whose b is above it is reported `relay_suspected`, an
// advisory flag that never changes the verdict (K2, Q12).
//
// It also measures the region's size (V11): how far, in its own σ, each
// track's city point lies from the track's single-run fit, as a histogram per
// continent. k itself is a fixed table, lagdrift.DefaultK, since lag.dev's
// tracks, with no mid band, need a wider region than a drift run (review V-8).
// The measurement is an alarm: a week whose tail is far beyond the trailing 26
// weeks', or a 26 weeks' 99% bound far from the one 26 weeks before, means
// the network changed and the table needs deriving again (GetLagDriftKAlarms).
//
// Only aggregates are kept: the two numbers, histograms and thresholds. No
// track, visitor, address or place is, so nothing per visitor outlives lag's
// 7-day retention (R1). Until the first calibration, and whenever none is
// stored, the design's defaults apply.

// The design's values until the first calibration (lag.dev/DRIFTTEST.md V3,
// K2): κ, σ₀ and the relay threshold.
const (
	LagDriftDefaultKappa            = 0.25
	LagDriftDefaultSigma0Ms         = 1.0
	LagDriftDefaultRelayThresholdMs = 80.0
)

// No calibrated relay threshold is below this. A continent and network type
// whose b is tight, such as fiber where its 99th percentile is under 20 ms,
// would otherwise flag a visitor on a busy Wi-Fi.
const LagDriftRelayFloorMs = 30.0

// A visitor's network type, from its network's autonomous system where that
// tells (K2): the last-mile offset b is normal at a few ms on a fixed line,
// tens of ms on a mobile network and more on a satellite link. A network with
// no number is unknown. Hosting and cloud networks are automated tracks
// (lag.dev/PLAN.md amendment 12), never calibrated, so they have no type.
const (
	LagDriftNetworkTypeFixed     = "fixed"
	LagDriftNetworkTypeMobile    = "mobile"
	LagDriftNetworkTypeSatellite = "satellite"
	LagDriftNetworkTypeUnknown   = "unknown"
)

// Every network type, in the order the calibration keeps them.
var LagDriftNetworkTypes = []string{
	LagDriftNetworkTypeFixed,
	LagDriftNetworkTypeMobile,
	LagDriftNetworkTypeSatellite,
	LagDriftNetworkTypeUnknown,
}

// Satellite access networks by autonomous system number. A curated list of
// the largest, as lag's hosting list is (controller/lag_automated.go), not a
// census: a network outside it is typed by its name, else as fixed.
var lagDriftSatelliteAsns = map[int64]string{
	14593: "Starlink",
	7155:  "Viasat",
	6621:  "Hughes",
}

// Mobile networks by autonomous system number, for the large ones whose name
// does not say so. Networks that carry fixed and mobile lines under one
// number are left out, and count as fixed.
var lagDriftMobileAsns = map[int64]string{
	10507: "Sprint",
	12576: "EE",
	15897: "Vodafone Turkey",
	16135: "Turkcell",
	24203: "XL Axiata",
	26615: "TIM Brasil",
	29465: "MTN Nigeria",
	31133: "MegaFon",
	38266: "Vodafone Idea",
	55836: "Reliance Jio",
	10139: "Smart",
}

// What a network's name says of its type, lower case, satellite first.
var lagDriftSatelliteNameParts = []string{
	"starlink",
	"space exploration technologies",
	"satellite",
	"viasat",
	"hughes network",
}

var lagDriftMobileNameParts = []string{
	"mobil",
	"movil",
	"móvil",
	"wireless",
	"cellular",
	"celular",
	"cellco",
	"telkomsel",
	"docomo",
	"gprs",
}

// LagDriftNetworkType types a visitor's network from its autonomous system
// (lag_track's client_asn and client_asn_org): satellite or mobile when its
// number or its name says so, fixed otherwise, and unknown without a number.
func LagDriftNetworkType(asn *int64, asnOrg string) string {
	if asn == nil {
		return LagDriftNetworkTypeUnknown
	}
	name := strings.ToLower(asnOrg)
	if _, ok := lagDriftSatelliteAsns[*asn]; ok || lagDriftNameHasAny(name, lagDriftSatelliteNameParts) {
		return LagDriftNetworkTypeSatellite
	}
	if _, ok := lagDriftMobileAsns[*asn]; ok || lagDriftNameHasAny(name, lagDriftMobileNameParts) {
		return LagDriftNetworkTypeMobile
	}
	return LagDriftNetworkTypeFixed
}

func lagDriftNameHasAny(name string, parts []string) bool {
	for _, part := range parts {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}

// The drift model's constants and relay thresholds (lag.dev/DRIFTTEST.md V3,
// V4, K2, Q12), as GetLagDriftCalibration reads them.
type LagDriftCalibration struct {
	// κ: route error as a share of distance (V3)
	Kappa float64
	// σ₀: the floor for the stack's own timing, in ms (V3)
	Sigma0Ms float64
	// b above which a run is relay_suspected, in ms, over every continent
	// and network type: the threshold for a visitor whose continent and
	// network type have none of their own (K2, Q12)
	RelayThresholdMs float64
	// the relay thresholds in ms by continent code (ContinentCodes), "" for
	// every continent, then by network type (LagDriftNetworkTypes), "" for
	// every type: only where the calibration had enough tracks. Empty for the
	// defaults
	RelayThresholdsMs map[string]map[string]float64
	// the measured region size (V11) by continent code: the region-size
	// histograms of the calibrations in force over the LagDriftKWindow up to
	// this one, summed. It doesn't set k, which is a fixed table (K); the
	// weekly job's alarms watch it (GetLagDriftKAlarms). Empty for the
	// defaults
	CalibratedK map[string]*LagDriftCalibratedK
	// the calibration these are, for a verdict to record what it was judged
	// with; nil for the defaults
	CalibrationId *server.Id
	// when the calibration ran; nil for the defaults
	CalibratedAt *time.Time
}

// The confidences a drift test judges at (V11, P2), the ones the measured
// region size is kept for.
var LagDriftKConfidences = []float64{0.95, 0.99}

// A continent's 26 weeks of region-size histograms are enough to hold a 99%
// bound to about ±0.15 from this many tracks (V11): the drift alarm compares
// two such bounds only when both have them.
const LagDriftKMinTracks = 10000

// The weeks of region-size histograms summed up to a calibration (V11).
const LagDriftKWindow = 26 * 7 * 24 * time.Hour

// A continent's measured region size (V11).
type LagDriftCalibratedK struct {
	// the tracks in its summed histograms
	Tracks int64
	// at each of LagDriftKConfidences, in their order, an upper bound on the
	// quantile of the Mahalanobis distance of a city point from its track's
	// fit (lagDriftHistogramUpperBound); NaN where it is unusable, beyond the
	// histogram's last edge
	Quantiles []float64
}

// The design's values, for when nothing has been calibrated.
func DefaultLagDriftCalibration() *LagDriftCalibration {
	return &LagDriftCalibration{
		Kappa:             LagDriftDefaultKappa,
		Sigma0Ms:          LagDriftDefaultSigma0Ms,
		RelayThresholdMs:  LagDriftDefaultRelayThresholdMs,
		RelayThresholdsMs: map[string]map[string]float64{},
		CalibratedK:       map[string]*LagDriftCalibratedK{},
	}
}

// Whether the values are a calibration's rather than the defaults.
func (self *LagDriftCalibration) Calibrated() bool {
	return self.CalibrationId != nil
}

// K is the region's size at a confidence for a visitor on a continent, the
// continent code of client_ip's country (V11): lagdrift.DefaultK, a fixed
// table from T3's drift geometry, for every calibration and the defaults
// alike. A k measured on lag.dev's tracks doesn't carry over to a drift run:
// lag.dev pings 8 extenders with no mid band, whose fits need 1.5–2 times the
// k a drift run's 12 do (review V-8). So the weekly measurement only alarms
// (GetLagDriftKAlarms). The drift test asks here, so a change of rule is made
// in one place.
func (self *LagDriftCalibration) K(confidence float64, continentCode string) float64 {
	return lagdrift.DefaultK(confidence, continentCode)
}

// The one-sided normal quantile of a 95% upper bound.
const lagDriftKBoundZ = 1.645

// An upper bound on the q quantile of a histogram over edges (V11, drift-review
// V-3): the upper edge of the bin holding the value at rank
// n·q + 1.645·√(n·q·(1 − q)), a one-sided 95% bound on the quantile by the
// binomial's normal approximation, so the k it gives is above the true quantile
// at least 95% of the time rather than half. The bin below the first edge has
// the first edge as its upper edge. False when the rank lands in the bin from
// the last edge up, beyond what the histogram can tell, or past the sample.
func lagDriftHistogramUpperBound(edges []float64, counts []int64, q float64) (float64, bool) {
	total := int64(0)
	for _, count := range counts {
		total += count
	}
	if total == 0 || len(counts) != len(edges)+1 {
		return math.NaN(), false
	}
	n := float64(total)
	rank := max(int64(math.Ceil(n*q+lagDriftKBoundZ*math.Sqrt(n*q*(1-q)))), 1)
	if total < rank {
		return math.NaN(), false
	}
	cumulative := int64(0)
	for bin, count := range counts {
		cumulative += count
		if rank <= cumulative {
			if bin == len(edges) {
				return math.NaN(), false
			}
			return edges[bin], true
		}
	}
	return math.NaN(), false
}

// A calibration's region-size histograms by continent, from its summary's
// JSON; nil when it has none, or has them over other edges.
func parseLagDriftKHistograms(edgesJson []byte, histogramsJson []byte) map[string]*LagDriftHistogram {
	if len(edgesJson) == 0 || len(histogramsJson) == 0 {
		return nil
	}
	var edges []float64
	var histograms map[string]*LagDriftHistogram
	if json.Unmarshal(edgesJson, &edges) != nil || !slices.Equal(edges, LagDriftKEdges) {
		return nil
	}
	if json.Unmarshal(histogramsJson, &histograms) != nil {
		return nil
	}
	parsed := map[string]*LagDriftHistogram{}
	for continentCode, histogram := range histograms {
		if histogram != nil && len(histogram.Counts) == len(LagDriftKEdges)+1 {
			parsed[continentCode] = histogram
		}
	}
	return parsed
}

// The region-size histograms of the calibrations in force calibrated after
// `after` and up to `through`, or before it when through is excluded, summed
// by continent (V11). A withdrawn calibration's histograms count for nothing.
func sumLagDriftKHistogramsInConn(
	ctx context.Context,
	conn server.PgConn,
	after time.Time,
	through time.Time,
	throughIncluded bool,
) map[string]*LagDriftHistogram {
	sums := map[string]*LagDriftHistogram{}
	result, err := conn.Query(
		ctx,
		`
		SELECT summary_json->'k_edges', summary_json->'k_histograms'
		FROM lag_drift_calibration
		WHERE
			withdrawn_at IS NULL AND
			$1 < calibrated_at AND
			(calibrated_at < $2 OR ($3 AND calibrated_at = $2))
		`,
		after.UTC(),
		through.UTC(),
		throughIncluded,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var edgesJson []byte
			var histogramsJson []byte
			server.Raise(result.Scan(&edgesJson, &histogramsJson))
			for continentCode, histogram := range parseLagDriftKHistograms(edgesJson, histogramsJson) {
				sum := sums[continentCode]
				if sum == nil {
					sum = newLagDriftHistogram(LagDriftKEdges)
					sums[continentCode] = sum
				}
				sum.addHistogram(histogram)
			}
		}
	})
	return sums
}

// The measured region size by continent from summed histograms: each
// confidence's upper bound, NaN where unusable.
func lagDriftCalibratedK(sums map[string]*LagDriftHistogram) map[string]*LagDriftCalibratedK {
	calibratedK := map[string]*LagDriftCalibratedK{}
	for continentCode, sum := range sums {
		k := &LagDriftCalibratedK{Tracks: int64(sum.Tracks)}
		for _, confidence := range LagDriftKConfidences {
			bound, _ := lagDriftHistogramUpperBound(LagDriftKEdges, sum.Counts, confidence)
			k.Quantiles = append(k.Quantiles, bound)
		}
		calibratedK[continentCode] = k
	}
	return calibratedK
}

// The measured region size by continent over the LagDriftKWindow up to
// calibratedAt (V11).
func getLagDriftCalibratedKInConn(ctx context.Context, conn server.PgConn, calibratedAt time.Time) map[string]*LagDriftCalibratedK {
	return lagDriftCalibratedK(sumLagDriftKHistogramsInConn(ctx, conn, calibratedAt.Add(-LagDriftKWindow), calibratedAt, true))
}

// The weekly measurement's alarms (V11): either means the network's geometry
// or paths changed, and the k table should be derived again with T3 before
// the next release.
const (
	// a week puts more than twice the expected share of its tracks beyond the
	// trailing 26 weeks' 99% bound
	LagDriftKAlarmTail = "tail"
	// the 26 weeks' 99% bound moved more than 15% from 26 weeks earlier
	LagDriftKAlarmDrift = "drift"
	// the 26 weeks' 99% bound is beyond the histograms' last edge, with
	// enough tracks to tell: a tail no honest fit has
	LagDriftKAlarmOverflow = "overflow"
)

// The alarms' thresholds (V11).
const (
	// the confidence they watch
	LagDriftKAlarmConfidence = 0.99
	// a week needs this many tracks for the tail alarm to judge it
	LagDriftKAlarmTailMinTracks = 2000
	// the share of a week's tracks beyond the trailing 99% bound that raises
	// the tail alarm: twice the 1% expected
	LagDriftKAlarmTailShare = 0.02
	// the relative move of the 99% bound over 26 weeks that raises the drift
	// alarm
	LagDriftKAlarmDriftShare = 0.15
)

// One alarm of the weekly measurement, for a continent (V11).
type LagDriftKAlarm struct {
	ContinentCode string
	// LagDriftKAlarmTail, LagDriftKAlarmDrift or LagDriftKAlarmOverflow
	Kind string
	// the 99% bound it is about: the trailing 26 weeks' (tail) or the 26 weeks'
	// up to the week (drift, overflow), NaN when unusable, and their tracks
	BoundK float64
	Tracks int64
	// tail: the week's tracks, and the share of them beyond BoundK
	WeekTracks  int64
	ShareBeyond float64
	// drift: the 26 weeks' bound 26 weeks earlier, NaN when unusable, and
	// their tracks
	EarlierBoundK float64
	EarlierTracks int64
}

// The share of a histogram's tracks at or beyond bound, an edge of
// LagDriftKEdges: those in the bins from that edge up.
func lagDriftShareBeyond(histogram *LagDriftHistogram, bound float64) float64 {
	total := int64(0)
	beyond := int64(0)
	for bin, count := range histogram.Counts {
		total += count
		// bin is [edges[bin−1], edges[bin]); the last is from the last edge up
		if 0 < bin && bound <= LagDriftKEdges[bin-1] {
			beyond += count
		}
	}
	if total == 0 {
		return 0
	}
	return float64(beyond) / float64(total)
}

// The alarms of a week's histograms by continent, against the 26 weeks before
// it (trailing), the 26 weeks up to and with it (current) and the 26 weeks
// before those (earlier), in continent order (V11). Pure.
//
//   - tail: the week has LagDriftKAlarmTailMinTracks tracks, and more than
//     LagDriftKAlarmTailShare of them are beyond the trailing 26 weeks' 99%
//     upper bound, where that bound can be had;
//   - drift: the current and the earlier 26 weeks each have LagDriftKMinTracks
//     tracks, and their 99% upper bounds differ by more than
//     LagDriftKAlarmDriftShare of the earlier, or one of them is beyond the
//     histograms' last edge and the other is not;
//   - overflow: the current 26 weeks have LagDriftKMinTracks tracks and their
//     99% upper bound is beyond the histograms' last edge.
func lagDriftKAlarms(week, trailing, current, earlier map[string]*LagDriftHistogram) []*LagDriftKAlarm {
	continentCodes := map[string]bool{}
	for _, sums := range []map[string]*LagDriftHistogram{week, current} {
		for continentCode := range sums {
			continentCodes[continentCode] = true
		}
	}
	bound := func(histogram *LagDriftHistogram) (float64, bool) {
		return lagDriftHistogramUpperBound(LagDriftKEdges, histogram.Counts, LagDriftKAlarmConfidence)
	}
	alarms := []*LagDriftKAlarm{}
	for _, continentCode := range slices.Sorted(maps.Keys(continentCodes)) {
		if w, t := week[continentCode], trailing[continentCode]; w != nil && t != nil && LagDriftKAlarmTailMinTracks <= w.Tracks {
			if b, ok := bound(t); ok {
				if share := lagDriftShareBeyond(w, b); LagDriftKAlarmTailShare < share {
					alarms = append(alarms, &LagDriftKAlarm{
						ContinentCode: continentCode,
						Kind:          LagDriftKAlarmTail,
						BoundK:        b,
						Tracks:        int64(t.Tracks),
						WeekTracks:    int64(w.Tracks),
						ShareBeyond:   share,
					})
				}
			}
		}
		c := current[continentCode]
		if c == nil || int64(c.Tracks) < LagDriftKMinTracks {
			continue
		}
		now, nowOk := bound(c)
		if e := earlier[continentCode]; e != nil && LagDriftKMinTracks <= int64(e.Tracks) {
			then, thenOk := bound(e)
			moved := nowOk != thenOk || (nowOk && thenOk && LagDriftKAlarmDriftShare*then < math.Abs(now-then))
			if moved {
				alarms = append(alarms, &LagDriftKAlarm{
					ContinentCode: continentCode,
					Kind:          LagDriftKAlarmDrift,
					BoundK:        now,
					Tracks:        int64(c.Tracks),
					EarlierBoundK: then,
					EarlierTracks: int64(e.Tracks),
				})
			}
		}
		if !nowOk {
			alarms = append(alarms, &LagDriftKAlarm{
				ContinentCode: continentCode,
				Kind:          LagDriftKAlarmOverflow,
				BoundK:        now,
				Tracks:        int64(c.Tracks),
			})
		}
	}
	return alarms
}

// GetLagDriftKAlarms is the weekly measurement's alarms for a stored
// calibration's week (V11): its own region-size histograms against the 26
// weeks before it, and the 26 weeks up to it against the 26 before those
// (lagDriftKAlarms). Calibrations withdrawn count for nothing; nil when the
// calibration is gone or has no histograms.
func GetLagDriftKAlarms(ctx context.Context, calibrationId server.Id) []*LagDriftKAlarm {
	var alarms []*LagDriftKAlarm
	server.Db(ctx, func(conn server.PgConn) {
		var calibratedAt time.Time
		var edgesJson []byte
		var histogramsJson []byte
		found := false
		result, err := conn.Query(
			ctx,
			`
			SELECT calibrated_at, summary_json->'k_edges', summary_json->'k_histograms'
			FROM lag_drift_calibration
			WHERE calibration_id = $1
			`,
			calibrationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&calibratedAt, &edgesJson, &histogramsJson))
				found = true
			}
		})
		week := parseLagDriftKHistograms(edgesJson, histogramsJson)
		if !found || week == nil {
			return
		}
		alarms = lagDriftKAlarms(
			week,
			sumLagDriftKHistogramsInConn(ctx, conn, calibratedAt.Add(-LagDriftKWindow), calibratedAt, false),
			sumLagDriftKHistogramsInConn(ctx, conn, calibratedAt.Add(-LagDriftKWindow), calibratedAt, true),
			sumLagDriftKHistogramsInConn(ctx, conn, calibratedAt.Add(-2*LagDriftKWindow), calibratedAt.Add(-LagDriftKWindow), true),
		)
	})
	return alarms
}

// RelayThresholdMsFor is the relay threshold in ms for a visitor on a
// continent (ContinentCodeForCountry of the IP country) and network type
// (LagDriftNetworkType): the most specific one the calibration has. The
// network type comes before the continent, since it sets b far more: the
// continent's for the type, every continent's for the type, the continent's
// for every type, and else RelayThresholdMs.
func (self *LagDriftCalibration) RelayThresholdMsFor(continentCode string, networkType string) float64 {
	lookup := func(continentCode string, networkType string) (float64, bool) {
		thresholdMs, ok := self.RelayThresholdsMs[continentCode][networkType]
		return thresholdMs, ok && 0 < thresholdMs
	}
	if continentCode != "" && networkType != "" {
		if thresholdMs, ok := lookup(continentCode, networkType); ok {
			return thresholdMs
		}
	}
	if networkType != "" {
		if thresholdMs, ok := lookup("", networkType); ok {
			return thresholdMs
		}
	}
	if continentCode != "" {
		if thresholdMs, ok := lookup(continentCode, ""); ok {
			return thresholdMs
		}
	}
	return self.RelayThresholdMs
}

// Whether a run's last-mile offset b, in ms, is far above normal for the
// visitor's continent and network type (K2, Q12).
func (self *LagDriftCalibration) RelaySuspected(lastMileMs float64, continentCode string, networkType string) bool {
	return self.RelayThresholdMsFor(continentCode, networkType) < lastMileMs
}

// κ and σ₀ as lagdrift's fits take them (V3).
func (self *LagDriftCalibration) Model() lagdrift.Model {
	return lagdrift.Model{
		Kappa:    self.Kappa,
		Sigma0Ms: self.Sigma0Ms,
	}
}

// A calibration row's columns, in the order scanLagDriftCalibration reads
// them. Of the summary only the relay thresholds are read.
const lagDriftCalibrationColumns = `
	calibration_id,
	calibrated_at,
	kappa,
	sigma0_ms,
	relay_threshold_ms,
	summary_json->'relay_thresholds_ms'
`

func scanLagDriftCalibration(result server.PgResult) *LagDriftCalibration {
	calibration := DefaultLagDriftCalibration()
	var calibrationId server.Id
	var calibratedAt time.Time
	var thresholdsJson []byte
	server.Raise(result.Scan(
		&calibrationId,
		&calibratedAt,
		&calibration.Kappa,
		&calibration.Sigma0Ms,
		&calibration.RelayThresholdMs,
		&thresholdsJson,
	))
	calibration.CalibrationId = &calibrationId
	calibration.CalibratedAt = &calibratedAt
	if 0 < len(thresholdsJson) {
		server.Raise(json.Unmarshal(thresholdsJson, &calibration.RelayThresholdsMs))
	}
	if calibration.RelayThresholdsMs == nil {
		calibration.RelayThresholdsMs = map[string]map[string]float64{}
	}
	return calibration
}

// GetLagDriftCalibration reads the drift model's calibration in force
// (lag.dev/DRIFTTEST.md V4): the latest one not withdrawn, or the defaults
// when there is none: κ 0.25, σ₀ 1 ms and a relay threshold of 80 ms.
func GetLagDriftCalibration(ctx context.Context) *LagDriftCalibration {
	calibration := DefaultLagDriftCalibration()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+lagDriftCalibrationColumns+`
			FROM lag_drift_calibration
			WHERE withdrawn_at IS NULL
			ORDER BY calibrated_at DESC, calibration_id DESC
			LIMIT 1
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				calibration = scanLagDriftCalibration(result)
			}
		})
		if calibration.CalibratedAt != nil {
			calibration.CalibratedK = getLagDriftCalibratedKInConn(ctx, conn, *calibration.CalibratedAt)
		}
	})
	return calibration
}

// GetLagDriftCalibrationById reads one calibration, withdrawn or not, so a
// verdict that recorded it can be judged again as it was; nil when there is
// no such calibration.
func GetLagDriftCalibrationById(ctx context.Context, calibrationId server.Id) *LagDriftCalibration {
	var calibration *LagDriftCalibration
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT `+lagDriftCalibrationColumns+`
			FROM lag_drift_calibration
			WHERE calibration_id = $1
			`,
			calibrationId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				calibration = scanLagDriftCalibration(result)
			}
		})
		if calibration != nil {
			calibration.CalibratedK = getLagDriftCalibratedKInConn(ctx, conn, *calibration.CalibratedAt)
		}
	})
	return calibration
}

// AddLagDriftCalibration stores an applied calibration, which is in force
// from then on, and answers its id (V4). The row holds only aggregates: the
// result's κ, σ₀, thresholds, histograms and counts (R1).
func AddLagDriftCalibration(
	ctx context.Context,
	calibratedAt time.Time,
	windowStart time.Time,
	windowEnd time.Time,
	result *LagDriftCalibrationResult,
) server.Id {
	summaryJson, err := json.Marshal(result)
	server.Raise(err)
	calibrationId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO lag_drift_calibration (
				calibration_id,
				calibrated_at,
				window_start,
				window_end,
				fit_tracks,
				fit_observations,
				kappa,
				sigma0_ms,
				relay_threshold_ms,
				summary_json
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
			calibrationId,
			calibratedAt.UTC(),
			windowStart.UTC(),
			windowEnd.UTC(),
			result.FitTracks,
			result.FitObservations,
			result.Kappa,
			result.Sigma0Ms,
			result.RelayThresholdMs,
			summaryJson,
		))
	})
	return calibrationId
}

// WithdrawLagDriftCalibration takes a calibration out of force: the drift
// test goes back to the latest one before it that is still in force, or to
// the defaults, until the next calibration. The row stays, for the verdicts
// that name it. False when there is no such calibration in force.
func WithdrawLagDriftCalibration(ctx context.Context, calibrationId server.Id, now time.Time) (withdrawn bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_calibration
			SET withdrawn_at = $2
			WHERE calibration_id = $1 AND withdrawn_at IS NULL
			`,
			calibrationId,
			now.UTC(),
		))
		withdrawn = tag.RowsAffected() == 1
	})
	return
}

// WithdrawLagDriftCalibrations takes every calibration out of force, so the
// drift test goes back to the defaults until the next calibration, and
// answers how many it withdrew.
func WithdrawLagDriftCalibrations(ctx context.Context, now time.Time) (withdrawn int) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE lag_drift_calibration
			SET withdrawn_at = $1
			WHERE withdrawn_at IS NULL
			`,
			now.UTC(),
		))
		withdrawn = int(tag.RowsAffected())
	})
	return
}

// GetLagDriftCalibrationWindowEnd is where the latest window of a calibration
// in force ended, nil when none is in force. The next calibration's window
// starts there, if that is later than the lag retention allows, so a run
// beside the weekly one never bins a track twice into the 26 weeks the region
// size sums (V11, drift-review V-5). A withdrawn calibration's tracks count
// again.
func GetLagDriftCalibrationWindowEnd(ctx context.Context) (windowEnd *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT MAX(window_end)
			FROM lag_drift_calibration
			WHERE withdrawn_at IS NULL
			`,
		).Scan(&windowEnd))
	})
	return
}
