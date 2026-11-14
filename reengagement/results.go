// SPDX-License-Identifier: MPL-2.0

package reengagement

// The results side of the program (mmm/reengagement/PLAN.md "MEASUREMENT"):
// the outcome windows the nightly rollup applies to one episode, the keyset
// cursor of GET /admin/reengagement/results and the rollup's schedule. Pure,
// like onboarding/results.go: the model loads each episode's facts, bounded per
// cohort day, and the controller aggregates the outcomes into
// reengagement_results_daily.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The outcome windows, measured from the episode's first step (its real or
// virtual send) and open at the step: an event counts in (step, step + window].
// A window that has not closed when the rollup runs counts nothing; the row's
// matured_days says how far its cohort day has aged.
const (
	WindowReactivation7d  = 7 * 24 * time.Hour
	WindowReactivation30d = 30 * 24 * time.Hour
	// retention_90d: a connection on days 61 through 90 after the step
	WindowRetention90dFrom = 60 * 24 * time.Hour
	WindowRetention90dTo   = 90 * 24 * time.Hour
	WindowProRestart30d    = 30 * 24 * time.Hour
)

// The horizons the model loads facts within: the longest window over each
// kind of fact, so the windows above always see everything they need.
const (
	ResultsConnectHorizon = WindowRetention90dTo
	ResultsPaidHorizon    = WindowProRestart30d
)

// ResultsEventSlack is how long before the first step the episode's own events
// may be stamped: Brevo's webhook times are whole seconds, so a delivery in the
// send's own second reads as up to a second before it.
const ResultsEventSlack = time.Minute

// ResultsUnknown is the stream, platform or tier of an episode that did not
// record one.
const ResultsUnknown = "unknown"

// EpisodeFacts is what the rollup knows about one episode. The episode's own
// events are the ones from its first step (less ResultsEventSlack) to the next
// episode's start; its connections and paid windows are the network's, from
// the first step on, whatever came after (intent to treat).
type EpisodeFacts struct {
	// FirstStepAt is the episode's first real send (reengage.email.sent) or
	// virtual send (reengage.virtual_send)
	FirstStepAt time.Time
	// Virtual is whether the first step was a virtual send: the holdout
	Virtual bool

	// at least one event of each name in the episode
	Sent           bool
	Delivered      bool
	LandingClicked bool
	Answered       bool
	Kept           bool
	Unsubscribed   bool
	Complained     bool
	Bounced        bool

	// ConnectDays are the network's connect.day times (one per UTC day with a
	// connection, at its first connection that day) from the first step on
	ConnectDays []time.Time
	// PaidStarts are the starts of the network's supporter windows with
	// positive net revenue from the first step on that the network bought: a
	// redeemed gift year carries its giver's payment and is not one
	PaidStarts []time.Time
}

// EpisodeOutcomes are the per-episode booleans a results row counts.
type EpisodeOutcomes struct {
	Virtual         bool
	Sent            bool
	Delivered       bool
	LandingClicked  bool
	ReasonAnswered  bool
	Kept            bool
	Reactivation7d  bool
	Reactivation30d bool
	Retention90d    bool
	ProRestart30d   bool
	Unsubscribe     bool
	Complaint       bool
	HardBounce      bool
}

// ComputeEpisodeOutcomes applies the windows to one episode at `now`. The
// definitions (also documented on GET /admin/reengagement/results):
//   - sent, delivered, landing_clicked, reason_answers, keeps, unsubscribe,
//     complaint, hard_bounce: at least one reengage.email.sent,
//     reengage.email.delivered, reengage.landing.clicked, reengage.reason,
//     reengage.keep, reengage.email.unsubscribed, reengage.email.complained,
//     reengage.email.bounced in the episode
//   - reactivation_7d, reactivation_30d: a connect.day in (step, step + 7d],
//     (step, step + 30d]
//   - retention_90d: a connect.day in (step + 60d, step + 90d], days 61-90
//   - pro_restart_30d: a paid supporter window starting in (step, step + 30d],
//     one the network bought (never a redeemed gift year)
//
// A window that has not closed at `now` reports false; read matured_days to
// tell "not yet" from "did not".
func ComputeEpisodeOutcomes(f EpisodeFacts, now time.Time) EpisodeOutcomes {
	step := f.FirstStepAt
	closed := func(window time.Duration) bool {
		return !now.Before(step.Add(window))
	}
	o := EpisodeOutcomes{
		Virtual:        f.Virtual,
		Sent:           f.Sent,
		Delivered:      f.Delivered,
		LandingClicked: f.LandingClicked,
		ReasonAnswered: f.Answered,
		Kept:           f.Kept,
		Unsubscribe:    f.Unsubscribed,
		Complaint:      f.Complained,
		HardBounce:     f.Bounced,
	}
	if closed(WindowReactivation7d) {
		o.Reactivation7d = anyAfterStep(step, f.ConnectDays, 0, WindowReactivation7d)
	}
	if closed(WindowReactivation30d) {
		o.Reactivation30d = anyAfterStep(step, f.ConnectDays, 0, WindowReactivation30d)
	}
	if closed(WindowRetention90dTo) {
		o.Retention90d = anyAfterStep(step, f.ConnectDays, WindowRetention90dFrom, WindowRetention90dTo)
	}
	if closed(WindowProRestart30d) {
		o.ProRestart30d = anyAfterStep(step, f.PaidStarts, 0, WindowProRestart30d)
	}
	return o
}

// anyAfterStep is whether any time falls in (step + from, step + to].
func anyAfterStep(step time.Time, times []time.Time, from time.Duration, to time.Duration) bool {
	start := step.Add(from)
	end := step.Add(to)
	for _, t := range times {
		if start.Before(t) && !end.Before(t) {
			return true
		}
	}
	return false
}

// FirstStepStream is the stream whose episodes open with `flowStep`: r1 and r2
// open a lapsed episode (R2 takes R1's day when nothing changed), n1 a
// never-activated one, f1 a fix follow-up, w1 a win-back. R3 never opens one.
// The rollup's fallback when neither the episode's reengage.episode.started
// nor its network_reengagement row names the stream; "" when the step says
// nothing.
func FirstStepStream(flowStep string) string {
	switch flowStep {
	case StepR1, StepR2:
		return StreamLapsed
	case StepN1:
		return StreamNeverActivated
	case StepF1:
		return StreamFix
	case StepW1:
		return StreamWinback
	}
	return ""
}

// ----- admin results cursor -----

// ResultsKey is the ordering tuple of a results row and the keyset cursor of
// GET /admin/reengagement/results.
type ResultsKey struct {
	CohortDay  string `json:"d"`
	Experiment string `json:"e"`
	Variant    string `json:"v"`
	Stream     string `json:"s"`
	Platform   string `json:"p"`
	Tier       string `json:"t"`
}

// Less orders keys as the results table does: the dimension tuple in order.
func (k ResultsKey) Less(o ResultsKey) bool {
	if k.CohortDay != o.CohortDay {
		return k.CohortDay < o.CohortDay
	}
	if k.Experiment != o.Experiment {
		return k.Experiment < o.Experiment
	}
	if k.Variant != o.Variant {
		return k.Variant < o.Variant
	}
	if k.Stream != o.Stream {
		return k.Stream < o.Stream
	}
	if k.Platform != o.Platform {
		return k.Platform < o.Platform
	}
	return k.Tier < o.Tier
}

// EncodeResultsCursor is the opaque page cursor for a key.
func EncodeResultsCursor(key ResultsKey) string {
	b, _ := json.Marshal(key)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeResultsCursor parses a cursor; an empty cursor is the first page.
func DecodeResultsCursor(cursor string) (ResultsKey, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return ResultsKey{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return ResultsKey{}, fmt.Errorf("invalid cursor")
	}
	var key ResultsKey
	if err := json.Unmarshal(b, &key); err != nil {
		return ResultsKey{}, fmt.Errorf("invalid cursor")
	}
	if _, err := time.Parse("2006-01-02", key.CohortDay); err != nil {
		return ResultsKey{}, fmt.Errorf("invalid cursor")
	}
	return key, nil
}

// ----- schedule -----

// The rollup runs at 02:45 UTC, after the 02:30 scan (the scan opens the day's
// episodes; the rollup recomputes the cohort days up to yesterday).
const resultsRollupHour, resultsRollupMinute = 2, 45

// NextResultsRollupAt is the next 02:45 UTC strictly after `now`.
func NextResultsRollupAt(now time.Time) time.Time {
	now = now.UTC()
	at := time.Date(now.Year(), now.Month(), now.Day(), resultsRollupHour, resultsRollupMinute, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}
