package controller

import (
	"bufio"
	"context"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect/lagdrift"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The drift test's verdict (lag.dev/DRIFTTEST.md V5-V7, V10, G1), from the
// authoritative model, github.com/urnetwork/connect/lagdrift (I3, V9). The
// server checks the browser's stop against the signed reports, then fits one
// position over the window's pooled evidence and judges it against the IP
// country's borders with lagdrift.Decide, the geometry the demo token's local
// verdict shares (W8).

// The country borders verdicts are judged against (G1): Natural Earth Admin 0
// at 1:10m, in lagdrift's binary form, under a stable name in the config
// beside lag.yml, with its release inside the file, which every verdict
// records as geometry_version (Borders.Version). Both processes that complete
// runs load it: the api, which completes them as their reports arrive and as
// they are read, and the taskworker, whose sweep completes the rest. A process
// without it completes none (completeLagDriftRuns), so a bad deploy shows as
// runs left processing (urnetwork_lag_drift_stuck_runs) rather than as
// verdicts judged against nothing.
const LagDriftBordersResource = "lag/ne-admin0-10m.bin"

// The looks of the server's stop check: the frame looks after every ping
// round from 3 s after the sessions open (V10).
const lagDriftLookMs = 250

// How far before the cap a run may close, as its reports see the close, and
// still have stopped at the cap: the reports see the close by the run's last
// activity, up to a ping interval and a round trip before the browser closed,
// and the browser counts the cap from its own first session's opening, a
// little before the run's opening here (the median of its sessions').
const lagDriftCapSlackMs = 1000.0

// How long after its start a run's sessions may open, as the operator's clock
// counts it, for the check of an interrupted run (lagDriftOperatorCloseMs).
const lagDriftMaxOpenDelayMs = 3000.0

// The borders as loaded.
type lagDriftLoadedBorders struct {
	borders *lagdrift.Borders
}

// The borders are read once per process, as the api starts serving drift
// tests. Without them every start is refused as unavailable (G1), and the
// first read logs the missing file once, as A3 does for the ASN database.
var lagDriftBordersOnce = sync.OnceValue(loadLagDriftBorders)

// A test's borders, in place of the resource's.
var lagDriftBordersOverride atomic.Pointer[lagDriftLoadedBorders]

func loadLagDriftBorders() *lagDriftLoadedBorders {
	path, err := server.Config.ResourcePath(LagDriftBordersResource)
	if err != nil {
		glog.Errorf("[lag]no drift test borders (%s): drift test runs are refused as unavailable (lag.dev/DRIFTTEST.md G1): %s\n", LagDriftBordersResource, err)
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		glog.Errorf("[lag]drift test borders %s are not readable: %s\n", path, err)
		return nil
	}
	defer file.Close()
	borders, err := lagdrift.ReadBorders(bufio.NewReader(file))
	if err != nil {
		glog.Errorf("[lag]drift test borders %s are not readable: %s\n", path, err)
		return nil
	}
	glog.Infof("[lag]drift test borders %s\n", borders.Version())
	return &lagDriftLoadedBorders{borders: borders}
}

// The borders, or nil when the deployment has none.
func lagDriftBorders() *lagDriftLoadedBorders {
	if loaded := lagDriftBordersOverride.Load(); loaded != nil {
		if loaded.borders == nil {
			return nil
		}
		return loaded
	}
	return lagDriftBordersOnce()
}

// Testing_SetLagDriftBorders judges drift tests against borders, or against
// none for nil, as a process without the file, until the returned restore
// runs.
func Testing_SetLagDriftBorders(borders *lagdrift.Borders) (restore func()) {
	previous := lagDriftBordersOverride.Swap(&lagDriftLoadedBorders{borders: borders})
	return func() {
		lagDriftBordersOverride.Store(previous)
	}
}

// The country of a code, or nil when it has none or there are no borders.
func (self *lagDriftLoadedBorders) country(code string) *lagdrift.Country {
	if self == nil || code == "" {
		return nil
	}
	return self.borders.Country(code)
}

// The version every verdict records (G1, P4), "" without borders.
func (self *lagDriftLoadedBorders) version() string {
	if self == nil {
		return ""
	}
	return self.borders.Version()
}

// The model the processor judges with (V4): the calibration in force, or the
// design's defaults while an operator has switched calibration off (lag.yml
// drift.calibration_enabled), whose verdicts then record no calibration. A
// withdrawal lasts only until the next weekly run; the switch lasts until it
// is set again.
func lagDriftCalibrationFor(ctx context.Context, drift *LagDriftSettings) *model.LagDriftCalibration {
	if !drift.CalibrationOn() {
		return model.DefaultLagDriftCalibration()
	}
	return model.GetLagDriftCalibration(ctx)
}

// The relay threshold for a run (K2, Q12): the calibration's for the IP
// country's continent and the run's network type.
func lagDriftRelayThresholdMs(calibration *model.LagDriftCalibration, run *model.LagDriftRun, client *model.LagDriftTrackClient) float64 {
	return calibration.RelayThresholdMsFor(model.ContinentCodeForCountry(run.IpCountryCode), lagDriftNetworkType(client))
}

func lagDriftNetworkType(client *model.LagDriftTrackClient) string {
	if client == nil {
		return model.LagDriftNetworkTypeUnknown
	}
	return model.LagDriftNetworkType(client.Asn, client.AsnOrg)
}

// The model a verdict was judged with, for its record (V4): the calibration's
// id and time, null for the defaults, κ, σ₀, and the relay threshold of the
// window's newest run.
func newLagDriftCalibrationView(calibration *model.LagDriftCalibration, relayThresholdMs float64) *LagDriftCalibrationView {
	return &LagDriftCalibrationView{
		CalibrationId:    calibration.CalibrationId,
		CalibratedAt:     lagDriftOptionalTime(calibration.CalibratedAt),
		Kappa:            calibration.Kappa,
		Sigma0Ms:         calibration.Sigma0Ms,
		RelayThresholdMs: relayThresholdMs,
	}
}

// One observation of a session's round trips (V1, V5), false with fewer than
// the estimate needs.
func lagDriftObservation(session *lagDriftSession, rttMs []float64, run int) (lagdrift.Observation, bool) {
	roundTrip, ok := lagdrift.EstimateRoundTrip(rttMs)
	if !ok {
		return lagdrift.Observation{}, false
	}
	return lagdrift.Observation{
		ExtenderId:   session.extender.ExtenderId.String(),
		At:           lagdrift.Point{Lat: session.extender.Latitude, Lon: session.extender.Longitude},
		PlaceErrorKm: session.extender.PlaceErrorKm,
		Run:          run,
		RoundTrip:    roundTrip,
	}, true
}

// The place a fit starts from and measures east and north from: the IP
// place's city point when the track kept one (A5), else the middle of the
// box around the IP country's largest outline, else the extender with the
// shortest round trip. One run's looks all measure from the same place, so
// the stop check can compare their positions (V10).
func lagDriftExpectedPoint(cityPoint *lagDriftPoint, country *lagdrift.Country, observations []lagdrift.Observation) lagdrift.Point {
	if cityPoint != nil {
		return lagdrift.Point{Lat: cityPoint.latitude, Lon: cityPoint.longitude}
	}
	if country != nil {
		var largest []lagdrift.Point
		for _, polygon := range country.Polygons {
			if 0 < len(polygon) && len(largest) < len(polygon[0]) {
				largest = polygon[0]
			}
		}
		if 0 < len(largest) {
			minLat, maxLat := largest[0].Lat, largest[0].Lat
			minLon, maxLon := largest[0].Lon, largest[0].Lon
			for _, point := range largest {
				minLat, maxLat = min(minLat, point.Lat), max(maxLat, point.Lat)
				minLon, maxLon = min(minLon, point.Lon), max(maxLon, point.Lon)
			}
			return lagdrift.Point{Lat: (minLat + maxLat) / 2, Lon: (minLon + maxLon) / 2}
		}
	}
	expected := lagdrift.Point{}
	shortest := math.Inf(1)
	for _, observation := range observations {
		if observation.RoundTrip.Ms < shortest {
			shortest = observation.RoundTrip.Ms
			expected = observation.At
		}
	}
	return expected
}

// The run's close on the operator's clock, ms from its opening: from its start
// to the frame's finish, less the time its sessions took to open. That is the
// sessions' median opening on the extenders' clocks after the start on the
// operator's, held to [0, 3 s], since the clocks may disagree.
func lagDriftOperatorCloseMs(run *model.LagDriftRun, timeline *lagDriftTimeline) (float64, bool) {
	if run.FinishedAt == nil || run.Stop == "" {
		return 0, false
	}
	openDelayMs := min(max(timeline.openedUnixMs-float64(run.StartedAt.UnixMilli()), 0), lagDriftMaxOpenDelayMs)
	return float64(run.FinishedAt.Sub(run.StartedAt).Milliseconds()) - openDelayMs, true
}

// The stop the run's reports support, or "" for none (V10): the frame's own
// rules, each look a fit of the round trips back by then, with the server's
// wider tolerance on the ellipse's area, at the close the reports show. That
// is the run's last activity, which a frame cannot delay to make its fit look
// steadier. A run that closed at the cap stopped there whatever the rules
// say. An interrupted run's silence shows in no report, since an extender
// sends no ping while one is unanswered (M4), so its close is the frame's
// finish on the operator's clock (lagDriftOperatorCloseMs).
func lagDriftSupportedStop(
	run *model.LagDriftRun,
	timeline *lagDriftTimeline,
	cityPoint *lagDriftPoint,
	drift *LagDriftSettings,
	calibration *model.LagDriftCalibration,
) string {
	country := lagDriftBorders().country(run.IpCountryCode)
	driftModel := calibration.Model()
	all := []lagdrift.Observation{}
	for _, session := range timeline.sessions {
		if observation, ok := lagDriftObservation(session, session.rttMs, run.Run); ok {
			all = append(all, observation)
		}
	}
	expected := lagDriftExpectedPoint(cityPoint, country, all)
	tolerance := lagdrift.StopTolerance{AreaRatio: drift.StopAreaTolerance}
	history := []lagdrift.Snapshot{}
	var warm *lagdrift.Fit
	for atMs := float64(lagdrift.StopEarliestMs); atMs <= timeline.closeMs; atMs += lagDriftLookMs {
		observations := []lagdrift.Observation{}
		roundTrips := []int{}
		for _, session := range timeline.sessions {
			rttMs := session.roundTripsBy(atMs)
			roundTrips = append(roundTrips, len(rttMs))
			if observation, ok := lagDriftObservation(session, rttMs, run.Run); ok {
				observations = append(observations, observation)
			}
		}
		snapshot := lagdrift.Snapshot{AtMs: int64(math.Floor(atMs)), RoundTrips: roundTrips}
		if fit, err := lagdrift.FitPosition(driftModel, expected, observations, warm); err == nil {
			snapshot.Fit = fit
			warm = fit
		}
		history = append(history, snapshot)
	}
	closeMs := int64(math.Floor(timeline.closeMs))
	lastPongMs := int64(math.Floor(timeline.lastPongMs))
	if stop, ok := lagdrift.EvaluateStop(history, 0, closeMs, lastPongMs, country, tolerance); ok {
		return string(stop)
	}
	if float64(lagdrift.StopCapMs)-lagDriftCapSlackMs <= timeline.closeMs {
		return string(lagdrift.StopCap)
	}
	if operatorCloseMs, ok := lagDriftOperatorCloseMs(run, timeline); ok && run.Stop == string(lagdrift.StopInterrupted) {
		if lagdrift.StopHolds(lagdrift.StopInterrupted, history, 0, int64(operatorCloseMs), lastPongMs, country, tolerance) {
			return string(lagdrift.StopInterrupted)
		}
	}
	return ""
}

// The drift country (V6, G3): the IP country when its own polygons hold the
// fit, since a disputed area belongs to every claimant and the borders name
// the first claimant by code; otherwise the country containing the fit, else
// the nearest within 300 km, else "" (at sea).
func lagDriftCountryOf(loaded *lagDriftLoadedBorders, ipCountry *lagdrift.Country, at lagdrift.Point) string {
	if ipCountry != nil && ipCountry.Contains(at) {
		return ipCountry.Code
	}
	if loaded == nil {
		return ""
	}
	return loaded.borders.CountryAt(at, lagdrift.DriftCountryWithinKm)
}

// The verdict over the window (V7, V6): one fit over the evidence of the last
// window_runs runs that count, one position and one last-mile offset per run,
// with each extender's route error shared by its runs. With no run in the
// window it is inconclusive for the reason the runs stayed out
// (lagDriftNoWindowResult); otherwise lagdrift.Decide judges the fit: an IP
// country without borders, an ellipse too wide to place anyone, and round
// trips physically impossible for any place the region allows in the country
// (V5) are inconclusive; agree when the fit is in the IP country or its region
// touches it, and disagree when the whole region is outside. The figures are
// toward the country's boundary point nearest in the region's own metric, and
// never the fit's coordinates (Q6).
func lagDriftWindowResult(
	test *model.LagDriftTest,
	runs []*model.LagDriftRun,
	reports map[int][]*model.LagDriftReport,
	clients map[int]*model.LagDriftTrackClient,
	drift *LagDriftSettings,
	calibration *model.LagDriftCalibration,
) *LagDriftResult {
	window := lagDriftWindowRuns(runs, drift)
	loaded := lagDriftBorders()
	if len(window) == 0 {
		result := lagDriftNoWindowResult(test, runs)
		result.GeometryVersion = loaded.version()
		result.Calibration = newLagDriftCalibrationView(calibration, calibration.RelayThresholdMs)
		return result
	}
	newest := window[len(window)-1]
	result := &LagDriftResult{
		Verdict:         LagDriftVerdictInconclusive,
		RunsInVerdict:   []int{},
		Confidence:      test.Confidence,
		GeometryVersion: loaded.version(),
		Ip: &LagDriftIp{
			CountryCode: newest.IpCountryCode,
			CountryName: newest.IpCountryName,
			Family:      newest.IpFamily,
		},
		Calibration: newLagDriftCalibrationView(calibration, lagDriftRelayThresholdMs(calibration, newest, clients[newest.Run])),
	}
	var cityPoint *lagDriftPoint
	if client := clients[newest.Run]; client != nil && client.Latitude != nil && client.Longitude != nil {
		cityPoint = &lagDriftPoint{latitude: *client.Latitude, longitude: *client.Longitude}
	}

	observations := []lagdrift.Observation{}
	for _, run := range window {
		result.RunsInVerdict = append(result.RunsInVerdict, run.Run)
		timeline := newLagDriftTimeline(run, reports[run.Run], drift)
		for _, session := range timeline.sessions {
			if len(session.rttMs) < drift.UsableRoundTrips {
				continue
			}
			if observation, ok := lagDriftObservation(session, session.rttMs, run.Run); ok {
				observations = append(observations, observation)
			}
		}
	}

	country := loaded.country(newest.IpCountryCode)
	fit, err := lagdrift.FitPosition(calibration.Model(), lagDriftExpectedPoint(cityPoint, country, observations), observations, nil)
	if err != nil {
		// a window run has the extenders to fit, so this is geometry that
		// places no one
		result.Reason = LagDriftReasonLowPrecision
		return result
	}

	verdict, reason, judgement := lagdrift.Decide(fit, country, observations, test.Confidence, drift.MaxSigmaKm)
	result.Verdict = string(verdict)
	result.Reason = string(reason)

	view := &LagDriftDrift{
		EllipseKm: &LagDriftEllipse{
			Major:      lagDriftRound(fit.MajorKm),
			Minor:      lagDriftRound(fit.MinorKm),
			BearingDeg: lagDriftRound(fit.BearingDeg),
		},
		LastMileMs:    []float64{},
		FitChi2PerDof: math.Round(fit.Chi2PerDof*100) / 100,
		Extenders:     fit.Extenders,
	}
	// b per run, in the order of fit.Runs, the window's ascending (V7)
	windowRuns := map[int]*model.LagDriftRun{}
	for _, run := range window {
		windowRuns[run.Run] = run
	}
	for i, lastMileMs := range fit.LastMileMs {
		view.LastMileMs = append(view.LastMileMs, lagDriftRound(lastMileMs))
		// advisory, against what is normal for the run's continent and
		// network type: the verdict is unchanged (K2, Q12)
		if i < len(fit.Runs) {
			if run := windowRuns[fit.Runs[i]]; run != nil {
				client := clients[run.Run]
				if calibration.RelaySuspected(lastMileMs, model.ContinentCodeForCountry(run.IpCountryCode), lagDriftNetworkType(client)) {
					view.RelaySuspected = true
				}
			}
		}
	}
	if driftCountryCode := lagDriftCountryOf(loaded, country, fit.At); driftCountryCode != "" {
		view.CountryCode = &driftCountryCode
	}
	if country != nil && reason == lagdrift.ReasonLowPrecision {
		// Decide judges no fit it can't trust; its figures still show how
		// far off and how wide it is
		judgement = lagdrift.Judge(fit, country, test.Confidence)
	}
	if country != nil {
		distanceKm := lagDriftRound(judgement.DistanceKm)
		significanceKm := lagDriftRound(judgement.SignificanceKm)
		marginKm := lagDriftRound(judgement.MarginKm)
		view.DistanceKm = &distanceKm
		view.SignificanceKm = &significanceKm
		view.MarginKm = &marginKm
	}
	result.Drift = view
	return result
}
