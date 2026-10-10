// SPDX-License-Identifier: MPL-2.0

package onboarding

import (
	"testing"
	"time"
)

func cohortFacts(cohort time.Time) NetworkFacts {
	return NetworkFacts{CohortAt: cohort, FirstEventAt: map[string]time.Time{}}
}

func TestComputeOutcomesWindows(t *testing.T) {
	cohort := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	d := func(days float64) time.Time { return cohort.Add(time.Duration(days * 24 * float64(time.Hour))) }
	f := cohortFacts(cohort)
	f.FirstEventAt[EventEmailSent] = d(1)
	f.FirstEventAt[EventEmailDelivered] = d(1)
	f.FirstEventAt[EventEmailOpened] = d(2)
	f.FirstEventAt[EventEmailClicked] = d(2)
	f.FirstEventAt[EventLandingClicked] = d(2)
	f.FirstEventAt[EventAppOpened] = d(2.5)
	f.FirstEventAt[EventWidgetAdded] = d(6.9)
	f.FirstEventAt[EventFeedbackSubmitted] = d(7.5) // outside the 7 day window
	f.FirstEventAt[EventPurchaseCompleted] = d(3)
	f.FirstEventAt[EventTrialConverted] = d(17)
	f.FirstEventAt[EventRefund] = d(40)
	f.FirstEventAt[EventEmailUnsubscribed] = d(9)
	f.ConnectionDays = []time.Time{d(0.1), d(6.5), d(29)}
	pro := d(3)
	f.FirstProAt = &pro

	// fully matured
	o := ComputeOutcomes(f, d(61))
	want := Outcomes{
		Sent: true, Delivered: true, Opened: true, Clicked: true, LandingClicked: true,
		AppOpen48h: true, Connect7d: true, Widget7d: true, Feedback7d: false,
		ProStart14d: true, TrialToPaid35d: true, Refund60d: true,
		RetentionD7: true, RetentionD30: true, Unsubscribe: true, Complaint: false,
	}
	if o != want {
		t.Fatalf("matured outcomes = %+v, want %+v", o, want)
	}

	// at day 8: the 7 day windows and retention_d7 are matured, nothing longer
	o = ComputeOutcomes(f, d(8))
	if !o.Connect7d || !o.Widget7d || !o.RetentionD7 {
		t.Fatalf("day 8 outcomes missing the matured windows: %+v", o)
	}
	if o.ProStart14d || o.TrialToPaid35d || o.Refund60d || o.RetentionD30 {
		t.Fatalf("day 8 outcomes report immature windows: %+v", o)
	}
	// the always-on columns do not wait
	if !o.Sent || !o.Unsubscribe || !o.AppOpen48h {
		t.Fatalf("day 8 outcomes lost the unwindowed columns: %+v", o)
	}

	// retention_d30 needs a connection in [28d, 31d): move the day-29 connection
	f.ConnectionDays = []time.Time{d(0.1), d(6.5), d(27)}
	if ComputeOutcomes(f, d(61)).RetentionD30 {
		t.Fatalf("retention_d30 counted a day-27 connection")
	}
	// retention_d7 needs [6d, 8d): a day-5 connection does not count
	f.ConnectionDays = []time.Time{d(0.1), d(5.2)}
	if ComputeOutcomes(f, d(61)).RetentionD7 {
		t.Fatalf("retention_d7 counted a day-5 connection")
	}
	// events before the cohort never count in a window
	g := cohortFacts(cohort)
	g.FirstEventAt[EventConnectFirst] = d(-1)
	if ComputeOutcomes(g, d(61)).Connect7d {
		t.Fatalf("connect_7d counted a pre-cohort event")
	}
	// the feedback table stands in for the event
	fb := d(2)
	g.FirstFeedbackAt = &fb
	if !ComputeOutcomes(g, d(61)).Feedback7d {
		t.Fatalf("feedback_7d ignored the feedback table")
	}
}

func TestMaturedDaysAndCohortDay(t *testing.T) {
	cohort := time.Date(2026, 9, 1, 23, 30, 0, 0, time.UTC)
	if got := CohortDay(cohort); got != time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("CohortDay = %s", got)
	}
	if got := MaturedDays(cohort, cohort.Add(-time.Hour)); got != 0 {
		t.Fatalf("MaturedDays before the cohort = %d", got)
	}
	if got := MaturedDays(cohort, cohort.Add(9*24*time.Hour+time.Hour)); got != 9 {
		t.Fatalf("MaturedDays = %d, want 9", got)
	}
}

func TestTrialBackstop(t *testing.T) {
	started := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got := TrialBackstop(started, true, started.Add(TrialLength+TrialOutcomeGrace-time.Hour)); got != "" {
		t.Fatalf("decided %q before the grace passed", got)
	}
	at := started.Add(TrialLength + TrialOutcomeGrace)
	if got := TrialBackstop(started, true, at); got != EventTrialConverted {
		t.Fatalf("pro network = %q", got)
	}
	if got := TrialBackstop(started, false, at); got != EventTrialCancelled {
		t.Fatalf("non-pro network = %q", got)
	}
}

func TestDecideGuardrails(t *testing.T) {
	guardrails := map[string]float64{GuardrailUnsubscribeRate: 0.005, GuardrailComplaintRate: 0.001, GuardrailRefundRate: 0.06}
	// under min exposures: never
	d := DecideGuardrails(guardrails, 2000, GuardrailInput{Exposures: 100, Sent: 100, Unsubscribe: 50})
	if d.Breached {
		t.Fatalf("breached under the volume floor: %+v", d)
	}
	// a tiny denominator: never
	d = DecideGuardrails(guardrails, 0, GuardrailInput{Exposures: 5000, ProStart14d: 3, Refund60d: 3})
	if d.Breached {
		t.Fatalf("breached on a 3-subscription denominator: %+v", d)
	}
	// within
	d = DecideGuardrails(guardrails, 2000, GuardrailInput{Exposures: 5000, Sent: 4000, Delivered: 3900, Unsubscribe: 10, Complaint: 2, ProStart14d: 100, Refund60d: 5})
	if d.Breached {
		t.Fatalf("breached within the thresholds: %+v", d)
	}
	// complaint rate over: names are checked in sorted order, complaint first
	d = DecideGuardrails(guardrails, 2000, GuardrailInput{Exposures: 5000, Sent: 4000, Delivered: 3900, Unsubscribe: 100, Complaint: 20})
	if !d.Breached || d.Guardrail != GuardrailComplaintRate {
		t.Fatalf("expected a complaint breach, got %+v", d)
	}
	// refund rate over
	d = DecideGuardrails(guardrails, 2000, GuardrailInput{Exposures: 5000, ProStart14d: 100, Refund60d: 7})
	if !d.Breached || d.Guardrail != GuardrailRefundRate || d.Rate != 0.07 {
		t.Fatalf("expected a refund breach, got %+v", d)
	}
	// an unknown guardrail name and a zero threshold are ignored
	d = DecideGuardrails(map[string]float64{"weird": 0.1, GuardrailRefundRate: 0}, 0, GuardrailInput{Exposures: 5000, ProStart14d: 100, Refund60d: 90})
	if d.Breached {
		t.Fatalf("unknown or zero guardrail breached: %+v", d)
	}
	if DecideGuardrails(nil, 0, GuardrailInput{}).Breached {
		t.Fatalf("no guardrails breached")
	}
}

func TestEffectiveVariant(t *testing.T) {
	variants := []string{"control", "holdout", "warm"}
	if got := EffectiveVariant("warm", variants, map[string]bool{}); got != "warm" {
		t.Fatalf("unpaused = %q", got)
	}
	if got := EffectiveVariant("warm", variants, map[string]bool{"warm": true}); got != "control" {
		t.Fatalf("paused falls to control, got %q", got)
	}
	// control paused too: the first non-paused in sorted order
	if got := EffectiveVariant("warm", variants, map[string]bool{"warm": true, "control": true}); got != "holdout" {
		t.Fatalf("paused without control = %q", got)
	}
	// no control in the registry
	if got := EffectiveVariant("b", []string{"b", "a"}, map[string]bool{"b": true}); got != "a" {
		t.Fatalf("no control = %q", got)
	}
	// everything paused: off
	if got := EffectiveVariant("a", []string{"a"}, map[string]bool{"a": true}); got != "" {
		t.Fatalf("all paused = %q", got)
	}
}

func TestResultsCursorRoundTrip(t *testing.T) {
	key := ResultsKey{CohortDay: "2026-09-01", Experiment: "offer_screen", Variant: "control", Surface: "offer.in_app", Platform: "ios", Tier: "standard", Path: "A"}
	got, err := DecodeResultsCursor(EncodeResultsCursor(key))
	if err != nil || got != key {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	if k, err := DecodeResultsCursor(""); err != nil || k != (ResultsKey{}) {
		t.Fatalf("empty cursor = %+v, %v", k, err)
	}
	for _, bad := range []string{"!!!", "e30", "bm90IGpzb24"} {
		if _, err := DecodeResultsCursor(bad); err == nil {
			t.Fatalf("accepted cursor %q", bad)
		}
	}
	// ordering follows the tuple
	if !key.Less(ResultsKey{CohortDay: "2026-09-02"}) || key.Less(key) {
		t.Fatalf("Less is not the tuple order")
	}
	if !(ResultsKey{CohortDay: "2026-09-01", Experiment: "a"}).Less(ResultsKey{CohortDay: "2026-09-01", Experiment: "b"}) {
		t.Fatalf("Less ignores the experiment")
	}
}

func TestNextRollupAt(t *testing.T) {
	if got := NextRollupAt(time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)); got != time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC) {
		t.Fatalf("before 02:00 = %s", got)
	}
	if got := NextRollupAt(time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)); got != time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC) {
		t.Fatalf("at 02:00 = %s", got)
	}
	if got := NextRollupAt(time.Date(2026, 9, 1, 23, 59, 0, 0, time.UTC)); got != time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC) {
		t.Fatalf("late evening = %s", got)
	}
	if Rate(1, 0) != 0 || Rate(1, 4) != 0.25 {
		t.Fatalf("Rate")
	}
}
