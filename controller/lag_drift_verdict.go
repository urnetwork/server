package controller

import (
	"bufio"
	"context"
	"math"
	"os"
	"slices"
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

// The looks of the server's stop check: the frame fits after every ping
// round, 250 ms, from its first, and evaluates the rules from 3 s after the
// sessions open (V10).
const lagDriftLookMs = 250

// How many extenders with an estimate the frame's fit needs before it fits
// at all (lag.dev's check.ts): the server's history has a fit only where the
// frame's could.
const lagDriftLookExtenders = 6

// How much shorter than measure_ms a main session may have lasted, on its own
// extender's clock, and still have run to the cap (V10, review F-22): sessions
// to far extenders open up to a second or so late.
const lagDriftCapSessionSlackMs = 2000.0

// How much shorter than measure_ms the operator's clock may see a run, from
// its start to its finish, and still have run to the cap: an additional check
// on the reports' durations, never a substitute, since the client times its
// finish (V10, review F-22).
const lagDriftCapFinishSlackMs = 1000.0

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

// The relay threshold for a run (K2, Q12): the calibration's for the judged
// country's continent and the run's network type.
func lagDriftRelayThresholdMs(calibration *model.LagDriftCalibration, test *model.LagDriftTest, client *model.LagDriftTrackClient) float64 {
	return calibration.RelayThresholdMsFor(model.ContinentCodeForCountry(test.ClientIpCountryCode), lagDriftNetworkType(client))
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

// The stop the run's reports support, or "" for none (V10, E5): the frame's
// own rules on fits of the round trips back by each look, with the server's
// wider tolerance on the ellipse's area, against the judged country,
// client_ip's (K3). The history is rebuilt from the sessions' first round
// trips, as the frame keeps its fits from its first tick, and the last look
// is at the close the reports show, the run's last activity, so the round
// trips the frame counted are all in it. The frame's own stop is answered
// when the reports support it, else the first rule that holds.
//
// An interrupted run counts only if converged or clear held when its silence
// began, at its last pong (review F-02): otherwise a client could withhold its
// pongs right after a favourable look and end the run there. Through any
// silence the fit doesn't move, so the trailing 2 s look steadier than they
// were: the rules on the fit look no later than the frame's first look after
// the last pong. cap holds on the signed reports' own durations (review
// F-22), never on the finish's timing, which the client controls.
func lagDriftSupportedStop(
	test *model.LagDriftTest,
	run *model.LagDriftRun,
	timeline *lagDriftTimeline,
	cityPoint *lagDriftPoint,
	drift *LagDriftSettings,
	calibration *model.LagDriftCalibration,
) string {
	country := lagDriftBorders().country(test.ClientIpCountryCode)
	all := []lagdrift.Observation{}
	for _, session := range timeline.sessions {
		if observation, ok := lagDriftObservation(session, session.rttMs, run.Run); ok {
			all = append(all, observation)
		}
	}
	expected := lagDriftExpectedPoint(cityPoint, country, all)
	// a clear stop holds only at a k_early, which lagdrift requires (V11):
	// the textbook k's, until the run records its own
	tolerance := lagdrift.StopTolerance{
		AreaRatio: drift.StopAreaTolerance,
		KEarly:    lagdrift.KEarly(lagdrift.KForConfidence(test.Confidence)),
	}

	// whole ms, rounded up, so the pong at the close is in the look there
	closeMs := int64(math.Ceil(timeline.closeMs))
	lastPongMs := int64(-1)
	if 0 <= timeline.lastPongMs {
		lastPongMs = int64(math.Ceil(timeline.lastPongMs))
	}
	lookMs := closeMs
	if 0 <= lastPongMs {
		lookMs = min(closeMs, lastPongMs+lagDriftLookMs)
	}
	history := lagDriftHistory(run, timeline, calibration.Model(), expected, lookMs, closeMs)

	if lagDriftInterrupted(run, timeline, history, closeMs, lastPongMs, country, tolerance) {
		// it counts only if converged or clear held when the silence began
		if lagdrift.StopHolds(lagdrift.StopConverged, history, 0, lookMs, lastPongMs, country, tolerance) ||
			lagdrift.StopHolds(lagdrift.StopClear, history, 0, lookMs, lastPongMs, country, tolerance) {
			return string(lagdrift.StopInterrupted)
		}
		return ""
	}
	holds := func(stop lagdrift.StopReason) bool {
		switch stop {
		case lagdrift.StopClear, lagdrift.StopConverged:
			return lagdrift.StopHolds(stop, history, 0, lookMs, lastPongMs, country, tolerance)
		case lagdrift.StopCap:
			return lagDriftCapHolds(run, timeline, drift)
		default:
			return false
		}
	}
	if holds(lagdrift.StopReason(run.Stop)) {
		return run.Stop
	}
	for _, stop := range []lagdrift.StopReason{lagdrift.StopClear, lagdrift.StopConverged, lagdrift.StopCap} {
		if holds(stop) {
			return string(stop)
		}
	}
	return ""
}

// The frame's looks rebuilt from the reports (V10): one every 250 ms from the
// run's opening, from the first that has a round trip back, and one at each
// of the given moments, in order. Each fits the round trips back by then,
// from the look before, once the frame would fit: with lagDriftLookExtenders
// estimates. Every look measures from the same expected point, so the stop
// check can compare their positions.
func lagDriftHistory(
	run *model.LagDriftRun,
	timeline *lagDriftTimeline,
	driftModel lagdrift.Model,
	expected lagdrift.Point,
	atMs ...int64,
) []lagdrift.Snapshot {
	firstPongMs := math.Inf(1)
	for _, session := range timeline.sessions {
		for _, pongMs := range session.pongMs {
			firstPongMs = min(firstPongMs, pongMs)
		}
	}
	if math.IsInf(firstPongMs, 1) {
		return []lagdrift.Snapshot{}
	}
	lastMs := slices.Max(atMs)
	looks := slices.Clone(atMs)
	for lookMs := int64(math.Ceil(max(firstPongMs, 0)/lagDriftLookMs)) * lagDriftLookMs; lookMs <= lastMs; lookMs += lagDriftLookMs {
		looks = append(looks, lookMs)
	}
	slices.Sort(looks)
	looks = slices.Compact(looks)

	history := []lagdrift.Snapshot{}
	var warm *lagdrift.Fit
	for _, lookMs := range looks {
		observations := []lagdrift.Observation{}
		roundTrips := []int{}
		for _, session := range timeline.sessions {
			rttMs := session.roundTripsBy(float64(lookMs))
			roundTrips = append(roundTrips, len(rttMs))
			if observation, ok := lagDriftObservation(session, rttMs, run.Run); ok {
				observations = append(observations, observation)
			}
		}
		snapshot := lagdrift.Snapshot{AtMs: lookMs, RoundTrips: roundTrips}
		if lagDriftLookExtenders <= len(observations) {
			if fit, err := lagdrift.FitPosition(driftModel, expected, observations, warm); err == nil {
				snapshot.Fit = fit
				warm = fit
			}
		}
		history = append(history, snapshot)
	}
	return history
}

// Whether a run stopped as interrupted (V10): 4 s without a pong, before the
// cap could stop it. The reports show the silence when the sessions went on
// pinging into it, each unanswered ping lost after 5 s and the next sent then
// (M4). They show none when the frame closed them sooner, since an extender
// sends no ping while one is unanswered, so then the silence is the frame's
// finish on the operator's clock, for a frame that says it was interrupted.
// The reports' close is the silence's start only in that second case: a
// client that withholds its pongs and holds its sessions open moves the close
// 5 s past the last pong with each lost ping. So interrupted is decided
// before the other rules, which look no later than one look past the last
// pong: through a silence the fit doesn't move, its trailing 2 s look steady,
// and converged would hold at such a close on a look the client chose
// (TestLagDriftProcessInterrupted's third run).
func lagDriftInterrupted(
	run *model.LagDriftRun,
	timeline *lagDriftTimeline,
	history []lagdrift.Snapshot,
	closeMs int64,
	lastPongMs int64,
	country *lagdrift.Country,
	tolerance lagdrift.StopTolerance,
) bool {
	if lagdrift.StopCapMs < max(lastPongMs, 0)+lagdrift.StopInterruptedMs {
		// the cap comes first
		return false
	}
	if lagdrift.StopHolds(lagdrift.StopInterrupted, history, 0, closeMs, lastPongMs, country, tolerance) {
		return true
	}
	if operatorCloseMs, ok := lagDriftOperatorCloseMs(run, timeline); ok && run.Stop == string(lagdrift.StopInterrupted) {
		return lagdrift.StopHolds(lagdrift.StopInterrupted, history, 0, int64(operatorCloseMs), lastPongMs, country, tolerance)
	}
	return false
}

// Whether the signed reports show the run went to the cap (V10, review
// F-22): at least min_extenders main sessions each lasted at least
// measure_ms less 2 s, each from its own session start to its last round
// trip on its own extender's clock, so no clocks need aligning. A session
// lasts to its last round trip rather than its last ping: a client that
// withholds its pongs leaves the extender pinging, each ping lost after 5 s
// (M4), and so could keep a session going on with its fit frozen at a look
// it chose. The operator's clock is an additional check: a run whose finish
// came sooner than measure_ms less 1 s after its start didn't run to the cap.
// One with no finish was open on that clock until processing.
func lagDriftCapHolds(run *model.LagDriftRun, timeline *lagDriftTimeline, drift *LagDriftSettings) bool {
	lasted := 0
	for _, session := range timeline.sessions {
		if drift.Extenders <= session.report.Position {
			// a spare is no main session
			continue
		}
		// the session's times are from its own start (newLagDriftTimeline)
		if float64(drift.MeasureMs)-lagDriftCapSessionSlackMs <= session.lastPongMs() {
			lasted += 1
		}
	}
	if lasted < drift.MinExtenders {
		return false
	}
	if run.FinishedAt != nil &&
		float64(run.FinishedAt.Sub(run.StartedAt).Milliseconds()) < float64(drift.MeasureMs)-lagDriftCapFinishSlackMs {
		return false
	}
	return true
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
		// judged against client_ip's country, never a start address's (K3)
		Ip:          lagDriftIpOf(test, newest.IpFamily),
		Calibration: newLagDriftCalibrationView(calibration, lagDriftRelayThresholdMs(calibration, test, clients[newest.Run])),
	}
	var cityPoint *lagDriftPoint
	if client := clients[newest.Run]; client != nil && client.Latitude != nil && client.Longitude != nil {
		cityPoint = &lagDriftPoint{latitude: *client.Latitude, longitude: *client.Longitude}
	}

	observations := []lagdrift.Observation{}
	// bound while every run in the window matched client_ip in its family
	// (K3): a run from the other family counts unbound
	result.Bound = true
	for _, run := range window {
		result.RunsInVerdict = append(result.RunsInVerdict, run.Run)
		if run.ClientIpMatches == nil || !*run.ClientIpMatches {
			result.Bound = false
		}
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

	country := loaded.country(test.ClientIpCountryCode)
	fit, err := lagdrift.FitPosition(calibration.Model(), lagDriftExpectedPoint(cityPoint, country, observations), observations, nil)
	if err != nil {
		// a window run has the extenders to fit, so this is geometry that
		// places no one
		result.Reason = LagDriftReasonLowPrecision
		return result
	}

	// the textbook k at the token's confidence (V6), as before V11's
	// per-run k
	k := lagdrift.KForConfidence(test.Confidence)
	verdict, reason, judgement := lagdrift.Decide(fit, country, observations, k, drift.MaxSigmaKm)
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
				if calibration.RelaySuspected(lastMileMs, model.ContinentCodeForCountry(test.ClientIpCountryCode), lagDriftNetworkType(client)) {
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
		judgement = lagdrift.Judge(fit, country, k)
	}
	if country != nil {
		view.DistanceKm = lagDriftFigureKm(judgement.DistanceKm)
		view.SignificanceKm = lagDriftFigureKm(judgement.SignificanceKm)
		view.MarginKm = lagDriftMarginKm(judgement)
	}
	result.Drift = view
	return result
}

// A drift figure for P4, rounded to 0.1 km, and never -0. nil when not
// finite: lagdrift.Judge answers +Inf for a country without an outline.
func lagDriftFigureKm(valueKm float64) *float64 {
	if math.IsInf(valueKm, 0) || math.IsNaN(valueKm) {
		return nil
	}
	// adding 0 turns -0 into 0
	rounded := lagDriftRound(valueKm) + 0
	return &rounded
}

// The margin for P4 (review F-11), rounded as lagDriftFigureKm, and positive
// exactly when the whole region is outside the country (outside, D > k): such
// a margin reads at least 0.1 km rather than rounding to 0, and any other is
// at most 0.
func lagDriftMarginKm(judgement lagdrift.Judgement) *float64 {
	marginKm := lagDriftFigureKm(judgement.MarginKm)
	if marginKm == nil {
		return nil
	}
	if !judgement.Inside && judgement.K < judgement.D {
		*marginKm = max(*marginKm, 0.1)
	} else {
		*marginKm = min(*marginKm, 0) + 0
	}
	return marginKm
}
