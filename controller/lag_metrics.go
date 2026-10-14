package controller

import (
	"errors"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
)

// lag.dev's counters (lag.dev/PLAN.md X9), read by
// grafana/dashboards/lag-dev.json beside the stats collector's lag gauges
// (stats_collector_lag.go). The handlers count what the database cannot
// show: refusals, rejections and outcomes that store nothing. Every label is
// a bounded enum, and none names a visitor, an address or an extender
// (amendment 9). The api serves the lag routes and the taskworker runs the
// sweep, so those processes are the ones that push non-zero series.

var lagSessionsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "sessions_total",
		Help:      "lag.dev guest sessions asked for (A1), by outcome: created, refused_limit, unavailable, rejected or error",
	},
	[]string{"outcome"},
)

var lagTracksCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "tracks_total",
		Help:      "lag.dev tracks asked for (A6), by outcome: created, reissued, refused_limit, reissue_refused, unavailable, rejected or error",
	},
	[]string{"outcome"},
)

var lagReportsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "reports_total",
		Help:      "lag.dev reports an extender posted (A7), by outcome: accepted or rejected_<reason>",
	},
	[]string{"outcome"},
)

var lagReportPostsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "report_posts_total",
		Help:      "lag.dev report posts (A7), by outcome: processed, rate_limited, rejected or error",
	},
	[]string{"outcome"},
)

var lagSweepDeletedCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "lag",
		Name:      "sweep_deleted_total",
		Help:      "Rows the lag sweep removed (A2, A5), by kind: guests, tickets, results or tracks",
	},
	[]string{"kind"},
)

// Registers the lag.dev counters with the default registry.
func init() {
	prometheus.MustRegister(
		lagSessionsCounter,
		lagTracksCounter,
		lagReportsCounter,
		lagReportPostsCounter,
		lagSweepDeletedCounter,
	)
}

// The outcomes of a guest session or track request.
const (
	lagOutcomeCreated        = "created"
	lagOutcomeReissued       = "reissued"
	lagOutcomeRefusedLimit   = "refused_limit"
	lagOutcomeReissueRefused = "reissue_refused"
	lagOutcomeUnavailable    = "unavailable"
	lagOutcomeRejected       = "rejected"
	lagOutcomeError          = "error"
)

// The outcomes of a report post, and of each report in it.
const (
	lagReportOutcomeProcessed   = "processed"
	lagReportOutcomeRateLimited = "rate_limited"
	lagReportOutcomeAccepted    = "accepted"
	lagReportOutcomeOther       = "rejected_other"
)

// Every reason a report is rejected for (A7), each counted as
// rejected_<reason>. A reason this server does not name counts as
// rejected_other, so the label stays bounded.
var lagReportRejectReasons = map[string]bool{
	LagReportRejectMalformed: true,
	LagReportRejectExtender:  true,
	LagReportRejectSignature: true,
	LagReportRejectTicket:    true,
	LagReportRejectBounds:    true,
	LagReportRejectDuplicate: true,
}

// An error that answers the caller with a 4xx status, which the handlers
// write as its first word.
var lagClientErrorPattern = regexp.MustCompile(`^4[0-9][0-9] `)

// The outcome of a request a handler failed with an error: rejected when the
// caller's request was at fault (a 4xx), else error.
func lagErrorOutcome(err error) string {
	if lagClientErrorPattern.MatchString(err.Error()) {
		return lagOutcomeRejected
	}
	return lagOutcomeError
}

// The outcome of a refusal answered in the result's error field (A1, A6).
func lagRefusalOutcome(lagError *LagError, limitOutcome string) string {
	switch lagError.Code {
	case LagErrorCodeLimit:
		return limitOutcome
	case LagErrorCodeUnavailable:
		return lagOutcomeUnavailable
	}
	return lagOutcomeError
}

// Counts one answer to `POST /lag/session`.
func countLagSession(result *LagSessionResult, err error) {
	lagSessionsCounter.WithLabelValues(lagSessionOutcome(result, err)).Inc()
}

func lagSessionOutcome(result *LagSessionResult, err error) string {
	switch {
	case err != nil:
		return lagErrorOutcome(err)
	case result == nil:
		return lagOutcomeError
	case result.Error != nil:
		return lagRefusalOutcome(result.Error, lagOutcomeRefusedLimit)
	}
	return lagOutcomeCreated
}

// Counts one answer to `POST /lag/track`, a new track or a reissue in one.
func countLagTrack(reissue bool, result *LagTrackResult, err error) {
	lagTracksCounter.WithLabelValues(lagTrackOutcome(reissue, result, err)).Inc()
}

func lagTrackOutcome(reissue bool, result *LagTrackResult, err error) string {
	switch {
	case err != nil:
		return lagErrorOutcome(err)
	case result == nil:
		return lagOutcomeError
	case result.Error != nil && reissue:
		// a reissue's limit is its cap of one per dot and a few per track,
		// not an abuse limit, and an unavailable reissue is a track that can
		// no longer be reissued
		return lagOutcomeReissueRefused
	case result.Error != nil:
		return lagRefusalOutcome(result.Error, lagOutcomeRefusedLimit)
	case reissue:
		return lagOutcomeReissued
	}
	return lagOutcomeCreated
}

// Counts one report post and each report in it.
func countLagReports(result *ExtenderLagReportResult, err error) {
	if err != nil {
		var limitErr *lagReportLimitError
		if errors.As(err, &limitErr) {
			lagReportPostsCounter.WithLabelValues(lagReportOutcomeRateLimited).Inc()
		} else {
			lagReportPostsCounter.WithLabelValues(lagErrorOutcome(err)).Inc()
		}
		return
	}
	if result == nil {
		lagReportPostsCounter.WithLabelValues(lagOutcomeError).Inc()
		return
	}
	lagReportPostsCounter.WithLabelValues(lagReportOutcomeProcessed).Inc()
	if 0 < result.Accepted {
		lagReportsCounter.WithLabelValues(lagReportOutcomeAccepted).Add(float64(result.Accepted))
	}
	for _, rejection := range result.Rejected {
		lagReportsCounter.WithLabelValues(lagReportRejectOutcome(rejection.Reason)).Inc()
	}
}

func lagReportRejectOutcome(reason string) string {
	if lagReportRejectReasons[reason] {
		return "rejected_" + reason
	}
	return lagReportOutcomeOther
}

// CountLagSweep counts what one lag sweep removed.
func CountLagSweep(guests int, tickets int, results int, tracks int) {
	for kind, count := range map[string]int{
		"guests":  guests,
		"tickets": tickets,
		"results": results,
		"tracks":  tracks,
	} {
		if 0 < count {
			lagSweepDeletedCounter.WithLabelValues(kind).Add(float64(count))
		}
	}
}
