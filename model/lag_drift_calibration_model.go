package model

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
)

// The drift model's calibration (lag.dev/DRIFTTEST.md V3, V4, K2, Q12).
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
	// the calibration these are, for a verdict to record what it was judged
	// with; nil for the defaults
	CalibrationId *server.Id
	// when the calibration ran; nil for the defaults
	CalibratedAt *time.Time
}

// The design's values, for when nothing has been calibrated.
func DefaultLagDriftCalibration() *LagDriftCalibration {
	return &LagDriftCalibration{
		Kappa:             LagDriftDefaultKappa,
		Sigma0Ms:          LagDriftDefaultSigma0Ms,
		RelayThresholdMs:  LagDriftDefaultRelayThresholdMs,
		RelayThresholdsMs: map[string]map[string]float64{},
	}
}

// Whether the values are a calibration's rather than the defaults.
func (self *LagDriftCalibration) Calibrated() bool {
	return self.CalibrationId != nil
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
