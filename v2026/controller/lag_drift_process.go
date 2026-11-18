package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Processing a drift test's runs (lag.dev/DRIFTTEST.md E5, P6, V7, V10). A
// run is started at its start, processing from its finish, or
// processing_after without one, and complete once a report is in for every
// session it opened or report_wait after it began processing, whichever comes
// first. Completing a run first checks its stop against the signed reports
// (V10) and what it measured (E5, K3, K4), then recomputes the token's
// verdict from the pooled window (V7, V6) and stores it.
//
// Runs move on when anything looks at them: a report arriving (the report
// hook, lagDriftReportStored), the frame's finish and result, the customer's
// read, and the drift sweep for runs nobody reads. Each completion runs under
// the test's row lock, so concurrent ones take turns and none completes a
// run twice. A report arriving after its run completed is stored and changes
// nothing (E5).

// Moves a drift test's runs along at `now` and answers the test and its runs
// as they then stand.
func advanceLagDriftTest(
	ctx context.Context,
	test *model.LagDriftTest,
	now time.Time,
	settings *LagSettings,
) (*model.LagDriftTest, []*model.LagDriftRun) {
	runs := model.GetLagDriftRuns(ctx, test.DriftTestId)
	if !lagDriftRunsDue(ctx, runs, now, &settings.Drift) {
		return test, runs
	}
	completeLagDriftRuns(ctx, test.DriftTestId, now, settings)
	if advanced := model.GetLagDriftTest(ctx, test.DriftTestId); advanced != nil {
		test = advanced
	}
	return test, model.GetLagDriftRuns(ctx, test.DriftTestId)
}

// Moves the runs of a drift test along at `now`, for the sweep.
func AdvanceLagDriftTest(ctx context.Context, driftTestId server.Id, now time.Time, settings *LagSettings) {
	if test := model.GetLagDriftTest(ctx, driftTestId); test != nil {
		advanceLagDriftTest(ctx, test, now, settings)
	}
}

// The report hook (E5): a report of a drift run was stored. A processing run
// whose reports are now all in completes at once. Anything else waits: a run
// still measuring for its finish, and a complete one changes no more. A
// failure here is logged and never fails the extender's post, whose report is
// stored; the sweep completes the run later.
func lagDriftReportStored(ctx context.Context, trackId server.Id, now time.Time) {
	server.HandleError(func() {
		run := model.GetLagDriftRunByTrack(ctx, trackId)
		if run == nil || run.State != model.LagDriftRunStateProcessing {
			return
		}
		settings := GetLagSettings()
		if lagDriftReportsIn(run, model.GetLagDriftReportedPositions(ctx, trackId), &settings.Drift) {
			completeLagDriftRuns(ctx, run.DriftTestId, now, settings)
		}
	})
}

// The drift test's side of a report post (lag.dev/DRIFTTEST.md E4, review
// F-08). One operator client runs every extender, and report_limit counts its
// posts: a prompt post per drift session would spend it at about 60 runs an
// hour and then starve lag.dev's own reports. So a post isn't counted when
// every report in it verifies under one of the client's extenders and names a
// drift run's ticket issued to that extender for that track and position,
// and at least one of them is its ticket's first report, before the ticket
// expires: the tickets the server issued bound such posts. A repeat, for a
// ticket whose report is stored, is answered and dropped, and a post of
// repeats alone counts as every post did, so a looping extender can't
// re-post one old report outside the limit. So does a post with any other
// report, or a first report after its ticket expired.
type lagDriftReportPost struct {
	// whether report_limit doesn't count the post
	prompt bool
	// the reports read and verified, by index, so they are stored without
	// being read again
	read map[int]*lagReadReport
	// the repeats by index: their tickets have their reports
	repeats map[int]bool
}

// Reads a report post's drift reports (lagDriftReportPost), each verified
// once, up to the first report that isn't one.
func lagDriftReadReportPost(
	ctx context.Context,
	reporterClientId server.Id,
	reportTexts []string,
	now time.Time,
	extenderByKey func(publicKey []byte) *model.NetworkExtender,
) *lagDriftReportPost {
	post := &lagDriftReportPost{
		read:    map[int]*lagReadReport{},
		repeats: map[int]bool{},
	}
	fresh := false
	for index, reportText := range reportTexts {
		body, extender, reason := readLagReport(reporterClientId, reportText, extenderByKey)
		if reason != "" {
			return post
		}
		post.read[index] = &lagReadReport{body: body, extender: extender}
		ticketId, err := server.IdFromBytes(body.TicketId)
		if err != nil {
			return post
		}
		trackId, err := server.IdFromBytes(body.TrackId)
		if err != nil {
			return post
		}
		driftTicket := model.GetLagDriftReportTicket(ctx, ticketId)
		if driftTicket == nil ||
			driftTicket.Ticket.ExtenderId != extender.ExtenderId ||
			driftTicket.Ticket.TrackId != trackId ||
			uint32(driftTicket.Ticket.Position) != body.TrackPosition {
			return post
		}
		switch {
		case driftTicket.Reported:
			post.repeats[index] = true
		case !now.Before(driftTicket.Ticket.ExpiresAt):
			return post
		default:
			fresh = true
		}
	}
	post.prompt = fresh
	return post
}

// Whether any run is due at `now`: started past processing_after, or
// processing past its report wait or with every report in.
func lagDriftRunsDue(ctx context.Context, runs []*model.LagDriftRun, now time.Time, drift *LagDriftSettings) bool {
	for _, run := range runs {
		switch run.State {
		case model.LagDriftRunStateStarted:
			if !now.Before(run.StartedAt.Add(drift.ProcessingAfter)) {
				return true
			}
		case model.LagDriftRunStateProcessing:
			if run.FinishedAt == nil || !now.Before(run.FinishedAt.Add(drift.ReportWait())) {
				return true
			}
			if lagDriftReportsIn(run, model.GetLagDriftReportedPositions(ctx, run.TrackId), drift) {
				return true
			}
		}
	}
	return false
}

// Whether a report is in for every session the run opened (E5). The server
// knows the tickets, not which sessions the browser opened: a session that
// never opened never reports, and a spare opens only for one that failed
// (E2). So the run's reports are in when every one of its own positions has
// reported, or when as many positions have reported as it had extenders, a
// spare having stood in for a session that failed.
func lagDriftReportsIn(run *model.LagDriftRun, reported map[int]bool, drift *LagDriftSettings) bool {
	mainCount := 0
	covered := 0
	for _, extender := range run.Extenders {
		if extender.Position < drift.Extenders {
			mainCount += 1
			if reported[extender.Position] {
				covered += 1
			}
		}
	}
	return 0 < mainCount && (covered == mainCount || mainCount <= len(reported))
}

// One run's update, decided before anything is written.
type lagDriftRunUpdate struct {
	run          *model.LagDriftRun
	processingAt *time.Time
	completion   *model.LagDriftRunCompletion
}

// Completes the due runs of a drift test under its lock and, when one
// completed, recomputes and stores the verdict (E5, V7). Everything is read
// and computed before anything is written, so an internal error in the
// computation writes nothing; the test is then marked failed (P4) and the
// failure logged.
func completeLagDriftRuns(ctx context.Context, driftTestId server.Id, now time.Time, settings *LagSettings) {
	drift := &settings.Drift
	if lagDriftBorders() == nil {
		// no verdict without the borders (G1): the runs wait, open, for a
		// process that has them, and the missing file is logged once
		return
	}
	calibration := lagDriftCalibrationFor(ctx, drift)
	var failure error
	var networkId server.Id
	var completed []*lagDriftRunUpdate
	var verdict string
	var reason string
	server.Tx(ctx, func(tx server.PgTx) {
		// a rerun starts over
		failure = nil
		completed = nil
		verdict = ""
		reason = ""

		test := model.GetLagDriftTestForUpdateInTx(ctx, tx, driftTestId)
		if test == nil || test.FailedAt != nil {
			return
		}
		networkId = test.NetworkId
		runs := model.GetLagDriftRunsInTx(ctx, tx, driftTestId)
		reports := map[int][]*model.LagDriftReport{}
		runReports := func(run *model.LagDriftRun) []*model.LagDriftReport {
			if _, ok := reports[run.Run]; !ok {
				reports[run.Run] = model.GetLagDriftReportsInTx(ctx, tx, run.TrackId)
			}
			return reports[run.Run]
		}
		clients := map[int]*model.LagDriftTrackClient{}
		runClient := func(run *model.LagDriftRun) *model.LagDriftTrackClient {
			if _, ok := clients[run.Run]; !ok {
				clients[run.Run] = model.GetLagDriftTrackClientInTx(ctx, tx, run.TrackId)
			}
			return clients[run.Run]
		}

		updates := []*lagDriftRunUpdate{}
		for _, run := range runs {
			update := &lagDriftRunUpdate{run: run}
			if run.State == model.LagDriftRunStateStarted && !now.Before(run.StartedAt.Add(drift.ProcessingAfter)) {
				// no finish came: the run stopped measuring at the latest
				// processing_after after its start (P6)
				processingAt := run.StartedAt.Add(drift.ProcessingAfter)
				update.processingAt = &processingAt
				run.State = model.LagDriftRunStateProcessing
				run.FinishedAt = &processingAt
			}
			if run.State == model.LagDriftRunStateProcessing {
				reported := map[int]bool{}
				for _, report := range runReports(run) {
					reported[report.Position] = true
				}
				if !now.Before(run.FinishedAt.Add(drift.ReportWait())) || lagDriftReportsIn(run, reported, drift) {
					cityPoint := lagDriftCityPoint(runClient(run))
					var completion *model.LagDriftRunCompletion
					failure = lagDriftSafely(func() {
						completion = lagDriftCompleteRun(test, run, runReports(run), cityPoint, drift, calibration)
					})
					if failure != nil {
						failure = fmt.Errorf("run %d: %w", run.Run, failure)
						return
					}
					update.completion = completion
					run.State = model.LagDriftRunStateComplete
					run.CompletedAt = &now
					run.Outcome = completion.Outcome
					run.SupportedStop = completion.SupportedStop
					run.Answered = &completion.Answered
					run.Usable = &completion.Usable
					run.MeasuredMs = completion.MeasuredMs
				}
			}
			if update.processingAt != nil || update.completion != nil {
				updates = append(updates, update)
			}
		}

		var resultJson []byte
		if slices.ContainsFunc(updates, func(update *lagDriftRunUpdate) bool { return update.completion != nil }) {
			// the window's reports and client fields, read before the
			// verdict's computation
			for _, run := range lagDriftWindowRuns(runs, drift) {
				runReports(run)
				runClient(run)
			}
			var result *LagDriftResult
			failure = lagDriftSafely(func() {
				result = lagDriftWindowResult(test, runs, reports, clients, drift, calibration)
			})
			if failure != nil {
				failure = fmt.Errorf("verdict: %w", failure)
				return
			}
			var err error
			resultJson, err = json.Marshal(result)
			server.Raise(err)
			verdict = result.Verdict
			reason = result.Reason
		}

		for _, update := range updates {
			if update.processingAt != nil {
				model.SetLagDriftRunProcessingInTx(ctx, tx, driftTestId, update.run.Run, *update.processingAt)
			}
			if update.completion != nil {
				model.CompleteLagDriftRunInTx(ctx, tx, driftTestId, update.run.Run, update.completion, now)
				completed = append(completed, update)
			}
		}
		if resultJson != nil {
			model.SetLagDriftResultInTx(ctx, tx, driftTestId, resultJson, now)
		}
	})
	if failure != nil {
		// an internal error, which alerts (P4)
		glog.Errorf("[lag]drift test %s failed: %s\n", driftTestId, failure)
		model.MarkLagDriftTestFailed(ctx, driftTestId, now)
		countLagDriftFailure()
		return
	}
	for _, update := range completed {
		var finishToComplete *time.Duration
		if update.run.Stop != "" && update.run.FinishedAt != nil {
			// a run the frame finished
			duration := now.Sub(*update.run.FinishedAt)
			finishToComplete = &duration
		}
		countLagDriftRunCompleted(update.completion.Outcome, update.run.Stop, finishToComplete)
	}
	if verdict != "" {
		countLagDriftVerdict(networkId, verdict, reason)
	}
}

// The city point a run's track kept of its start address (A5), or nil.
func lagDriftCityPoint(client *model.LagDriftTrackClient) *lagDriftPoint {
	if client == nil || client.Latitude == nil || client.Longitude == nil {
		return nil
	}
	return &lagDriftPoint{latitude: *client.Latitude, longitude: *client.Longitude}
}

// Runs a computation, and answers a panic in it as an error.
func lagDriftSafely(compute func()) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	compute()
	return nil
}

// One session of a run on the run's timeline, in ms from the run's opening
// (V10): its round trips, each with the time its ping was sent and the time
// its pong arrived, when it last sent a ping, and where its extender was.
type lagDriftSession struct {
	report   *model.LagDriftReport
	extender *model.LagDriftRunExtender
	// the session's opening on the run's timeline (newLagDriftTimeline), so
	// its own durations are its times less this
	openMs float64
	sendMs []float64
	pongMs []float64
	rttMs  []float64
	// the last ping sent, lost or not; -1 for none
	lastSendMs float64
}

// The round trips the session had back by atMs.
func (self *lagDriftSession) roundTripsBy(atMs float64) []float64 {
	rtts := []float64{}
	for i, pongMs := range self.pongMs {
		if pongMs <= atMs {
			rtts = append(rtts, self.rttMs[i])
		}
	}
	return rtts
}

// The latest pong of the session, or -1 for none.
func (self *lagDriftSession) lastPongMs() float64 {
	last := -1.0
	for _, pongMs := range self.pongMs {
		last = max(last, pongMs)
	}
	return last
}

// The session's last activity: its last pong or the last ping it sent,
// whichever was later, or -1 for none.
func (self *lagDriftSession) lastActivityMs() float64 {
	return max(self.lastPongMs(), self.lastSendMs)
}

// A run's reports on one timeline (V10): ms from the sessions' opening.
type lagDriftTimeline struct {
	sessions []*lagDriftSession
	// the run's opening on the extenders' clocks, unix ms: its earliest
	// plausible session opening (newLagDriftTimeline)
	openedUnixMs float64
	// when the sessions closed, as the reports show it: the median of the
	// run's own sessions' last activity
	closeMs float64
	// the latest pong of any session, -1 for none
	lastPongMs float64
}

// Lays a run's reports on one timeline (V10), as the frame saw them: each
// session at its own opening, its pings at their send offsets from it. The
// frame opens the sessions together, but sessions to far extenders open
// seconds later (the e2e run of 2026-11-17: up to 3 s), and the frame's look
// counts their round trips that late. The run opens at its earliest
// plausible session opening, the nearest to the frame's own. Each report's
// times are its extender's clock (A7), so an opening further than
// lagDriftOpenSpreadMs from the median of the run's own sessions' openings
// is a clock off the others' (review F-14): such a session is laid at the
// run's opening. Each round trip's pong arrives its round trip after its ping
// was sent. A report whose position the run never ticketed is left out.
func newLagDriftTimeline(run *model.LagDriftRun, reports []*model.LagDriftReport, drift *LagDriftSettings) *lagDriftTimeline {
	extenders := map[int]*model.LagDriftRunExtender{}
	for _, extender := range run.Extenders {
		extenders[extender.Position] = extender
	}
	openings := []float64{}
	for _, report := range reports {
		if extenders[report.Position] != nil && report.Position < drift.Extenders {
			openings = append(openings, float64(report.SessionStartTime.UnixMilli()))
		}
	}
	if len(openings) == 0 {
		for _, report := range reports {
			openings = append(openings, float64(report.SessionStartTime.UnixMilli()))
		}
	}
	medianOpenMs := lagDriftMedian(openings)
	plausible := func(openingMs float64) bool {
		return math.Abs(openingMs-medianOpenMs) <= lagDriftOpenSpreadMs
	}
	runOpenMs := medianOpenMs
	for _, openingMs := range openings {
		if plausible(openingMs) {
			runOpenMs = min(runOpenMs, openingMs)
		}
	}

	timeline := &lagDriftTimeline{openedUnixMs: runOpenMs, lastPongMs: -1}
	closes := []float64{}
	for _, report := range reports {
		extender := extenders[report.Position]
		if extender == nil {
			continue
		}
		openMs := 0.0
		if openingMs := float64(report.SessionStartTime.UnixMilli()); plausible(openingMs) {
			openMs = openingMs - runOpenMs
		}
		windowMs := float64(report.WindowStartTime.Sub(report.SessionStartTime).Milliseconds())
		session := &lagDriftSession{report: report, extender: extender, openMs: openMs, lastSendMs: -1}
		for i, offsetMs := range report.PingOffsetMs {
			if len(report.PingMicros) <= i {
				break
			}
			sendMs := openMs + windowMs + float64(offsetMs)
			session.lastSendMs = max(session.lastSendMs, sendMs)
			micros := report.PingMicros[i]
			if micros <= 0 {
				// lost (A5)
				continue
			}
			rttMs := float64(micros) / 1000
			session.sendMs = append(session.sendMs, sendMs)
			session.rttMs = append(session.rttMs, rttMs)
			session.pongMs = append(session.pongMs, sendMs+rttMs)
		}
		timeline.sessions = append(timeline.sessions, session)
		timeline.lastPongMs = max(timeline.lastPongMs, session.lastPongMs())
		if lastActivityMs := session.lastActivityMs(); 0 <= lastActivityMs && report.Position < drift.Extenders {
			closes = append(closes, lastActivityMs)
		}
	}
	if len(closes) == 0 {
		for _, session := range timeline.sessions {
			closes = append(closes, max(session.lastActivityMs(), 0))
		}
	}
	timeline.closeMs = lagDriftMedian(closes)
	timeline.lastPongMs = min(timeline.lastPongMs, timeline.closeMs)
	return timeline
}

// The median of values, 0 for none.
func lagDriftMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}

// What processing makes of a run (E5, V10, K3, K4), in order: a run from
// another address than client_ip stays out (K3), as does one from client_ip's
// other family whose address's country isn't client_ip's (ip_family_mismatch,
// K3), one where fewer than min_extenders sessions reported at all
// (reports_missing), one whose sessions mostly took the start's other family
// (K4), one where at least min_extenders reported but fewer are usable
// (too_few_extenders, E5), and one the browser stopped before its reports
// support a stop (V10). Every other run counts.
func lagDriftCompleteRun(
	test *model.LagDriftTest,
	run *model.LagDriftRun,
	reports []*model.LagDriftReport,
	cityPoint *lagDriftPoint,
	drift *LagDriftSettings,
	calibration *model.LagDriftCalibration,
) *model.LagDriftRunCompletion {
	completion := &model.LagDriftRunCompletion{}
	timeline := newLagDriftTimeline(run, reports, drift)
	otherFamily := 0
	for _, session := range timeline.sessions {
		if 0 < len(session.rttMs) {
			completion.Answered += 1
		}
		if drift.UsableRoundTrips <= len(session.rttMs) {
			completion.Usable += 1
		}
		if session.report.IpFamily != run.IpFamily {
			otherFamily += 1
		}
	}
	if 0 < len(timeline.sessions) {
		measuredMs := int(math.Round(max(timeline.closeMs, 0)))
		completion.MeasuredMs = &measuredMs
	}

	switch {
	case run.ClientIpMatches != nil && !*run.ClientIpMatches:
		completion.Outcome = LagDriftRunOutcomeIpMismatch
	case run.ClientIpMatches == nil &&
		(run.IpCountryCode == "" || !strings.EqualFold(run.IpCountryCode, test.ClientIpCountryCode)):
		completion.Outcome = LagDriftRunOutcomeIpFamilyMismatch
	case len(timeline.sessions) < drift.MinExtenders:
		completion.Outcome = LagDriftRunOutcomeReportsMissing
	case len(timeline.sessions) < 2*otherFamily:
		completion.Outcome = LagDriftRunOutcomeIpFamilyMismatch
	case completion.Usable < drift.MinExtenders:
		completion.Outcome = LagDriftRunOutcomeTooFewExtenders
	default:
		completion.SupportedStop = lagDriftSupportedStop(test, run, timeline, cityPoint, drift, calibration)
		if completion.SupportedStop == "" {
			completion.Outcome = LagDriftRunOutcomeStoppedEarly
		} else {
			completion.Outcome = LagDriftRunOutcomeCounted
		}
	}
	return completion
}

// The address's side of a verdict (P4, K3): client_ip's country, which every
// run is judged against, and the family of the runs judged, client_ip's
// before any.
func lagDriftIpOf(test *model.LagDriftTest, family int) *LagDriftIp {
	if family == 0 {
		family = test.ClientIpFamily
	}
	return &LagDriftIp{
		CountryCode: test.ClientIpCountryCode,
		CountryName: test.ClientIpCountryName,
		Family:      family,
	}
}

// The verdict when no run of the window can be fitted (V6, step 1): no run in
// the window, because none matched client_ip or the newest had another reason
// to stay out of it.
func lagDriftNoWindowResult(test *model.LagDriftTest, runs []*model.LagDriftRun) *LagDriftResult {
	result := &LagDriftResult{
		Verdict:       LagDriftVerdictInconclusive,
		Reason:        LagDriftReasonTooFewExtenders,
		RunsInVerdict: []int{},
		Confidence:    test.Confidence,
		Ip:            lagDriftIpOf(test, 0),
	}
	mismatched := true
	var newest *model.LagDriftRun
	for _, run := range runs {
		if !run.Complete() {
			continue
		}
		newest = run
		if run.Outcome != LagDriftRunOutcomeIpMismatch {
			mismatched = false
		}
	}
	if newest == nil {
		return result
	}
	result.Ip = lagDriftIpOf(test, newest.IpFamily)
	switch {
	case mismatched:
		result.Reason = LagDriftReasonIpMismatch
	case newest.Outcome != LagDriftRunOutcomeCounted && newest.Outcome != LagDriftRunOutcomeIpMismatch && newest.Outcome != "":
		result.Reason = newest.Outcome
	default:
		// the newest that stayed out was a mismatch; one before it had its
		// own reason
		for i := len(runs) - 1; 0 <= i; i -= 1 {
			run := runs[i]
			if run.Complete() && run.Outcome != LagDriftRunOutcomeIpMismatch && run.Outcome != LagDriftRunOutcomeCounted {
				result.Reason = run.Outcome
				break
			}
		}
	}
	return result
}
