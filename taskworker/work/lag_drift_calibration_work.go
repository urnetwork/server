package work

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The drift model's weekly calibration (lag.dev/DRIFTTEST.md V3, V4, K2,
// Q12). Every Monday at 04:00 UTC it fits κ and σ₀ to the lag tracks stored
// within the lag retention, the last 7 days, and keeps the relay thresholds
// behind relay_suspected (model.CalibrateLagDrift). A calibration that
// applies is stored and in force from then on (model.GetLagDriftCalibration);
// one that does not changes nothing, and its result says why. Only aggregates
// are stored, and the result holds only counts and the fitted values, so
// nothing per visitor outlives the retention (R1).
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

// The most tracks one calibration reads, the newest first. 2,000 already fix
// κ to 1%, and each track is about 2,000 pings read from lag_result, so this
// bounds the job's read at a few hundred MB.
const lagDriftCalibrationMaxTracks = 10000

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

// The calibration as the clock reads `now`, over what the lag retention
// keeps.
func calibrateLagDrift(
	ctx context.Context,
	now time.Time,
	lagSettings *controller.LagSettings,
	settings *model.LagDriftCalibrationSettings,
	maxTracks int,
) *LagDriftCalibrationResult {
	windowStart := now.Add(-lagSettings.ResultRetention)
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
