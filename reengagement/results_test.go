// SPDX-License-Identifier: MPL-2.0

package reengagement

import (
	"testing"
	"time"
)

// Each window is open at the first step and closed at its end: an event at the
// step itself is not "after" it, one exactly at the end counts, and a window
// that has not closed at rollup time counts nothing.
func TestComputeEpisodeOutcomesWindows(t *testing.T) {
	step := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d := func(days float64) time.Time { return step.Add(time.Duration(days * 24 * float64(time.Hour))) }
	matured := d(91)

	for _, c := range []struct {
		name    string
		connect []time.Time
		paid    []time.Time
		want    EpisodeOutcomes
	}{
		{"nothing after the step", nil, nil, EpisodeOutcomes{}},
		{"a connection at the step itself", []time.Time{step}, nil, EpisodeOutcomes{}},
		{"a connection a second after the step", []time.Time{step.Add(time.Second)}, nil,
			EpisodeOutcomes{Reactivation7d: true, Reactivation30d: true}},
		{"a connection exactly 7 days after", []time.Time{d(7)}, nil,
			EpisodeOutcomes{Reactivation7d: true, Reactivation30d: true}},
		{"a connection just past 7 days", []time.Time{d(7).Add(time.Second)}, nil,
			EpisodeOutcomes{Reactivation30d: true}},
		{"a connection exactly 30 days after", []time.Time{d(30)}, nil,
			EpisodeOutcomes{Reactivation30d: true}},
		{"a connection on day 31", []time.Time{d(30).Add(time.Second)}, nil, EpisodeOutcomes{}},
		{"a connection exactly 60 days after is day 60", []time.Time{d(60)}, nil, EpisodeOutcomes{}},
		{"a connection on day 61", []time.Time{d(60).Add(time.Second)}, nil, EpisodeOutcomes{Retention90d: true}},
		{"a connection exactly 90 days after", []time.Time{d(90)}, nil, EpisodeOutcomes{Retention90d: true}},
		{"a connection past 90 days", []time.Time{d(90).Add(time.Second)}, nil, EpisodeOutcomes{}},
		{"a paid window at the step itself", nil, []time.Time{step}, EpisodeOutcomes{}},
		{"a paid window 30 days after", nil, []time.Time{d(30)}, EpisodeOutcomes{ProRestart30d: true}},
		{"a paid window past 30 days", nil, []time.Time{d(30).Add(time.Second)}, EpisodeOutcomes{}},
	} {
		got := ComputeEpisodeOutcomes(EpisodeFacts{FirstStepAt: step, ConnectDays: c.connect, PaidStarts: c.paid}, matured)
		if got != c.want {
			t.Errorf("%s: outcomes = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// The windows close one by one; the episode's own events never wait.
func TestComputeEpisodeOutcomesMaturity(t *testing.T) {
	step := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	d := func(days float64) time.Time { return step.Add(time.Duration(days * 24 * float64(time.Hour))) }
	f := EpisodeFacts{
		FirstStepAt:    step,
		Sent:           true,
		Delivered:      true,
		LandingClicked: true,
		Answered:       true,
		Kept:           true,
		Unsubscribed:   true,
		Complained:     true,
		Bounced:        true,
		ConnectDays:    []time.Time{d(2), d(75)},
		PaidStarts:     []time.Time{d(10)},
	}
	always := EpisodeOutcomes{
		Sent: true, Delivered: true, LandingClicked: true, ReasonAnswered: true, Kept: true,
		Unsubscribe: true, Complaint: true, HardBounce: true,
	}
	for _, c := range []struct {
		now  time.Time
		want func(EpisodeOutcomes) EpisodeOutcomes
	}{
		{step, func(o EpisodeOutcomes) EpisodeOutcomes { return o }},
		{d(7).Add(-time.Second), func(o EpisodeOutcomes) EpisodeOutcomes { return o }},
		{d(7), func(o EpisodeOutcomes) EpisodeOutcomes { o.Reactivation7d = true; return o }},
		{d(30), func(o EpisodeOutcomes) EpisodeOutcomes {
			o.Reactivation7d, o.Reactivation30d, o.ProRestart30d = true, true, true
			return o
		}},
		{d(90).Add(-time.Second), func(o EpisodeOutcomes) EpisodeOutcomes {
			o.Reactivation7d, o.Reactivation30d, o.ProRestart30d = true, true, true
			return o
		}},
		{d(90), func(o EpisodeOutcomes) EpisodeOutcomes {
			o.Reactivation7d, o.Reactivation30d, o.ProRestart30d, o.Retention90d = true, true, true, true
			return o
		}},
	} {
		want := c.want(always)
		if got := ComputeEpisodeOutcomes(f, c.now); got != want {
			t.Errorf("at %s: outcomes = %+v, want %+v", c.now.Sub(step), got, want)
		}
	}

	// the holdout's first step was virtual; it is reported as such
	if !ComputeEpisodeOutcomes(EpisodeFacts{FirstStepAt: step, Virtual: true}, d(1)).Virtual {
		t.Fatalf("a virtual first step lost its arm")
	}
}

// The model loads facts within the horizons; they must cover every window.
func TestResultsHorizonsCoverTheWindows(t *testing.T) {
	for _, window := range []time.Duration{WindowReactivation7d, WindowReactivation30d, WindowRetention90dTo} {
		if ResultsConnectHorizon < window {
			t.Errorf("connection horizon %s is shorter than a %s window", ResultsConnectHorizon, window)
		}
	}
	if ResultsPaidHorizon < WindowProRestart30d {
		t.Errorf("paid horizon %s is shorter than the pro restart window", ResultsPaidHorizon)
	}
	if WindowRetention90dTo-WindowRetention90dFrom != 30*24*time.Hour {
		t.Errorf("retention_90d covers days 61 through 90")
	}
}

func TestFirstStepStream(t *testing.T) {
	for step, want := range map[string]string{
		StepR1: StreamLapsed,
		StepR2: StreamLapsed,
		StepN1: StreamNeverActivated,
		StepF1: StreamFix,
		StepW1: StreamWinback,
		StepR3: "",
		"":     "",
		"e1":   "",
	} {
		if got := FirstStepStream(step); got != want {
			t.Errorf("FirstStepStream(%q) = %q, want %q", step, got, want)
		}
	}
}

func TestResultsCursorRoundTrip(t *testing.T) {
	key := ResultsKey{CohortDay: "2026-10-01", Experiment: "reengage_lapsed_1", Variant: "holdout", Stream: StreamLapsed, Platform: "android", Tier: "standard"}
	got, err := DecodeResultsCursor(EncodeResultsCursor(key))
	if err != nil || got != key {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// the episodes no experiment covered have an empty experiment and variant
	empty := ResultsKey{CohortDay: "2026-10-01", Stream: StreamWinback, Platform: "web", Tier: "unknown"}
	if got, err := DecodeResultsCursor(EncodeResultsCursor(empty)); err != nil || got != empty {
		t.Fatalf("round trip without an experiment = %+v, %v", got, err)
	}
	if k, err := DecodeResultsCursor(""); err != nil || k != (ResultsKey{}) {
		t.Fatalf("empty cursor = %+v, %v", k, err)
	}
	for _, bad := range []string{
		"!!!",
		"e30",         // {}
		"bm90IGpzb24", // not json
		EncodeResultsCursor(ResultsKey{CohortDay: "2026-13-01"}),
		EncodeResultsCursor(ResultsKey{CohortDay: "yesterday", Experiment: "x"}),
	} {
		if _, err := DecodeResultsCursor(bad); err == nil {
			t.Fatalf("accepted cursor %q", bad)
		}
	}
	// ordering follows the tuple, the empty experiment first
	if !key.Less(ResultsKey{CohortDay: "2026-10-02"}) || key.Less(key) {
		t.Fatalf("Less is not the tuple order")
	}
	if !empty.Less(key) {
		t.Fatalf("Less does not put the empty experiment first")
	}
	a := ResultsKey{CohortDay: "2026-10-01", Experiment: "e", Variant: "v", Stream: StreamFix, Platform: "ios", Tier: "regional"}
	for _, b := range []ResultsKey{
		{CohortDay: "2026-10-01", Experiment: "e", Variant: "v", Stream: StreamLapsed, Platform: "android", Tier: ""},
		{CohortDay: "2026-10-01", Experiment: "e", Variant: "v", Stream: StreamFix, Platform: "linux", Tier: ""},
		{CohortDay: "2026-10-01", Experiment: "e", Variant: "v", Stream: StreamFix, Platform: "ios", Tier: "standard"},
	} {
		if !a.Less(b) || b.Less(a) {
			t.Fatalf("Less(%+v, %+v) is not the tuple order", a, b)
		}
	}
}

func TestNextResultsRollupAt(t *testing.T) {
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{time.Date(2026, 10, 1, 2, 30, 0, 0, time.UTC), time.Date(2026, 10, 1, 2, 45, 0, 0, time.UTC)},
		{time.Date(2026, 10, 1, 2, 45, 0, 0, time.UTC), time.Date(2026, 10, 2, 2, 45, 0, 0, time.UTC)},
		{time.Date(2026, 10, 1, 23, 59, 0, 0, time.UTC), time.Date(2026, 10, 2, 2, 45, 0, 0, time.UTC)},
		// another zone's clock is read as UTC: 18:00 PDT is 01:00 UTC the next day
		{time.Date(2026, 10, 1, 18, 0, 0, 0, time.FixedZone("PDT", -7*3600)), time.Date(2026, 10, 2, 2, 45, 0, 0, time.UTC)},
	} {
		if got := NextResultsRollupAt(c.now); !got.Equal(c.want) {
			t.Errorf("NextResultsRollupAt(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}
