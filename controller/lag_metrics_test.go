package controller

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
)

// Each answer of the lag routes counts under one bounded outcome (X9): a
// refusal by its code, an error by whether the caller was at fault, and a
// reissue apart from a new track, because a reissue's cap is not an abuse
// limit.
func TestLagSessionAndTrackOutcomes(t *testing.T) {
	limit := &LagError{Code: LagErrorCodeLimit}
	unavailable := &LagError{Code: LagErrorCodeUnavailable}

	connect.AssertEqual(t, lagSessionOutcome(&LagSessionResult{}, nil), lagOutcomeCreated)
	connect.AssertEqual(t, lagSessionOutcome(&LagSessionResult{Error: limit}, nil), lagOutcomeRefusedLimit)
	connect.AssertEqual(t, lagSessionOutcome(&LagSessionResult{Error: unavailable}, nil), lagOutcomeUnavailable)
	connect.AssertEqual(t, lagSessionOutcome(nil, errors.New("could not read the caller address")), lagOutcomeError)
	connect.AssertEqual(t, lagSessionOutcome(nil, fmt.Errorf("400 A bad request.")), lagOutcomeRejected)
	connect.AssertEqual(t, lagSessionOutcome(nil, nil), lagOutcomeError)

	connect.AssertEqual(t, lagTrackOutcome(false, &LagTrackResult{}, nil), lagOutcomeCreated)
	connect.AssertEqual(t, lagTrackOutcome(true, &LagTrackResult{}, nil), lagOutcomeReissued)
	connect.AssertEqual(t, lagTrackOutcome(false, &LagTrackResult{Error: limit}, nil), lagOutcomeRefusedLimit)
	connect.AssertEqual(t, lagTrackOutcome(false, &LagTrackResult{Error: unavailable}, nil), lagOutcomeUnavailable)
	connect.AssertEqual(t, lagTrackOutcome(true, &LagTrackResult{Error: limit}, nil), lagOutcomeReissueRefused)
	connect.AssertEqual(t, lagTrackOutcome(true, &LagTrackResult{Error: unavailable}, nil), lagOutcomeReissueRefused)
	connect.AssertEqual(t, lagTrackOutcome(false, nil, fmt.Errorf("401 A track requires a client.")), lagOutcomeRejected)
	connect.AssertEqual(t, lagTrackOutcome(false, nil, errors.New("db down")), lagOutcomeError)
}

// A post counts once by outcome, and each of its reports once: accepted, or
// rejected by its reason, with a reason this server does not name folded into
// rejected_other.
func TestLagReportCounts(t *testing.T) {
	read := func(counter string, outcome string) float64 {
		switch counter {
		case "posts":
			return testutil.ToFloat64(lagReportPostsCounter.WithLabelValues(outcome))
		default:
			return testutil.ToFloat64(lagReportsCounter.WithLabelValues(outcome))
		}
	}
	processed := read("posts", lagReportOutcomeProcessed)
	rateLimited := read("posts", lagReportOutcomeRateLimited)
	rejectedPosts := read("posts", lagOutcomeRejected)
	accepted := read("reports", lagReportOutcomeAccepted)
	duplicate := read("reports", "rejected_"+LagReportRejectDuplicate)
	signature := read("reports", "rejected_"+LagReportRejectSignature)
	other := read("reports", lagReportOutcomeOther)

	countLagReports(&ExtenderLagReportResult{
		Accepted: 3,
		Rejected: []*ExtenderLagReportRejection{
			{Index: 3, Reason: LagReportRejectDuplicate},
			{Index: 4, Reason: LagReportRejectSignature},
			{Index: 5, Reason: "a reason from a newer server"},
		},
	}, nil)
	countLagReports(nil, &lagReportLimitError{retryAfter: time.Minute})
	countLagReports(nil, fmt.Errorf("400 A lag report post carries at most 32 reports."))

	connect.AssertEqual(t, read("posts", lagReportOutcomeProcessed)-processed, float64(1))
	connect.AssertEqual(t, read("posts", lagReportOutcomeRateLimited)-rateLimited, float64(1))
	connect.AssertEqual(t, read("posts", lagOutcomeRejected)-rejectedPosts, float64(1))
	connect.AssertEqual(t, read("reports", lagReportOutcomeAccepted)-accepted, float64(3))
	connect.AssertEqual(t, read("reports", "rejected_"+LagReportRejectDuplicate)-duplicate, float64(1))
	connect.AssertEqual(t, read("reports", "rejected_"+LagReportRejectSignature)-signature, float64(1))
	connect.AssertEqual(t, read("reports", lagReportOutcomeOther)-other, float64(1))
}

func TestLagSweepCounts(t *testing.T) {
	read := func(kind string) float64 {
		return testutil.ToFloat64(lagSweepDeletedCounter.WithLabelValues(kind))
	}
	guests, tickets, results, tracks := read("guests"), read("tickets"), read("results"), read("tracks")

	CountLagSweep(2, 0, 5, 1)

	connect.AssertEqual(t, read("guests")-guests, float64(2))
	connect.AssertEqual(t, read("tickets")-tickets, float64(0))
	connect.AssertEqual(t, read("results")-results, float64(5))
	connect.AssertEqual(t, read("tracks")-tracks, float64(1))
}
