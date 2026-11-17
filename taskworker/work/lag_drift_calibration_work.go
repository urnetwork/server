package work

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The drift model's weekly calibration (lag.dev/DRIFTTEST.md V3, V4, V11, K2,
// Q12). Every Monday at 04:00 UTC it fits κ and σ₀ to the lag tracks stored
// within the lag retention, the last 7 days, and keeps the relay thresholds
// behind relay_suspected and the week's region-size histograms
// (model.CalibrateLagDrift). The window starts no earlier than the window of
// the calibration in force ended, so no track is binned twice. A calibration
// that applies is stored and in force from then on
// (model.GetLagDriftCalibration), and its week's region-size alarms are raised
// (alertLagDriftKAlarms); one that does not apply changes nothing, and its
// result says why. Only aggregates are stored, and the result holds only
// counts and the fitted values, so nothing per visitor outlives the retention
// (R1).
//
// An operator runs it at once by pulling its pending row forward:
//
//	UPDATE pending_task SET run_at = timezone('utc', now())
//	WHERE run_once_key = '["lag_drift_calibration"]' AND release_time < timezone('utc', now());

// The weekly time it runs at, in UTC: Monday's quietest hour.
const (
	lagDriftCalibrationWeekday = time.Monday
	lagDriftCalibrationHour    = 4
)

// Its task limit. Reading a week of tracks with their pings and fitting them
// takes a few minutes at lag.dev's volume.
const lagDriftCalibrationMaxTime = 30 * time.Minute

// The most tracks one calibration reads, the newest first. κ and σ₀ need far
// fewer (2,000 fix κ to 1%), but every track adds to its continent's
// region-size histogram, and a continent's k counts only once 26 weeks of them
// hold 10,000 tracks (V11), about 385 a week. Each track is about 2,000 pings
// read from lag_result, so this bounds the job's read at about 800 MB, in
// batches of 500 tracks, once a week.
const lagDriftCalibrationMaxTracks = 50000

type LagDriftCalibrationArgs struct {
}

// What a calibration found, as the task keeps it: counts and the fitted
// values, never a track or a visitor. The histograms are in the stored
// calibration's summary.
type LagDriftCalibrationResult struct {
	// whether the calibration was stored and is in force; why not, if not
	Applied       bool       `json:"applied"`
	Reason        string     `json:"reason,omitempty"`
	CalibrationId *server.Id `json:"calibration_id,omitempty"`

	Kappa            float64  `json:"kappa"`
	Sigma0Ms         float64  `json:"sigma0_ms"`
	KappaSe          *float64 `json:"kappa_se"`
	Sigma0MsSe       *float64 `json:"sigma0_ms_se"`
	RelayThresholdMs float64  `json:"relay_threshold_ms"`

	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	// the tracks read, those with too few usable extenders at a known place,
	// and the extenders the lag sample places nowhere
	ReadTracks        int `json:"read_tracks"`
	ShortTracks       int `json:"short_tracks"`
	UnplacedExtenders int `json:"unplaced_extenders"`
	// the tracks and observations fitted, and those the fit kept
	Tracks           int  `json:"tracks"`
	Observations     int  `json:"observations"`
	FitTracks        int  `json:"fit_tracks"`
	FitObservations  int  `json:"fit_observations"`
	ImpossibleTracks int  `json:"impossible_tracks"`
	UnfittedTracks   int  `json:"unfitted_tracks"`
	Passes           int  `json:"passes"`
	Converged        bool `json:"converged"`
	AtBound          bool `json:"at_bound"`
	// the week's tracks in each continent's region-size histogram (V11), and
	// those left out because light rules out their city point
	KTracks           map[string]int `json:"k_tracks"`
	KImpossibleTracks map[string]int `json:"k_impossible_tracks"`
	// by continent, the region-size alarms the stored week raised: tail,
	// drift or overflow (alertLagDriftKAlarms)
	KAlarms map[string][]string `json:"k_alarms,omitempty"`
}

// The region-size alarms the weekly calibration raised (V11), by continent
// and kind: tail, a week far beyond the trailing 26 weeks' 99% bound; drift,
// that bound moved more than 15% in 26 weeks; overflow, that bound beyond the
// histograms' last edge (model.GetLagDriftKAlarms). Any one means the k table
// should be derived again with T3. Its labels are bounded: the continent codes
// and the three kinds.
var lagDriftCalibrationKAlarmTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "drift_calibration_k_alarm_total",
		Help:      "Region-size alarms of the weekly drift calibration (lag.dev/DRIFTTEST.md V11), by continent and kind: tail (more than 2% of a week of 2,000 tracks beyond the trailing 26 weeks' 99% bound), drift (that bound moved more than 15% from 26 weeks earlier) or overflow (that bound beyond the histograms' last edge). Any one means the k table should be derived again",
	},
	[]string{"continent", "kind"},
)

func init() {
	prometheus.MustRegister(lagDriftCalibrationKAlarmTotal)
}

// Raises each region-size alarm of a stored week (V11): logs it as an error,
// counts it by continent and kind, and answers the kinds by continent, nil
// for none. The k table stays as it is: an alarm is for a person to derive it
// again with T3 before the next release.
func alertLagDriftKAlarms(calibrationId server.Id, alarms []*model.LagDriftKAlarm) map[string][]string {
	kinds := map[string][]string{}
	for _, alarm := range alarms {
		kinds[alarm.ContinentCode] = append(kinds[alarm.ContinentCode], alarm.Kind)
		lagDriftCalibrationKAlarmTotal.WithLabelValues(alarm.ContinentCode, alarm.Kind).Inc()
		switch alarm.Kind {
		case model.LagDriftKAlarmTail:
			glog.Errorf(
				"[lag]drift calibration %s: %s's week puts %.1f%% of its %d tracks beyond the trailing 26 weeks' 99%% bound of %.2f, over twice the 1%% expected; derive the k table again (lag.dev/DRIFTTEST.md V11)\n",
				calibrationId,
				alarm.ContinentCode,
				100*alarm.ShareBeyond,
				alarm.WeekTracks,
				alarm.BoundK,
			)
		case model.LagDriftKAlarmDrift:
			glog.Errorf(
				"[lag]drift calibration %s: %s's 26 weeks' 99%% bound moved to %.2f over %d tracks from %.2f over %d 26 weeks earlier (NaN is beyond the histograms' last edge); derive the k table again (lag.dev/DRIFTTEST.md V11)\n",
				calibrationId,
				alarm.ContinentCode,
				alarm.BoundK,
				alarm.Tracks,
				alarm.EarlierBoundK,
				alarm.EarlierTracks,
			)
		default:
			glog.Errorf(
				"[lag]drift calibration %s: %s's 26 weeks' 99%% bound over %d tracks is beyond the histograms' last edge, a tail no honest fit has; read k_histograms and k_impossible_tracks in the summaries, and derive the k table again (lag.dev/DRIFTTEST.md V11)\n",
				calibrationId,
				alarm.ContinentCode,
				alarm.Tracks,
			)
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	return kinds
}

// The first weekly time after `now`.
func nextLagDriftCalibrationTime(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), lagDriftCalibrationHour, 0, 0, 0, time.UTC)
	at = at.AddDate(0, 0, (int(lagDriftCalibrationWeekday)-int(at.Weekday())+7)%7)
	if !now.Before(at) {
		at = at.AddDate(0, 0, 7)
	}
	return at
}

// Schedules the next calibration at the next weekly time, once per chain. A
// restart keeps a pending row's earlier time (task.RunOnce), so it never
// postpones a calibration.
func ScheduleLagDriftCalibration(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		LagDriftCalibration,
		&LagDriftCalibrationArgs{},
		clientSession,
		task.RunOnce("lag_drift_calibration"),
		task.RunAt(nextLagDriftCalibrationTime(server.NowUtc())),
		task.MaxTime(lagDriftCalibrationMaxTime),
	)
}

// Calibrates the drift model over the lag retention, as the clock reads now.
func LagDriftCalibration(
	_ *LagDriftCalibrationArgs,
	clientSession *session.ClientSession,
) (*LagDriftCalibrationResult, error) {
	return calibrateLagDrift(
		clientSession.Ctx,
		server.NowUtc(),
		controller.GetLagSettings(),
		model.DefaultLagDriftCalibrationSettings(),
		lagDriftCalibrationMaxTracks,
	), nil
}

// The calibration as the clock reads `now`, over what the lag retention keeps
// since the window of the calibration in force ended: a run beside the weekly
// one, at once or after a restart, never reads a track twice into the 26 weeks
// the region size sums (V11, drift-review V-5).
func calibrateLagDrift(
	ctx context.Context,
	now time.Time,
	lagSettings *controller.LagSettings,
	settings *model.LagDriftCalibrationSettings,
	maxTracks int,
) *LagDriftCalibrationResult {
	windowStart := now.Add(-lagSettings.ResultRetention)
	if windowEnd := model.GetLagDriftCalibrationWindowEnd(ctx); windowEnd != nil && windowStart.Before(*windowEnd) {
		windowStart = *windowEnd
	}
	run := model.CalibrateLagDrift(ctx, now, windowStart, now, settings, maxTracks)
	result := &LagDriftCalibrationResult{
		Applied:           run.CalibrationId != nil,
		Reason:            run.Result.Reason,
		CalibrationId:     run.CalibrationId,
		Kappa:             run.Result.Kappa,
		Sigma0Ms:          run.Result.Sigma0Ms,
		KappaSe:           run.Result.KappaSe,
		Sigma0MsSe:        run.Result.Sigma0MsSe,
		RelayThresholdMs:  run.Result.RelayThresholdMs,
		WindowStart:       run.WindowStart,
		WindowEnd:         run.WindowEnd,
		ReadTracks:        run.ReadTracks,
		ShortTracks:       run.ShortTracks,
		UnplacedExtenders: run.UnplacedExtenders,
		Tracks:            run.Result.Tracks,
		Observations:      run.Result.Observations,
		FitTracks:         run.Result.FitTracks,
		FitObservations:   run.Result.FitObservations,
		ImpossibleTracks:  run.Result.ImpossibleTracks,
		UnfittedTracks:    run.Result.UnfittedTracks,
		Passes:            run.Result.Passes,
		Converged:         run.Result.Converged,
		AtBound:           run.Result.AtBound,
		KTracks:           map[string]int{},
		KImpossibleTracks: run.Result.KImpossibleTracks,
	}
	for continentCode, histogram := range run.Result.KHistograms {
		result.KTracks[continentCode] = histogram.Tracks
	}
	if run.Result.Applied && run.CalibrationId == nil {
		// the fit applied, but the task's context ended before it was stored
		result.Reason = "canceled"
	}
	if result.Applied {
		glog.Infof(
			"[lag]drift calibration %s: kappa %.4f, sigma0 %.3f ms, relay threshold %.1f ms, from %d of %d tracks and %d of %d observations\n",
			result.CalibrationId,
			result.Kappa,
			result.Sigma0Ms,
			result.RelayThresholdMs,
			result.FitTracks,
			result.Tracks,
			result.FitObservations,
			result.Observations,
		)
		result.KAlarms = alertLagDriftKAlarms(*result.CalibrationId, model.GetLagDriftKAlarms(ctx, *result.CalibrationId))
	} else {
		glog.Infof(
			"[lag]drift calibration not applied (%s): %d tracks read, %d fitted; the calibration in force stays\n",
			result.Reason,
			result.ReadTracks,
			result.FitTracks,
		)
	}
	return result
}

// Re-arms the chain for the next weekly time, in the transaction that
// finishes this calibration.
func LagDriftCalibrationPost(
	_ *LagDriftCalibrationArgs,
	_ *LagDriftCalibrationResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleLagDriftCalibration(clientSession, tx)
	return nil
}
