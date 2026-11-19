package controller

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/lagdrift"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Tests of processing (lag.dev/DRIFTTEST.md E5, V5-V7, V10, T4): reports
// posted as extenders post them (A7), the run's completion, the server's
// check of the stop, and the verdict over the pooled window.

// Where the synthetic visitors are.
var (
	lagDriftTestBerlin = lagdrift.Point{Lat: 52.52, Lon: 13.40}
	// 7 km inside the box's border with Poland: no early stop can be clear
	lagDriftTestBorderTown = lagdrift.Point{Lat: 52.35, Lon: 14.9}
	lagDriftTestMadrid     = lagdrift.Point{Lat: 40.42, Lon: -3.70}
	lagDriftTestParis      = lagdrift.Point{Lat: 48.86, Lon: 2.35}
	// beside the far band's nearest extender, which every first run has
	lagDriftTestCairo = lagdrift.Point{Lat: 30.06, Lon: 31.25}
)

// One run's synthetic sessions: a visitor's true place and last mile, how
// long the sessions pinged, and which positions report.
type lagDriftTestSessions struct {
	visitor    lagdrift.Point
	lastMileMs float64
	durationMs int
	// nil for the 12 own positions
	positions []int
	// the positions that pinged only this long, if any
	shortPositions  map[int]int
	ipFamily        uint32
	endReason       protocol.LagEndReason
	sessionStartsAt time.Time
	// the positions whose sessions opened this many ms late, as sessions to
	// far extenders do; the frame closes them with the rest all the same
	openDelays map[int]int
	// from this many ms after the sessions open no pong comes, if set: each
	// ping in the silence is lost after 5 s, and the next goes out then (M4)
	silentAfterMs int
	// when the frame finishes, ms after the sessions open, if not 300 ms
	// after it closes them
	finishAtMs int
	// from this many ms after the sessions open, if set, the paths to the
	// extenders east of the visitor are fasterEastMs faster, as when a route
	// changes mid-run
	fasterEastAfterMs int
	fasterEastMs      float64
	// the positions whose extenders' clocks are this many ms off the others',
	// which their reports' times carry (A7)
	clockSkews map[int]int
}

// Posts each reporting session's report of a started run, signed by its
// extender, as the extender's reporter does (M8, A7), at `now`.
func (self *lagDriftTestFixture) report(t testing.TB, start *LagDriftStartResult, sessions lagDriftTestSessions, now time.Time) {
	t.Helper()
	positions := sessions.positions
	if positions == nil {
		positions = []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	}
	for _, ticket := range start.Tickets {
		if !slices.Contains(positions, ticket.Position) {
			continue
		}
		extender := self.extenders[ticket.ExtenderId]
		result := postLagTestReports(t, extender, now, self.reportText(t, start, ticket, sessions))
		if result.Accepted != 1 {
			t.Fatalf("a report was refused: %+v", lagTestRejections(result))
		}
	}
}

// One session's report, signed by its extender. Each round trip is the
// visitor's last mile plus the path's propagation, off the great circle by a
// few percent per path, with a little jitter per ping. The session pings from
// its own opening until the frame closes it, durationMs after the sessions
// open.
func (self *lagDriftTestFixture) reportText(t testing.TB, start *LagDriftStartResult, ticket *LagDriftTicket, sessions lagDriftTestSessions) string {
	t.Helper()
	if sessions.ipFamily == 0 {
		sessions.ipFamily = 4
	}
	if sessions.endReason == protocol.LagEndReason_LAG_END_COMPLETE {
		sessions.endReason = protocol.LagEndReason_LAG_END_NO_LOAD
	}
	extender := self.extenders[ticket.ExtenderId]
	place := self.places[ticket.ExtenderId]
	ticketBody := verifyLagTestTicket(t, ticket.Ticket, self.rootPublicKey, start.expiresAtTime(t).Add(-time.Second))
	openDelayMs := sessions.openDelays[ticket.Position]
	opensAt := sessions.sessionStartsAt.Add(time.Duration(openDelayMs+sessions.clockSkews[ticket.Position]) * time.Millisecond)
	body := &protocol.LagReportBody{
		TicketId:           ticketBody.TicketId,
		ExtenderPublicKey:  extender.publicKey,
		TrackId:            ticketBody.TrackId,
		TrackPosition:      ticketBody.TrackPosition,
		IpFamily:           sessions.ipFamily,
		SessionStartTimeMs: uint64(opensAt.UnixMilli()),
		WindowStartTimeMs:  uint64(opensAt.UnixMilli()),
		IntervalMs:         250,
		EndReason:          sessions.endReason,
	}
	durationMs := sessions.durationMs
	if short, ok := sessions.shortPositions[ticket.Position]; ok {
		durationMs = short
	}
	distanceKm := lagdrift.DistanceKm(sessions.visitor, lagdrift.Point{Lat: place.Latitude, Lon: place.Longitude})
	detour := 1 + 0.02*float64(ticket.Position%5-2)
	for i, offsetMs := 0, 0; openDelayMs+offsetMs <= durationMs; i += 1 {
		body.PingOffsetMs = append(body.PingOffsetMs, uint32(offsetMs))
		if 0 < sessions.silentAfterMs && sessions.silentAfterMs < openDelayMs+offsetMs {
			body.PingMicros = append(body.PingMicros, 0)
			offsetMs += 5000
			continue
		}
		rttMs := sessions.lastMileMs + lagdrift.MsPerKm*distanceKm*detour + 0.25*float64(i%4)
		if 0 < sessions.fasterEastAfterMs && sessions.fasterEastAfterMs <= openDelayMs+offsetMs && sessions.visitor.Lon < place.Longitude {
			rttMs -= sessions.fasterEastMs
		}
		body.PingMicros = append(body.PingMicros, uint32(rttMs*1000))
		offsetMs += 250
	}
	return signLagTestReport(t, body, extender.privateKey)
}

func (self *LagDriftStartResult) expiresAtTime(t testing.TB) time.Time {
	t.Helper()
	expiresAt, err := time.Parse(time.RFC3339, self.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	return expiresAt
}

// Starts a run from the visitor's address, reports its sessions as given and
// finishes it with stop, the reports first; answers the run's start.
func (self *lagDriftTestFixture) runAt(
	t testing.TB,
	token string,
	clientAddress string,
	sessions lagDriftTestSessions,
	stop string,
	startsAt time.Time,
) *LagDriftStartResult {
	t.Helper()
	start := self.start(t, token, clientAddress, "", startsAt)
	if start.Error != nil {
		t.Fatalf("start refused: %+v", start.Error)
	}
	sessions.sessionStartsAt = startsAt.Add(300 * time.Millisecond)
	closesAt := sessions.sessionStartsAt.Add(time.Duration(sessions.durationMs) * time.Millisecond)
	finishesAt := closesAt.Add(300 * time.Millisecond)
	if 0 < sessions.finishAtMs {
		finishesAt = sessions.sessionStartsAt.Add(time.Duration(sessions.finishAtMs) * time.Millisecond)
	}
	self.report(t, start, sessions, closesAt.Add(200*time.Millisecond))
	self.finish(t, token, start.Run, stop, finishesAt)
	return start
}

// The stop processing found a run's reports support (V10), "" for none.
func (self *lagDriftTestFixture) supportedStop(t testing.TB, driftTestId server.Id, run int) string {
	t.Helper()
	for _, driftRun := range model.GetLagDriftRuns(self.ctx, driftTestId) {
		if driftRun.Run == run {
			return driftRun.SupportedStop
		}
	}
	t.Fatalf("no run %d", run)
	return ""
}

func (self *lagDriftTestFixture) runView(t testing.TB, driftTestId server.Id, run int, now time.Time) *LagDriftRunView {
	t.Helper()
	for _, runView := range self.read(t, driftTestId, now).Runs {
		if runView.Run == run {
			return runView
		}
	}
	t.Fatalf("no run %d", run)
	return nil
}

// A visitor whose pings place them where their address says: every report in
// completes the run at its finish, which counts and agrees, with the
// figures, the model it was judged with, and nothing of the fit's place. The
// widget reads only its fields. A relay's last mile is advisory.
func TestLagDriftProcessAgree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		start := fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
		}, LagDriftStopConverged, now)

		// complete at the finish, with every report in
		finishedAt := now.Add(5600 * time.Millisecond)
		view := fixture.read(t, allocated.DriftTestId, finishedAt)
		connect.AssertEqual(t, view.State, LagDriftStateActive)
		run := view.Runs[0]
		connect.AssertEqual(t, run.State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, *run.Stop, LagDriftStopConverged)
		connect.AssertEqual(t, *run.Answered, 12)
		connect.AssertEqual(t, run.InVerdict, true)
		if *run.MeasuredMs < 5000 || 5200 < *run.MeasuredMs {
			t.Fatalf("measured %d ms", *run.MeasuredMs)
		}
		connect.AssertNotEqual(t, model.GetLagDriftRuns(ctx, allocated.DriftTestId)[0].SupportedStop, "")

		result := view.Result
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, result.Reason, LagDriftReasonInsideIpCountry)
		connect.AssertEqual(t, result.RunsInVerdict, []int{1})
		connect.AssertEqual(t, result.PendingRun, (*int)(nil))
		connect.AssertEqual(t, result.Confidence, 0.99)
		connect.AssertEqual(t, result.GeometryVersion, "ne-admin0-10m-test")
		connect.AssertEqual(t, *result.Ip, LagDriftIp{CountryCode: "de", CountryName: "Germany", Family: 4})
		connect.AssertEqual(t, *result.Drift.CountryCode, "de")
		connect.AssertEqual(t, *result.Drift.DistanceKm, 0.0)
		if *result.Drift.MarginKm >= 0 {
			t.Fatalf("an agreeing margin of %f km", *result.Drift.MarginKm)
		}
		connect.AssertEqual(t, len(result.Drift.LastMileMs), 1)
		if result.Drift.LastMileMs[0] < 3 || 7 < result.Drift.LastMileMs[0] {
			t.Fatalf("last mile %v, want about 5 ms", result.Drift.LastMileMs)
		}
		connect.AssertEqual(t, result.Drift.RelaySuspected, false)
		connect.AssertEqual(t, result.Drift.Extenders, 12)
		// E2's ring trades a little precision at the IP place for reach
		// toward where a VPN's visitor may be (T3)
		if 150 < result.Drift.EllipseKm.Major {
			t.Fatalf("an ellipse of %+v", result.Drift.EllipseKm)
		}
		// the model it was judged with: the defaults, until a calibration
		connect.AssertEqual(t, *result.Calibration, LagDriftCalibrationView{Kappa: 0.25, Sigma0Ms: 1, RelayThresholdMs: 80})

		widget := fixture.widgetResult(t, allocated.Token, finishedAt)
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, widget.Run, 1)
		connect.AssertEqual(t, widget.RunState, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, widget.IpCountryName, "Germany")
		connect.AssertEqual(t, widget.RunsInVerdict, []int{1})
		connect.AssertEqual(t, widget.RetestAllowed, true)
		connect.AssertEqual(t, widget.RunsLeft, 5)

		// a late report, a spare's, changes nothing (E5)
		resultAt := model.GetLagDriftTest(ctx, allocated.DriftTestId).ResultAt
		fixture.report(t, start, lagDriftTestSessions{
			visitor:         lagDriftTestMadrid,
			lastMileMs:      5,
			durationMs:      5000,
			positions:       []int{12},
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}, finishedAt.Add(time.Second))
		connect.AssertEqual(t, model.GetLagDriftTest(ctx, allocated.DriftTestId).ResultAt.Equal(*resultAt), true)
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, finishedAt.Add(2*time.Second)).Result.Verdict, LagDriftVerdictAgree)

		// a last mile far above normal suspects a relay, and agrees still
		relayed := fixture.allocate(t, now, nil)
		fixture.runAt(t, relayed.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 120,
			durationMs: 5000,
		}, LagDriftStopConverged, now)
		relayedResult := fixture.read(t, relayed.DriftTestId, finishedAt).Result
		connect.AssertEqual(t, relayedResult.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, relayedResult.Drift.RelaySuspected, true)
	})
}

// A visitor whose pings place them far from their address's country: the
// whole region is outside it, so the verdict disagrees, with a positive
// margin, whichever order the finish and the reports come in. The visitor is
// beside one of the run's extenders: with route error a quarter of the
// distance (κ, V3), extenders all far from the visitor leave a region wide
// enough to reach the country, which agrees, as it should.
func TestLagDriftProcessDisagree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		// the finish first, then the reports: the last one completes the run
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		fixture.finish(t, allocated.Token, 1, LagDriftStopClear, now.Add(5300*time.Millisecond))
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(5400*time.Millisecond)).State, model.LagDriftRunStateProcessing)
		fixture.report(t, start, lagDriftTestSessions{
			visitor:         lagDriftTestCairo,
			lastMileMs:      5,
			durationMs:      5000,
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}, now.Add(5500*time.Millisecond))

		view := fixture.read(t, allocated.DriftTestId, now.Add(5600*time.Millisecond))
		connect.AssertEqual(t, view.Runs[0].State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		result := view.Result
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictDisagree)
		connect.AssertEqual(t, result.Reason, LagDriftReasonRegionOutside)
		if *result.Drift.MarginKm <= 0 || *result.Drift.DistanceKm < 500 {
			t.Fatalf("a disagreement %f km away with a margin of %f km", *result.Drift.DistanceKm, *result.Drift.MarginKm)
		}
		if result.Drift.CountryCode != nil && *result.Drift.CountryCode == "de" {
			t.Fatal("the pings place a visitor in Cairo in Germany")
		}
		widget := fixture.widgetResult(t, allocated.Token, now.Add(6*time.Second))
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictDisagree)
		connect.AssertEqual(t, widget.IpCountryName, "Germany")
	})
}

// A pooled verdict is judged at the largest k of its runs (V11), which P4
// reports, and a run's clear stop at its own k_early: one recorded too large
// for the visitor's distance confirms no clear stop. A run from before V11,
// with none recorded, is judged at the textbook k. Pure apart from the runs.
func TestLagDriftVerdictK(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)
		setRun := func(driftTestId server.Id, run int, column string, value float64) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`UPDATE lag_drift_run SET `+column+` = $3 WHERE drift_test_id = $1 AND run = $2`,
					driftTestId,
					run,
					value,
				))
			})
		}

		at := now
		for run := 1; run <= 2; run += 1 {
			fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
				visitor:    lagDriftTestBerlin,
				lastMileMs: 5,
				durationMs: 5000,
			}, LagDriftStopConverged, at)
			at = at.Add(10 * time.Second)
		}
		view := fixture.read(t, allocated.DriftTestId, at)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{1, 2})
		connect.AssertEqual(t, view.Result.K, lagdrift.DefaultK(0.99, "EU"))

		// a run started under a calibration with a larger k: the window's
		// next verdict takes the largest
		setRun(allocated.DriftTestId, 1, "k", 3.9)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
		}, LagDriftStopConverged, at)
		view = fixture.read(t, allocated.DriftTestId, at.Add(10*time.Second))
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{1, 2, 3})
		connect.AssertEqual(t, view.Result.K, 3.9)

		// a clear stop checked at the run's own k_early
		allocated = fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		setRun(allocated.DriftTestId, 1, "k_early", 1000)
		sessions := lagDriftTestSessions{
			visitor:         lagDriftTestCairo,
			lastMileMs:      5,
			durationMs:      3400,
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}
		fixture.report(t, start, sessions, now.Add(3900*time.Millisecond))
		fixture.finish(t, allocated.Token, start.Run, LagDriftStopClear, now.Add(4*time.Second))
		fixture.read(t, allocated.DriftTestId, now.Add(5*time.Second))
		connect.AssertNotEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopClear)

		// before V11, the textbook k
		test := &model.LagDriftTest{Confidence: 0.99}
		connect.AssertEqual(t, lagDriftRunK(test, &model.LagDriftRun{}), lagdrift.KForConfidence(0.99))
		connect.AssertEqual(t, lagDriftRunKEarly(test, &model.LagDriftRun{}), lagdrift.KEarly(lagdrift.KForConfidence(0.99)))
	})
}

// The server's check of the stop (V10, E5), with the e2e's runs
// (2026-11-17): a converged stop is supported by fits from the first look
// with 6 usable extenders, and a clear one at 3.4 s by the round trips back
// by the close itself. A run the browser stopped before the rules apply
// stays out of the window. The browser's own stop is the one recorded when the
// reports support it.
func TestLagDriftProcessStop(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		// a pass run, converged at 4.5 s: on the fits over usable
		// extenders, the first at about 1.8 s once each has 8 round trips,
		// the 2 s converged looks back on hold from about 4 s
		allocated := fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 4500,
		}, LagDriftStopConverged, now)
		view := fixture.read(t, allocated.DriftTestId, now.Add(6*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopConverged)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)

		// a VPN run, clear at 3.4 s: client_ip is in Germany, the visitor in
		// Cairo
		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestCairo,
			lastMileMs: 5,
			durationMs: 3400,
		}, LagDriftStopClear, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(5*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopClear)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictDisagree)

		// stopped at 2.8 s, before the rules are evaluated
		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 2800,
		}, LagDriftStopConverged, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(5*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, view.Runs[0].InVerdict, false)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), "")
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictInconclusive)
		connect.AssertEqual(t, view.Result.Reason, LagDriftReasonStoppedEarly)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{})
	})
}

// The e2e's converged run (2026-11-17), its own reports: a visitor in Austin
// whose sessions opened up to 3 s apart, as sessions to far extenders do, and
// whose frame stopped converged at about 5.75 s. Laid at their own openings,
// the reports support it at their close; laid each at its own start, the late
// openers' round trips came seconds early and the close 0.7 s before the
// frame's look, and they didn't.
func TestLagDriftStopE2eConverged(t *testing.T) {
	t.Cleanup(Testing_SetLagDriftBorders(lagDriftTestBorders(t)))
	fixtureJson, err := os.ReadFile("testdata/lag_drift_e2e_converged_run.json")
	if err != nil {
		t.Fatal(err)
	}
	var e2e struct {
		CityPoint struct {
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"city_point"`
		Stop      string                       `json:"stop"`
		Extenders []*model.LagDriftRunExtender `json:"extenders"`
		Reports   []struct {
			Position       int     `json:"position"`
			IpFamily       int     `json:"ip_family"`
			SessionStartMs int64   `json:"session_start_ms"`
			WindowStartMs  int64   `json:"window_start_ms"`
			PingOffsetMs   []int32 `json:"ping_offset_ms"`
			PingMicros     []int32 `json:"ping_micros"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(fixtureJson, &e2e); err != nil {
		t.Fatal(err)
	}
	run := &model.LagDriftRun{Run: 1, Stop: e2e.Stop, Extenders: e2e.Extenders}
	reports := []*model.LagDriftReport{}
	for _, report := range e2e.Reports {
		reports = append(reports, &model.LagDriftReport{
			Position:         report.Position,
			IpFamily:         report.IpFamily,
			SessionStartTime: time.UnixMilli(report.SessionStartMs),
			WindowStartTime:  time.UnixMilli(report.WindowStartMs),
			PingOffsetMs:     report.PingOffsetMs,
			PingMicros:       report.PingMicros,
		})
	}
	settings := DefaultLagSettings()
	timeline := newLagDriftTimeline(run, reports, &settings.Drift)
	if timeline.closeMs < 5600 || 5900 < timeline.closeMs {
		t.Fatalf("the reports close at %.0f ms, want near the frame's 5.75 s", timeline.closeMs)
	}
	austin := &lagDriftPoint{latitude: e2e.CityPoint.Latitude, longitude: e2e.CityPoint.Longitude}
	test := &model.LagDriftTest{ClientIpCountryCode: "us", Confidence: 0.99}
	connect.AssertEqual(t, lagDriftSupportedStop(test, run, timeline, austin, &settings.Drift, model.DefaultLagDriftCalibration()), LagDriftStopConverged)
}

// The last look is at the close itself (V10): the e2e's clear run at 3.4 s
// had its eighth usable extender's twelfth round trip back between the look
// before the close and the close, so a look before it saw only seven. Built
// on the timeline directly, where the pongs' times are exact.
func TestLagDriftStopLooksAtTheClose(t *testing.T) {
	t.Cleanup(Testing_SetLagDriftBorders(lagDriftTestBorders(t)))
	settings := DefaultLagSettings()
	drift := &settings.Drift
	// a visitor in Cairo, ticketed seven extenders around the eastern
	// Mediterranean that ping from their sessions' start, one in Cairo whose
	// pings start late enough that the round trip that makes it count, its
	// needed-th, is back at 3,155 ms, between the look at 2,950 ms and the
	// close at 3,200 ms, and four later still, short of it by the close; it
	// counts once usable and with lagdrift's stop count of round trips
	needed := max(drift.UsableRoundTrips, lagdrift.StopRoundTrips)
	places := []lagdrift.Point{
		{Lat: 37.98, Lon: 23.73}, {Lat: 41.01, Lon: 28.98}, {Lat: 32.09, Lon: 34.78}, {Lat: 24.71, Lon: 46.68},
		{Lat: 15.50, Lon: 32.56}, {Lat: 36.81, Lon: 10.18}, {Lat: 41.90, Lon: 12.50}, lagDriftTestCairo,
		{Lat: 48.86, Lon: 2.35}, {Lat: 52.52, Lon: 13.40}, {Lat: 25.20, Lon: 55.27}, {Lat: 6.52, Lon: 3.38},
	}
	run := &model.LagDriftRun{Run: 1, Stop: LagDriftStopClear}
	timeline := &lagDriftTimeline{lastPongMs: -1}
	for position, place := range places {
		extender := &model.LagDriftRunExtender{
			Position:     position,
			ExtenderId:   server.NewId(),
			Latitude:     place.Lat,
			Longitude:    place.Lon,
			PlaceErrorKm: 50,
		}
		run.Extenders = append(run.Extenders, extender)
		openMs := 0.0
		switch {
		case position == 7:
			openMs = float64(3150 - 250*(needed-1))
		case 7 < position:
			openMs = float64(3150 - 250*(needed-1) + 200)
		}
		rttMs := 5 + lagdrift.MsPerKm*lagdrift.DistanceKm(lagDriftTestCairo, place)
		session := &lagDriftSession{
			report:     &model.LagDriftReport{Position: position, IpFamily: 4},
			extender:   extender,
			openMs:     openMs,
			lastSendMs: -1,
		}
		for sendMs := openMs; sendMs+rttMs <= 3200; sendMs += 250 {
			session.sendMs = append(session.sendMs, sendMs)
			session.rttMs = append(session.rttMs, rttMs)
			session.pongMs = append(session.pongMs, sendMs+rttMs)
			session.lastSendMs = sendMs
		}
		timeline.sessions = append(timeline.sessions, session)
		timeline.lastPongMs = max(timeline.lastPongMs, session.lastPongMs())
	}
	timeline.closeMs = 3200
	test := &model.LagDriftTest{ClientIpCountryCode: "de", ClientIpFamily: 4, Confidence: 0.99}
	berlin := &lagDriftPoint{latitude: lagDriftTestBerlin.Lat, longitude: lagDriftTestBerlin.Lon}
	calibration := model.DefaultLagDriftCalibration()

	connect.AssertEqual(t, lagDriftSupportedStop(test, run, timeline, berlin, drift, calibration), LagDriftStopClear)

	// the Cairo session's needed-th pong a millisecond past the close:
	// seven count, and no rule holds
	connect.AssertEqual(t, len(timeline.sessions[7].pongMs), needed)
	timeline.sessions[7].pongMs[needed-1] = 3201
	connect.AssertEqual(t, lagDriftSupportedStop(test, run, timeline, berlin, drift, calibration), "")
}

// A stop is decided on the verdict's evidence, the extenders usable by each
// look (V10, E5), as the e2e's VPN run of 2026-11-17 showed: there the frame's
// clear look rested on far sessions that opened 2-3 s late with 4 round trips
// each, while the verdict, on the usable ones, was low_precision. A visitor in
// Poland 3 degrees east of the border, with the eight usable extenders all
// west of them in Germany and Czechia, and four to the east that opened
// 2.6 s late: on every estimate the region is clear of Germany, but on the
// usable ones it isn't, and the clear stop isn't supported.
func TestLagDriftStopUsableExtenders(t *testing.T) {
	t.Cleanup(Testing_SetLagDriftBorders(lagDriftTestBorders(t)))
	settings := DefaultLagSettings()
	visitor := lagdrift.Point{Lat: 52.4, Lon: 18.0}
	usable := []lagdrift.Point{
		{Lat: 52.52, Lon: 13.40}, {Lat: 51.34, Lon: 12.37}, {Lat: 53.55, Lon: 9.99}, {Lat: 51.05, Lon: 13.74},
		{Lat: 50.08, Lon: 14.44}, {Lat: 48.14, Lon: 11.58}, {Lat: 50.11, Lon: 8.68}, {Lat: 54.32, Lon: 10.12},
	}
	late := []lagdrift.Point{{Lat: 52.23, Lon: 21.01}, {Lat: 50.45, Lon: 30.52}, {Lat: 54.69, Lon: 25.28}, {Lat: 47.50, Lon: 19.04}}
	run := &model.LagDriftRun{Run: 1, Stop: LagDriftStopClear}
	timeline := &lagDriftTimeline{lastPongMs: -1, closeMs: 3400}
	for position, place := range append(slices.Clone(usable), late...) {
		extender := &model.LagDriftRunExtender{
			Position:     position,
			ExtenderId:   server.NewId(),
			Latitude:     place.Lat,
			Longitude:    place.Lon,
			PlaceErrorKm: 50,
		}
		run.Extenders = append(run.Extenders, extender)
		openMs := 0.0
		if len(usable) <= position {
			openMs = 2600
		}
		rttMs := 2 + lagdrift.MsPerKm*lagdrift.DistanceKm(visitor, place)
		session := &lagDriftSession{
			report:     &model.LagDriftReport{Position: position, IpFamily: 4},
			extender:   extender,
			openMs:     openMs,
			lastSendMs: -1,
		}
		for sendMs := openMs; sendMs+rttMs <= timeline.closeMs; sendMs += 250 {
			session.sendMs = append(session.sendMs, sendMs)
			session.rttMs = append(session.rttMs, rttMs)
			session.pongMs = append(session.pongMs, sendMs+rttMs)
			session.lastSendMs = sendMs
		}
		timeline.sessions = append(timeline.sessions, session)
		timeline.lastPongMs = max(timeline.lastPongMs, session.lastPongMs())
	}
	test := &model.LagDriftTest{ClientIpCountryCode: "de", ClientIpFamily: 4, Confidence: 0.99}
	calibration := model.DefaultLagDriftCalibration()

	connect.AssertEqual(t, lagDriftSupportedStop(test, run, timeline, nil, &settings.Drift, calibration), "")
	// on every estimate, as the frame looked before, it was clear
	everyEstimate := settings.Drift
	everyEstimate.UsableRoundTrips = 1
	connect.AssertEqual(t, lagDriftSupportedStop(test, run, timeline, nil, &everyEstimate, calibration), LagDriftStopClear)
}

// A session whose extender's clock is more than 5 s off the median of the
// run's openings is laid at the run's opening (V10, review F-14): five
// extenders whose clocks run 8 s ahead of the others' don't move their
// sessions 8 s late on the run's timeline, nor do clocks 8 s behind move them
// early, and a converged stop at 4.5 s holds.
func TestLagDriftProcessClockSkew(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		for _, skewMs := range []int{8000, -8000} {
			skews := map[int]int{}
			for position := 7; position < 12; position += 1 {
				skews[position] = skewMs
			}
			allocated := fixture.allocate(t, now, nil)
			fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
				visitor:    lagDriftTestBerlin,
				lastMileMs: 5,
				durationMs: 4500,
				clockSkews: skews,
			}, LagDriftStopConverged, now)
			view := fixture.read(t, allocated.DriftTestId, now.Add(6*time.Second))
			connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
			connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopConverged)
			connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)
		}
	})
}

// A drift's margin is positive exactly when the whole region is outside the
// country (P4, review F-11): one that would round to 0 there reads 0.1 km,
// and none reads -0. A figure that isn't finite is left out. Pure.
func TestLagDriftMarginRounding(t *testing.T) {
	outside := lagDriftMarginKm(lagdrift.Judgement{D: 3.04, K: 3.035, DistanceKm: 900, MarginKm: 0.02})
	connect.AssertEqual(t, *outside, 0.1)
	overlapping := lagDriftMarginKm(lagdrift.Judgement{Overlaps: true, D: 3.03, K: 3.035, DistanceKm: 900, MarginKm: -0.04})
	connect.AssertEqual(t, *overlapping, 0.0)
	connect.AssertEqual(t, math.Signbit(*overlapping), false)
	inside := lagDriftMarginKm(lagdrift.Judgement{Inside: true, Overlaps: true, D: 5, K: 3.035, MarginKm: -0.03})
	connect.AssertEqual(t, math.Signbit(*inside), false)
	marginJson, err := json.Marshal(inside)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(marginJson), "0")
	connect.AssertEqual(t, *lagDriftMarginKm(lagdrift.Judgement{D: 10, K: 3.035, DistanceKm: 1300, MarginKm: 912.34}), 912.3)
	connect.AssertEqual(t, *lagDriftMarginKm(lagdrift.Judgement{Inside: true, Overlaps: true, D: 1, K: 3.035, MarginKm: -140.06}), -140.1)
	connect.AssertEqual(t, lagDriftMarginKm(lagdrift.Judgement{D: math.Inf(1), K: 3.035, DistanceKm: math.Inf(1), MarginKm: math.Inf(1)}), (*float64)(nil))
	connect.AssertEqual(t, lagDriftFigureKm(math.Inf(1)), (*float64)(nil))
	connect.AssertEqual(t, math.Signbit(*lagDriftFigureKm(-0.04)), false)
}

// cap holds on the signed reports' own durations (V10, review F-22): an
// honest capped run passes though its far sessions opened late, as the e2e's
// run from Frankfurt did, while a client that cuts its sessions short at a
// look it likes fails, whether it keeps one alive to 11 s or finishes at
// 11 s.
func TestLagDriftProcessCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		// seven of the twelve sessions open 1.1 s late, so they last 10.9 s
		// on their own clocks
		late := map[int]int{5: 1100, 6: 1100, 7: 1100, 8: 1100, 9: 1100, 10: 1100, 11: 1100}
		allocated := fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 12000,
			openDelays: late,
		}, LagDriftStopCap, now)
		view := fixture.read(t, allocated.DriftTestId, now.Add(13*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopCap)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)

		// five sessions open 2.5 s late, so at 4 s only seven extenders are
		// usable and no rule on the fit holds: a cut there is a look the
		// client chose
		late = map[int]int{7: 2500, 8: 2500, 9: 2500, 10: 2500, 11: 2500}
		cut := map[int]int{}
		for position := 1; position < 12; position += 1 {
			cut[position] = 4000
		}
		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:        lagDriftTestBerlin,
			lastMileMs:     5,
			durationMs:     11000,
			openDelays:     late,
			shortPositions: cut,
		}, LagDriftStopCap, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(13*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), "")

		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 4000,
			openDelays: late,
			finishAtMs: 11000,
		}, LagDriftStopCap, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(13*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), "")
	})
}

// An interrupted run counts only if converged or clear held when its silence
// began (V10, review F-02). A run that went quiet once its fit had settled
// counts. One that went quiet before any rule held stays out, as does one
// whose sessions went on pinging into the silence: there the frozen fit's
// trailing 2 s look steady, and converged would hold at the close.
func TestLagDriftProcessInterrupted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)

		// quiet from 6 s, the fit settled; the frame finishes 4.5 s later
		allocated := fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:       lagDriftTestBerlin,
			lastMileMs:    5,
			durationMs:    10500,
			silentAfterMs: 6000,
		}, LagDriftStopInterrupted, now)
		view := fixture.read(t, allocated.DriftTestId, now.Add(12*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), LagDriftStopInterrupted)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)

		// quiet from 3.5 s, when the five sessions that opened 2.5 s late
		// were too new to be usable, so no rule held
		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:       lagDriftTestBorderTown,
			lastMileMs:    5,
			durationMs:    7600,
			silentAfterMs: 3500,
			openDelays:    map[int]int{7: 2500, 8: 2500, 9: 2500, 10: 2500, 11: 2500},
		}, LagDriftStopInterrupted, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(12*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), "")

		// quiet from 3.5 s, while the fit was still moving east after the
		// paths there got faster at 3 s, and pinging on into the silence
		// until 9 s: by then the fit had been frozen for 5 s
		allocated = fixture.allocate(t, now, nil)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:           lagDriftTestBorderTown,
			lastMileMs:        5,
			durationMs:        9000,
			silentAfterMs:     3500,
			fasterEastAfterMs: 3000,
			fasterEastMs:      4,
		}, LagDriftStopConverged, now)
		view = fixture.read(t, allocated.DriftTestId, now.Add(12*time.Second))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeStoppedEarly)
		connect.AssertEqual(t, fixture.supportedStop(t, allocated.DriftTestId, 1), "")
	})
}

// Runs that stay out of the window (E5, K3, K4): fewer than 6 sessions
// reporting by the report wait is reports_missing, at least 6 reporting with
// fewer than 6 usable is too_few_extenders, sessions mostly on the start's
// other family is ip_family_mismatch, and a start from another address than
// client_ip is ip_mismatch. A run with no finish stops measuring after
// processing_after and completes after the report wait.
func TestLagDriftProcessExcluded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		reportWait := fixture.settings.Drift.ReportWait()
		outcome := func(clientAddress string, sessions lagDriftTestSessions) (*LagDriftRunView, *LagDriftResult) {
			allocated := fixture.allocate(t, now, nil)
			fixture.runAt(t, allocated.Token, clientAddress, sessions, LagDriftStopConverged, now)
			// partial reports wait for the report wait
			readAt := now.Add(6*time.Second + reportWait)
			view := fixture.read(t, allocated.DriftTestId, readAt)
			return view.Runs[0], view.Result
		}

		run, result := outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, positions: []int{0, 1, 2, 3, 4},
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeReportsMissing)
		connect.AssertEqual(t, result.Reason, LagDriftReasonReportsMissing)
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictInconclusive)

		run, result = outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000,
			// 7 of 12 pinged 1.5 s, under the 8 round trips that make one
			// usable
			shortPositions: map[int]int{0: 1500, 1: 1500, 2: 1500, 3: 1500, 4: 1500, 5: 1500, 6: 1500},
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeTooFewExtenders)
		connect.AssertEqual(t, result.Reason, LagDriftReasonTooFewExtenders)

		run, result = outcome(lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, ipFamily: 6,
		})
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeIpFamilyMismatch)
		connect.AssertEqual(t, result.Reason, LagDriftReasonIpFamilyMismatch)

		run, result = outcome(lagDriftTestNeighborAddress, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000,
		})
		connect.AssertEqual(t, *run.ClientIpMatches, false)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeIpMismatch)
		connect.AssertEqual(t, result.Reason, LagDriftReasonIpMismatch)

		// the other family counts, and the customer sees null (K3)
		run, result = outcome(lagDriftTestVisitorIpv6, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, ipFamily: 6,
		})
		connect.AssertEqual(t, run.ClientIpMatches, (*bool)(nil))
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, result.Ip.Family, 6)

		// no finish and no report
		allocated := fixture.allocate(t, now, nil)
		fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(29*time.Second)).State, model.LagDriftRunStateStarted)
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, now.Add(30*time.Second)).State, model.LagDriftRunStateProcessing)
		run = fixture.runView(t, allocated.DriftTestId, 1, now.Add(30*time.Second+reportWait))
		connect.AssertEqual(t, run.State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, run.Outcome, LagDriftRunOutcomeReportsMissing)
	})
}

// Every run is judged against client_ip's own country, looked up at
// allocation and stored with the token (K3, review F-10). An IPv4-only VPN in
// Germany on the customer's site, with the visitor's IPv6 reaching lag.dev
// from France, is never judged against France: the run from the other family
// and another country stays out as ip_family_mismatch. An other-family run
// from client_ip's own country counts, and leaves the verdict unbound. The
// window pools only the runs of its latest counting run's family.
func TestLagDriftProcessClientIpCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)
		test := model.GetLagDriftTest(ctx, allocated.DriftTestId)
		connect.AssertEqual(t, test.ClientIpCountryCode, "de")
		connect.AssertEqual(t, test.ClientIpCountryName, "Germany")
		germany := func(family int) LagDriftIp {
			return LagDriftIp{CountryCode: "de", CountryName: "Germany", Family: family}
		}

		// the VPN: the start over IPv6 from France, the visitor in Paris
		at := now
		fixture.runAt(t, allocated.Token, lagDriftTestLyonIpv6, lagDriftTestSessions{
			visitor:    lagDriftTestParis,
			lastMileMs: 5,
			durationMs: 5000,
			ipFamily:   6,
		}, LagDriftStopConverged, at)
		view := fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, view.Runs[0].ClientIpMatches, (*bool)(nil))
		connect.AssertEqual(t, view.Runs[0].Outcome, LagDriftRunOutcomeIpFamilyMismatch)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictInconclusive)
		connect.AssertEqual(t, view.Result.Reason, LagDriftReasonIpFamilyMismatch)
		connect.AssertEqual(t, view.Result.Bound, false)
		connect.AssertEqual(t, *view.Result.Ip, germany(6))

		// IPv6 from Germany counts, unbound
		at = at.Add(10 * time.Second)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorIpv6, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
			ipFamily:   6,
		}, LagDriftStopConverged, at)
		view = fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, view.Runs[1].Outcome, LagDriftRunOutcomeCounted)
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2})
		connect.AssertEqual(t, view.Result.Bound, false)
		connect.AssertEqual(t, *view.Result.Ip, germany(6))

		// IPv4 from client_ip: the window is its family's runs alone, bound
		at = at.Add(10 * time.Second)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
		}, LagDriftStopConverged, at)
		view = fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{3})
		connect.AssertEqual(t, view.Result.Bound, true)
		connect.AssertEqual(t, *view.Result.Ip, germany(4))

		// and IPv6 again: the window is the IPv6 runs
		at = at.Add(10 * time.Second)
		fixture.runAt(t, allocated.Token, lagDriftTestVisitorIpv6, lagDriftTestSessions{
			visitor:    lagDriftTestBerlin,
			lastMileMs: 5,
			durationMs: 5000,
			ipFamily:   6,
		}, LagDriftStopConverged, at)
		view = fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 4})
		connect.AssertEqual(t, view.Result.Bound, false)
		connect.AssertEqual(t, *view.Result.Ip, germany(6))

		// the widget names client_ip's country
		connect.AssertEqual(t, fixture.widgetResult(t, allocated.Token, at.Add(6*time.Second)).IpCountryName, "Germany")
	})
}

// The verdict pools the last 3 runs that count (V7): a fourth drops the
// oldest, b is per run, a run the visitor just finished is pending until its
// reports are in, and a run that doesn't count leaves the window as it was.
func TestLagDriftProcessWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)

		at := now
		for run := 1; run <= 4; run += 1 {
			start := fixture.runAt(t, allocated.Token, lagDriftTestVisitorAddress, lagDriftTestSessions{
				visitor:    lagDriftTestBerlin,
				lastMileMs: float64(4 + run),
				durationMs: 5000,
			}, LagDriftStopConverged, at)
			connect.AssertEqual(t, start.Run, run)
			at = at.Add(10 * time.Second)
		}
		view := fixture.read(t, allocated.DriftTestId, at)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, len(view.Result.Drift.LastMileMs), 3)
		connect.AssertEqual(t, view.Runs[0].InVerdict, false)
		connect.AssertEqual(t, view.Runs[3].InVerdict, true)
		// a retest's paths are new where the bands have others (E2): 4 runs
		// of 12 among 21 extenders repeat some
		if view.Result.Drift.Extenders <= 12 {
			t.Fatalf("the window fits %d distinct extenders", view.Result.Drift.Extenders)
		}

		// run 5 finished, its reports not yet in: pending
		fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", at)
		fixture.finish(t, allocated.Token, 5, LagDriftStopConverged, at.Add(5*time.Second))
		view = fixture.read(t, allocated.DriftTestId, at.Add(6*time.Second))
		connect.AssertEqual(t, *view.Result.PendingRun, 5)
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, view.State, LagDriftStateActive)
		widget := fixture.widgetResult(t, allocated.Token, at.Add(6*time.Second))
		connect.AssertEqual(t, widget.Run, 5)
		connect.AssertEqual(t, widget.RunState, model.LagDriftRunStateProcessing)
		connect.AssertEqual(t, *widget.Verdict, LagDriftVerdictAgree)

		// it never reports: it completes after the report wait and stays out
		view = fixture.read(t, allocated.DriftTestId, at.Add(5*time.Second+fixture.settings.Drift.ReportWait()))
		connect.AssertEqual(t, view.Runs[4].Outcome, LagDriftRunOutcomeReportsMissing)
		connect.AssertEqual(t, view.Result.PendingRun, (*int)(nil))
		connect.AssertEqual(t, view.Result.RunsInVerdict, []int{2, 3, 4})
		connect.AssertEqual(t, view.Result.Verdict, LagDriftVerdictAgree)

		// after the retest window, closed
		connect.AssertEqual(t, fixture.read(t, allocated.DriftTestId, now.Add(15*time.Minute)).State, LagDriftStateClosed)
		closed := fixture.widgetResult(t, allocated.Token, now.Add(15*time.Minute))
		connect.AssertEqual(t, closed.RetestAllowed, false)
		connect.AssertEqual(t, closed.RunsLeft, 0)
	})
}

// A report of a demo session, which an extender that predates NoReport still
// sends (E4, W8), is dropped without a row and counted.
func TestLagDriftDemoReportDropped(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		start := fixture.start(t, LagDriftDemoToken, lagDriftTestVisitorAddress, "", now)
		connect.AssertEqual(t, start.Error, (*LagError)(nil))
		before := lagTestTableText(t, ctx, "lag_result")

		ticket := start.Tickets[0]
		extender := fixture.extenders[ticket.ExtenderId]
		ticketBody := verifyLagTestTicket(t, ticket.Ticket, fixture.rootPublicKey, now)
		connect.AssertEqual(t, ticketBody.NoReport, true)
		body := &protocol.LagReportBody{
			TicketId:           ticketBody.TicketId,
			ExtenderPublicKey:  extender.publicKey,
			TrackId:            ticketBody.TrackId,
			TrackPosition:      ticketBody.TrackPosition,
			IpFamily:           4,
			SessionStartTimeMs: uint64(now.UnixMilli()),
			WindowStartTimeMs:  uint64(now.UnixMilli()),
			IntervalMs:         250,
			PingOffsetMs:       []uint32{0, 250, 500, 750},
			PingMicros:         []uint32{9000, 9100, 9200, 9300},
			EndReason:          protocol.LagEndReason_LAG_END_NO_LOAD,
		}
		result := postLagTestReports(t, extender, now.Add(2*time.Second), signLagTestReport(t, body, extender.privateKey))
		assertLagReportRejected(t, result, LagReportRejectTicket)
		connect.AssertEqual(t, lagTestTableText(t, ctx, "lag_result"), before)
		connect.AssertEqual(t, lagDriftDemoReport(server.RequireIdFromBytes(ticketBody.TicketId), server.NewId()), false)
	})
}

// A drift test's prompt reports, a post per session, aren't counted against
// report_limit (E4, review F-08), so 100 runs leave a lag.dev report its
// place under the limit. Each extender here has its own client, which posts
// its share of the 1,200 prompt reports, over the limit of 40 set for the
// test, as the operator's one client posts all of them in production, over
// its 720. A replayed drift report is answered and dropped. A post of
// repeats alone counts, so past the limit it is refused, while one with a
// fresh report beside a repeat is not. lag.dev's own posts count as before,
// and so does a drift report that comes after its ticket expired.
func TestLagDriftPromptReports(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		settings := *fixture.settings
		settings.ReportLimit = 40
		post := func(extender *lagTestExtender, at time.Time, reports ...string) (*ExtenderLagReportResult, error) {
			return extenderLagReport(&ExtenderLagReportArgs{Reports: reports}, extender.clientSession, at, &settings)
		}
		// spends an extender client's limit with posts of lag.dev's own
		spend := func(extender *lagTestExtender) {
			for range settings.ReportLimit + 1 {
				if _, err := post(extender, now, "junk"); err != nil {
					if !strings.HasPrefix(err.Error(), "429 ") {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatalf("no 429 after %d posts", settings.ReportLimit+1)
		}
		sessions := lagDriftTestSessions{
			visitor:         lagDriftTestBerlin,
			lastMileMs:      5,
			durationMs:      3400,
			sessionStartsAt: now.Add(300 * time.Millisecond),
		}

		posts := map[server.Id]int{}
		lastReports := map[server.Id]string{}
		for range 100 {
			allocated := fixture.allocate(t, now, nil)
			start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
			if start.Error != nil {
				t.Fatalf("start refused: %+v", start.Error)
			}
			for _, ticket := range start.Tickets {
				if fixture.settings.Drift.Extenders <= ticket.Position {
					continue
				}
				extender := fixture.extenders[ticket.ExtenderId]
				report := fixture.reportText(t, start, ticket, sessions)
				result, err := post(extender, now.Add(4*time.Second), report)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, result.Accepted, 1)
				posts[extender.extenderId] += 1
				lastReports[extender.extenderId] = report
			}
		}
		var busiest *lagTestExtender
		for extenderId, count := range posts {
			if busiest == nil || posts[busiest.extenderId] < count {
				busiest = fixture.extenders[extenderId]
			}
		}
		connect.AssertEqual(t, settings.ReportLimit < posts[busiest.extenderId], true)

		// lag.dev's own report from the busiest client goes in
		_, tickets := newLagTestTrack(ctx, now.Add(-2*time.Minute), busiest)
		lagReport := signLagTestReport(t, newLagTestReportBody(busiest, tickets[0], now.Add(-time.Minute)), busiest.privateKey)
		result, err := post(busiest, now.Add(5*time.Second), lagReport)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)

		// a replayed drift report is answered and dropped
		result, err = post(busiest, now.Add(6*time.Second), lastReports[busiest.extenderId])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 0)
		assertLagReportRejected(t, result, LagReportRejectDuplicate)

		// lag.dev's posts and posts of repeats alone count: past the limit,
		// a post of a repeat alone is refused
		spend(busiest)
		_, err = post(busiest, now.Add(6*time.Second), lastReports[busiest.extenderId])
		if err == nil || !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf("a post of a repeat alone past the limit answered %v, want a 429", err)
		}

		// a first report goes in past a spent limit before its ticket
		// expires, a repeat beside it dropped, and counts after
		allocated := fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)
		var ticket *LagDriftTicket
		for _, candidate := range start.Tickets {
			if candidate.Position < fixture.settings.Drift.Extenders && lastReports[candidate.ExtenderId] != "" {
				ticket = candidate
				break
			}
		}
		if ticket == nil {
			t.Fatal("no ticket of an extender that has reported")
		}
		extender := fixture.extenders[ticket.ExtenderId]
		report := fixture.reportText(t, start, ticket, sessions)
		spend(extender)
		_, err = post(extender, start.expiresAtTime(t).Add(time.Second), report)
		if err == nil || !strings.HasPrefix(err.Error(), "429 ") {
			t.Fatalf("a drift report after its ticket expired answered %v, want a 429", err)
		}
		result, err = post(extender, now.Add(4*time.Second), report, lastReports[extender.extenderId])
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, lagTestRejections(result), []ExtenderLagReportRejection{
			{Index: 1, Reason: LagReportRejectDuplicate},
		})
	})
}

// A process without the borders completes no run (G1): the runs wait, open,
// for one that has them, rather than complete as judged against nothing.
func TestLagDriftProcessWithoutBorders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newLagDriftTestFixture(t, ctx)
		now := server.NowUtc().Truncate(time.Millisecond)
		allocated := fixture.allocate(t, now, nil)
		start := fixture.start(t, allocated.Token, lagDriftTestVisitorAddress, "", now)

		restore := Testing_SetLagDriftBorders(nil)
		fixture.report(t, start, lagDriftTestSessions{
			visitor: lagDriftTestBerlin, lastMileMs: 5, durationMs: 5000, sessionStartsAt: now.Add(300 * time.Millisecond),
		}, now.Add(5500*time.Millisecond))
		fixture.finish(t, allocated.Token, 1, LagDriftStopConverged, now.Add(5600*time.Millisecond))
		later := now.Add(time.Minute)
		run := fixture.runView(t, allocated.DriftTestId, 1, later)
		connect.AssertEqual(t, run.State, model.LagDriftRunStateProcessing)
		connect.AssertEqual(t, model.CountStuckLagDriftRuns(ctx, later), 1)

		// with the borders back, the next look completes it
		restore()
		connect.AssertEqual(t, fixture.runView(t, allocated.DriftTestId, 1, later).State, model.LagDriftRunStateComplete)
		connect.AssertEqual(t, model.CountStuckLagDriftRuns(ctx, later), 0)
	})
}

// Verdicts use the calibration in force and record it (V4), unless an
// operator switched calibration off (lag.yml drift.calibration_enabled), when
// they use the design's defaults and record no calibration.
func TestLagDriftCalibrationSwitch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		calibrationId := model.AddLagDriftCalibration(ctx, now, now.Add(-7*24*time.Hour), now, &model.LagDriftCalibrationResult{
			Kappa:            0.3,
			Sigma0Ms:         1.5,
			RelayThresholdMs: 60,
			FitTracks:        2000,
			FitObservations:  20000,
		})
		drift := DefaultLagDriftSettings()

		inForce := lagDriftCalibrationFor(ctx, drift)
		connect.AssertEqual(t, *inForce.CalibrationId, calibrationId)
		connect.AssertEqual(t, inForce.Kappa, 0.3)
		recorded := newLagDriftCalibrationView(inForce, inForce.RelayThresholdMs)
		connect.AssertEqual(t, *recorded.CalibrationId, calibrationId)
		connect.AssertEqual(t, recorded.Sigma0Ms, 1.5)

		off := false
		drift.CalibrationEnabled = &off
		defaults := lagDriftCalibrationFor(ctx, drift)
		connect.AssertEqual(t, defaults.CalibrationId, (*server.Id)(nil))
		connect.AssertEqual(t, defaults.Kappa, 0.25)
		connect.AssertEqual(t, defaults.Sigma0Ms, 1.0)
		connect.AssertEqual(t, defaults.RelayThresholdMs, 80.0)
		recorded = newLagDriftCalibrationView(defaults, defaults.RelayThresholdMs)
		connect.AssertEqual(t, recorded.CalibrationId, (*server.Id)(nil))
		connect.AssertEqual(t, recorded.CalibratedAt, (*string)(nil))
	})
}
