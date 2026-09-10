// The results side of the onboarding program (mmm/onboarding/PLAN.md
// "OPTIMIZATION LOOP" §3-§5): the fixed-window outcome definitions the nightly
// rollup applies to one network, the guardrail decision the rollup makes per
// experiment variant, the keyset cursor of the admin results endpoint and the
// precedence rule that overlays a paused variant on the registry's assignment.
// Pure functions, unit tested without a database.
package onboarding

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// The fixed windows, measured from the network's cohort day (the
// network_onboarding row's created_at). A window that has not matured at rollup
// time counts as not reached; the row's matured_days says how far the cohort is.
const (
	WindowAppOpen        = 48 * time.Hour
	WindowConnect        = 7 * 24 * time.Hour
	WindowWidget         = 7 * 24 * time.Hour
	WindowFeedback       = 7 * 24 * time.Hour
	WindowProStart       = 14 * 24 * time.Hour
	WindowTrialToPaid    = 35 * 24 * time.Hour
	WindowRefund         = 60 * 24 * time.Hour
	RetentionD7          = 7
	RetentionD30         = 30
	retentionD7Start     = 6 * 24 * time.Hour
	retentionD7End       = 8 * 24 * time.Hour
	retentionD30Start    = 28 * 24 * time.Hour
	retentionD30End      = 31 * 24 * time.Hour
	TrialLength          = 14 * 24 * time.Hour
	TrialOutcomeGrace    = 3 * 24 * time.Hour
	GuardrailCohortDays  = 14
	GuardrailPausedState = "paused"
)

// Outcome event names the rollup reads (mirrors model's schema constants; the
// pure package cannot import model).
const (
	EventEmailSent         = "email.sent"
	EventEmailDelivered    = "email.delivered"
	EventEmailOpened       = "email.opened"
	EventEmailClicked      = "email.clicked"
	EventEmailComplained   = "email.complained"
	EventEmailUnsubscribed = "email.unsubscribed"
	EventLandingClicked    = "landing.clicked"
	EventAppOpened         = "app.opened"
	EventConnectFirst      = "connect.first"
	EventWidgetAdded       = "widget.added"
	EventFeedbackSubmitted = "feedback.submitted"
	EventPurchaseCompleted = "purchase.completed"
	EventTrialConverted    = "trial.converted"
	EventTrialCancelled    = "trial.cancelled"
	EventRefund            = "refund"
	EventRetentionD7       = "retention.d7"
	EventRetentionD30      = "retention.d30"
	EventConnectDay        = "connect.day"
)

// NetworkFacts is everything the rollup knows about one network: its cohort
// time and the first time each outcome was observed. Nil means never.
type NetworkFacts struct {
	CohortAt time.Time
	// FirstEventAt is the earliest `at` per event name.
	FirstEventAt map[string]time.Time
	// ConnectionDays are the UTC days with at least one connection, in any
	// order: the network's connect.day events (one per day, written by the
	// client connection path). network_client_connection is pruned hours after
	// disconnect and never backs this.
	ConnectionDays []time.Time
	// FirstFeedbackAt is the earliest account feedback (the feedback screen).
	FirstFeedbackAt *time.Time
	// FirstProAt is the earliest subscription renewal start (a trial start is a
	// Pro start; trial_to_paid is the separate column).
	FirstProAt *time.Time
}

func (f NetworkFacts) first(name string) (time.Time, bool) {
	t, ok := f.FirstEventAt[name]
	return t, ok
}

// within is whether the first observation fell inside [cohort, cohort+window).
func (f NetworkFacts) within(name string, window time.Duration) bool {
	t, ok := f.first(name)
	return ok && !t.Before(f.CohortAt) && t.Before(f.CohortAt.Add(window))
}

// Outcomes are the per-network booleans the results row counts.
type Outcomes struct {
	Sent           bool
	Delivered      bool
	Opened         bool
	Clicked        bool
	LandingClicked bool
	AppOpen48h     bool
	Connect7d      bool
	Widget7d       bool
	Feedback7d     bool
	ProStart14d    bool
	TrialToPaid35d bool
	Refund60d      bool
	RetentionD7    bool
	RetentionD30   bool
	Unsubscribe    bool
	Complaint      bool
}

// ComputeOutcomes applies the fixed windows to one network at `now`. The exact
// definitions (also documented on GET /admin/onboarding/results):
//   - sent, delivered, opened, clicked, unsubscribe, complaint: at least one
//     email.* event of that name, any step, any time
//   - landing_clicked: at least one landing.clicked
//   - app_open_48h: an app.opened (the auth-client attribution that only fires
//     within 48 h of a landing click) any time after the cohort day
//   - connect_7d: a connection day, or connect.first, within 7 days of the cohort
//   - widget_7d, feedback_7d: widget.added / feedback (event or the feedback
//     table) within 7 days
//   - pro_start_14d: the first subscription renewal (trial included) or a
//     purchase.completed within 14 days
//   - trial_to_paid_35d: trial.converted within 35 days
//   - refund_60d: a refund within 60 days
//   - retention_d7: a connection on day 6 or 7 ([cohort+6d, cohort+8d));
//     retention_d30: a connection in [cohort+28d, cohort+31d)
//
// A window that has not matured at `now` reports false; callers read
// MaturedDays to tell "not yet" from "did not".
func ComputeOutcomes(f NetworkFacts, now time.Time) Outcomes {
	matured := func(window time.Duration) bool {
		return !now.Before(f.CohortAt.Add(window))
	}
	_, sent := f.first(EventEmailSent)
	_, delivered := f.first(EventEmailDelivered)
	_, opened := f.first(EventEmailOpened)
	_, clicked := f.first(EventEmailClicked)
	_, landing := f.first(EventLandingClicked)
	_, unsub := f.first(EventEmailUnsubscribed)
	_, complaint := f.first(EventEmailComplained)
	appOpen := false
	if t, ok := f.first(EventAppOpened); ok && !t.Before(f.CohortAt) {
		appOpen = true
	}

	o := Outcomes{
		Sent:           sent,
		Delivered:      delivered,
		Opened:         opened,
		Clicked:        clicked,
		LandingClicked: landing,
		AppOpen48h:     appOpen,
		Unsubscribe:    unsub,
		Complaint:      complaint,
	}
	if matured(WindowConnect) {
		o.Connect7d = f.within(EventConnectFirst, WindowConnect) || f.connectedBetween(0, WindowConnect)
	}
	if matured(WindowWidget) {
		o.Widget7d = f.within(EventWidgetAdded, WindowWidget)
	}
	if matured(WindowFeedback) {
		o.Feedback7d = f.within(EventFeedbackSubmitted, WindowFeedback) ||
			(f.FirstFeedbackAt != nil && !f.FirstFeedbackAt.Before(f.CohortAt) && f.FirstFeedbackAt.Before(f.CohortAt.Add(WindowFeedback)))
	}
	if matured(WindowProStart) {
		o.ProStart14d = f.within(EventPurchaseCompleted, WindowProStart) ||
			(f.FirstProAt != nil && !f.FirstProAt.Before(f.CohortAt) && f.FirstProAt.Before(f.CohortAt.Add(WindowProStart)))
	}
	if matured(WindowTrialToPaid) {
		o.TrialToPaid35d = f.within(EventTrialConverted, WindowTrialToPaid)
	}
	if matured(WindowRefund) {
		o.Refund60d = f.within(EventRefund, WindowRefund)
	}
	if matured(retentionD7End) {
		o.RetentionD7 = f.connectedBetween(retentionD7Start, retentionD7End)
	}
	if matured(retentionD30End) {
		o.RetentionD30 = f.connectedBetween(retentionD30Start, retentionD30End)
	}
	return o
}

// connectedBetween is whether any connection day falls in
// [cohort+from, cohort+to), comparing whole UTC days.
func (f NetworkFacts) connectedBetween(from time.Duration, to time.Duration) bool {
	start := f.CohortAt.Add(from)
	end := f.CohortAt.Add(to)
	for _, day := range f.ConnectionDays {
		dayStart := day.UTC().Truncate(24 * time.Hour)
		dayEnd := dayStart.Add(24 * time.Hour)
		if dayEnd.After(start) && dayStart.Before(end) {
			return true
		}
	}
	return false
}

// MaturedDays is how many whole days the cohort has aged at `now` (0 when the
// cohort day is today or in the future).
func MaturedDays(cohortAt time.Time, now time.Time) int {
	if now.Before(cohortAt) {
		return 0
	}
	return int(now.Sub(cohortAt).Hours() / 24)
}

// CohortDay is the UTC day a network belongs to.
func CohortDay(createdAt time.Time) time.Time {
	return createdAt.UTC().Truncate(24 * time.Hour)
}

// ----- trial outcome backstop -----

// TrialBackstop is the decision the daily sweep makes for a network whose trial
// started at `trialStartedAt` and has neither converted nor cancelled: once the
// trial plus a grace period has passed, a network that is Pro converted and one
// that is not cancelled. Before that, nothing is decided (the store webhooks
// usually get there first; this is the store-agnostic fallback).
func TrialBackstop(trialStartedAt time.Time, isPro bool, now time.Time) (event string) {
	if now.Before(trialStartedAt.Add(TrialLength + TrialOutcomeGrace)) {
		return ""
	}
	if isPro {
		return EventTrialConverted
	}
	return EventTrialCancelled
}

// ----- guardrails -----

// GuardrailInput is one variant's aggregate over the guardrail window.
type GuardrailInput struct {
	Exposures   int
	Sent        int
	Delivered   int
	Unsubscribe int
	Complaint   int
	ProStart14d int
	Refund60d   int
}

// GuardrailDecision is the outcome of checking one variant against the
// registry's guardrails.
type GuardrailDecision struct {
	// Breached is whether any guardrail rate crossed its threshold.
	Breached bool
	// Guardrail names the first breached guardrail (unsubscribe_rate,
	// complaint_rate, refund_rate).
	Guardrail string
	Rate      float64
	Threshold float64
	// Reason is a plain sentence for the log and the ctl output.
	Reason string
}

// Guardrail names the registry may carry and their denominators.
const (
	GuardrailUnsubscribeRate = "unsubscribe_rate"
	GuardrailComplaintRate   = "complaint_rate"
	GuardrailRefundRate      = "refund_rate"
)

// guardrailMinDenominator is the smallest denominator a rate is judged on; a
// handful of refunds on three subscriptions is noise, not a breach.
const guardrailMinDenominator = 20

// DecideGuardrails checks the rates in registry order (sorted names, so the
// decision is deterministic). Insufficient volume (exposures under
// minExposures, or a denominator under guardrailMinDenominator) never breaches.
func DecideGuardrails(guardrails map[string]float64, minExposures int, in GuardrailInput) GuardrailDecision {
	if len(guardrails) == 0 {
		return GuardrailDecision{Reason: "no guardrails"}
	}
	if in.Exposures < minExposures {
		return GuardrailDecision{Reason: fmt.Sprintf("insufficient volume: %d exposures, %d required", in.Exposures, minExposures)}
	}
	names := make([]string, 0, len(guardrails))
	for name := range guardrails {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		threshold := guardrails[name]
		if threshold <= 0 {
			continue
		}
		var numerator, denominator int
		switch name {
		case GuardrailUnsubscribeRate:
			numerator, denominator = in.Unsubscribe, in.Sent
		case GuardrailComplaintRate:
			numerator, denominator = in.Complaint, in.Delivered
		case GuardrailRefundRate:
			numerator, denominator = in.Refund60d, in.ProStart14d
		default:
			continue
		}
		if denominator < guardrailMinDenominator {
			continue
		}
		rate := float64(numerator) / float64(denominator)
		if threshold < rate {
			return GuardrailDecision{
				Breached:  true,
				Guardrail: name,
				Rate:      rate,
				Threshold: threshold,
				Reason:    fmt.Sprintf("%s %.4f exceeds %.4f (%d/%d)", name, rate, threshold, numerator, denominator),
			}
		}
	}
	return GuardrailDecision{Reason: "within guardrails"}
}

// ----- paused-variant overlay -----

// EffectiveVariant applies the pause overlay to the registry's hash assignment:
// an assigned variant that is not paused stands; a paused one falls to
// `control` when it exists and is not paused, else to the first non-paused
// variant in sorted order, else "" (the experiment is effectively off). The
// fallback is deterministic, so every server agrees, and it applies to existing
// networks too: a breached variant stops for everyone, which is the point of
// the pause.
func EffectiveVariant(assigned string, variants []string, paused map[string]bool) string {
	if !paused[assigned] {
		return assigned
	}
	if _, ok := paused["control"]; !ok {
		for _, name := range variants {
			if name == "control" {
				return name
			}
		}
	}
	names := append([]string{}, variants...)
	sort.Strings(names)
	for _, name := range names {
		if !paused[name] {
			return name
		}
	}
	return ""
}

// ----- admin results cursor -----

// ResultsKey is the ordering tuple of a results row and the keyset cursor.
type ResultsKey struct {
	CohortDay  string `json:"d"`
	Experiment string `json:"e"`
	Variant    string `json:"v"`
	Surface    string `json:"s"`
	Platform   string `json:"p"`
	Tier       string `json:"t"`
	Path       string `json:"a"`
}

// EncodeResultsCursor is the opaque page cursor for a key.
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
	if k.Surface != o.Surface {
		return k.Surface < o.Surface
	}
	if k.Platform != o.Platform {
		return k.Platform < o.Platform
	}
	if k.Tier != o.Tier {
		return k.Tier < o.Tier
	}
	return k.Path < o.Path
}

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
	if key.CohortDay == "" {
		return ResultsKey{}, fmt.Errorf("invalid cursor")
	}
	return key, nil
}

// NextRollupAt is the next 02:00 UTC strictly after `now`: the nightly rollup's
// schedule (late outcomes from the previous day have landed by then).
func NextRollupAt(now time.Time) time.Time {
	day := now.UTC().Truncate(24 * time.Hour)
	at := day.Add(2 * time.Hour)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}

// Rate is numerator/denominator, or 0 when the denominator is 0 (for the ctl
// output; never NaN).
func Rate(numerator int, denominator int) float64 {
	if denominator <= 0 {
		return 0
	}
	r := float64(numerator) / float64(denominator)
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return 0
	}
	return r
}
