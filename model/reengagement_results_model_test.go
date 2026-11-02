package model

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/reengagement"
)

// The row's JSON names are a contract: the analysis (mmm/onboarding/analyze.mjs)
// is coded against exactly these, in this order.
func TestReengagementResultsRowJsonContract(t *testing.T) {
	row := &ReengagementResultsRow{
		CohortDay:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		CohortDayStr: "2026-10-01",
		ComputedAt:   time.Date(2026, 11, 2, 2, 45, 0, 0, time.UTC),
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, name.(string))
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"cohort_day", "experiment", "variant", "stream", "platform", "tier",
		"exposures", "virtual_exposures", "sent", "delivered", "landing_clicked",
		"reason_answers", "keeps", "reactivation_7d", "reactivation_30d",
		"retention_90d", "pro_restart_30d", "unsubscribe", "complaint", "hard_bounce",
		"matured_days", "computed_at",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("row JSON names = %v, want %v", names, want)
	}
	if !strings.Contains(string(b), `"cohort_day":"2026-10-01"`) || !strings.Contains(string(b), `"computed_at":"2026-11-02T02:45:00Z"`) {
		t.Fatalf("row JSON dates = %s", b)
	}

	// each outcome lands in its own column
	row.Add(reengagement.EpisodeOutcomes{
		Virtual: true, Sent: true, Delivered: true, LandingClicked: true, ReasonAnswered: true, Kept: true,
		Reactivation7d: true, Reactivation30d: true, Retention90d: true, ProRestart30d: true,
		Unsubscribe: true, Complaint: true, HardBounce: true,
	})
	row.Add(reengagement.EpisodeOutcomes{})
	b, err = json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var counts map[string]any
	if err := json.Unmarshal(b, &counts); err != nil {
		t.Fatal(err)
	}
	for _, name := range want[6:20] {
		expected := float64(1)
		if name == "exposures" {
			expected = 2
		}
		if counts[name] != expected {
			t.Errorf("%s = %v, want %v", name, counts[name], expected)
		}
	}
}

// The cohort of a day is the episodes whose first step, a real or a virtual
// send, fell on it. The episode begins at its start (the start event, else the
// network's row) and its own events end at the next episode's start; its
// connections and paid windows are the network's from the step on.
func TestListReengagementCohortEpisodes(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		// UTC times; the fraction is optional
		at := func(value string) time.Time {
			parsed, err := time.Parse("2006-01-02 15:04:05", value)
			server.Raise(err)
			return parsed.UTC()
		}
		networks := map[int]server.Id{}
		network := func(n int) server.Id {
			if _, ok := networks[n]; !ok {
				networks[n] = server.NewId()
			}
			return networks[n]
		}
		event := func(n int, name string, when string, platform string, experiment string, variant string, props map[string]any) {
			server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{
				NetworkId:  network(n),
				Name:       name,
				At:         at(when),
				Platform:   platform,
				Experiment: experiment,
				Variant:    variant,
				Props:      props,
			}))
		}
		started := func(n int, when string, stream string) {
			event(n, EventReengageEpisodeStarted, when, "", "", "", map[string]any{"stream": stream})
		}
		send := func(n int, name string, when string, platform string, experiment string, variant string, flowStep string) {
			event(n, name, when, platform, experiment, variant, map[string]any{"step": "template", "flow_step": flowStep, "variant": "default"})
		}
		row := func(n int, stream string, startedAt string, country string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_reengagement (network_id, stream, started_at, country) VALUES ($1, $2, $3, $4)`,
					network(n),
					stream,
					at(startedAt),
					country,
				))
			})
		}
		paid := func(n int, subscriptionType string, start string, revenue int64) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO subscription_renewal (network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, market, transaction_id)
						VALUES ($1, $2, $3, $4, $5, 'stripe', $6)
					`,
					network(n),
					subscriptionType,
					at(start),
					at(start).Add(30*24*time.Hour),
					revenue,
					server.NewId().String(),
				))
			})
		}

		// 1: mailed, R1 skipped so R2 is first; Brevo's delivery rounds to the
		// send's second; later R3; reconnects on day 3 and day 70; pays on day 5
		started(1, "2026-10-01 02:30:00.100", reengagement.StreamLapsed)
		send(1, EventReengageEmailSent, "2026-10-01 10:00:00.700", "android", "reengage_lapsed_1", "control", "r2")
		event(1, EventReengageEmailDelivered, "2026-10-01 10:00:00", "android", "reengage_lapsed_1", "control", nil)
		event(1, EventReengageLandingClicked, "2026-10-02 08:00:00", "", "", "", nil)
		send(1, EventReengageEmailSent, "2026-10-31 10:00:00", "android", "reengage_lapsed_1", "control", "r3")
		event(1, EventConnectDay, "2026-10-04 09:00:00", "", "", "", nil)
		event(1, EventConnectDay, "2026-12-10 09:00:00", "", "", "", nil)
		row(1, reengagement.StreamLapsed, "2026-10-01 02:30:00", "US")
		paid(1, SubscriptionTypeSupporter, "2026-10-06 00:00:00", 500_000_000)

		// 2: the holdout: virtual sends only
		started(2, "2026-10-01 02:30:00.200", reengagement.StreamLapsed)
		send(2, EventReengageVirtualSend, "2026-10-01 11:00:00", "ios", "reengage_lapsed_1", "holdout", "r2")
		send(2, EventReengageVirtualSend, "2026-10-31 11:00:00", "ios", "reengage_lapsed_1", "holdout", "r3")
		row(2, reengagement.StreamLapsed, "2026-10-01 02:30:00", "IN")

		// 3: its send on the day is the episode's second step (R1 went a week
		// before): a member of that day's cohort, not this one
		started(3, "2026-09-24 02:30:00", reengagement.StreamLapsed)
		send(3, EventReengageEmailSent, "2026-09-24 10:00:00", "android", "", "", "r1")
		send(3, EventReengageEmailSent, "2026-10-01 10:00:00", "android", "", "", "r2")
		row(3, reengagement.StreamLapsed, "2026-09-24 02:30:00", "US")

		// 4: an episode in March and a new one on the day; the March
		// unsubscribe is not the new episode's; a bounce is; renewals without
		// revenue or of another type are no restart
		started(4, "2026-03-15 02:30:00", reengagement.StreamLapsed)
		send(4, EventReengageEmailSent, "2026-03-15 10:00:00", "web", "", "", "r2")
		event(4, EventReengageEmailUnsubscribed, "2026-03-16 10:00:00", "web", "", "", nil)
		started(4, "2026-10-01 02:30:00", reengagement.StreamWinback)
		send(4, EventReengageEmailSent, "2026-10-01 15:00:00", "web", "", "", "w1")
		event(4, EventReengageEmailBounced, "2026-10-01 15:00:03", "web", "", "", nil)
		row(4, reengagement.StreamWinback, "2026-10-01 02:30:00", "DE")
		paid(4, SubscriptionTypeSupporter, "2026-10-03 00:00:00", 0)
		paid(4, "balance", "2026-10-03 00:00:00", 900)

		// 5: the current episode's start event is missing: its row's start
		// bounds it, past an older episode
		started(5, "2026-02-01 02:30:00", reengagement.StreamLapsed)
		send(5, EventReengageEmailSent, "2026-02-01 10:00:00", "android", "", "", "r2")
		send(5, EventReengageEmailSent, "2026-10-01 12:00:00", "android", "nudge_1", "b", "n1")
		row(5, reengagement.StreamNeverActivated, "2026-10-01 02:30:00", "")

		// 6: a lapsed episode ten days back, answered, then a fix follow-up on
		// the day; the complaint after the fix start is the fix episode's; a
		// connection at the step itself is not after it
		started(6, "2026-09-21 02:30:00", reengagement.StreamLapsed)
		send(6, EventReengageEmailSent, "2026-09-21 10:00:00", "ios", "", "", "r1")
		send(6, EventReengageEmailSent, "2026-09-28 10:00:00", "ios", "", "", "r2")
		event(6, EventReengageReason, "2026-09-28 12:00:00", "", "", "", map[string]any{"reason": "stopped", "flow_step": "r2"})
		started(6, "2026-10-01 02:30:00", reengagement.StreamFix)
		send(6, EventReengageEmailSent, "2026-10-01 09:00:00", "ios", "", "", "f1")
		event(6, EventReengageEmailComplained, "2026-10-02 09:00:00", "ios", "", "", nil)
		event(6, EventConnectDay, "2026-10-01 09:00:00", "", "", "", nil)
		event(6, EventConnectDay, "2026-10-08 09:00:00", "", "", "", nil)
		row(6, reengagement.StreamFix, "2026-10-01 02:30:00", "BR")

		// 7: a replayed virtual step minutes later is still one episode; a keep
		started(7, "2026-10-01 02:30:00", reengagement.StreamLapsed)
		send(7, EventReengageVirtualSend, "2026-10-01 16:00:00", "android", "reengage_lapsed_1", "holdout", "r2")
		send(7, EventReengageVirtualSend, "2026-10-01 16:05:00", "android", "reengage_lapsed_1", "holdout", "r2")
		event(7, EventReengageKeep, "2026-10-01 17:00:00", "", "", "", nil)
		row(7, reengagement.StreamLapsed, "2026-10-01 02:30:00", "US")

		// 8 and 9: first steps on the days around it
		started(8, "2026-09-30 02:30:00", reengagement.StreamLapsed)
		send(8, EventReengageEmailSent, "2026-09-30 23:59:59.999", "android", "", "", "r2")
		started(9, "2026-10-02 00:00:00", reengagement.StreamLapsed)
		send(9, EventReengageEmailSent, "2026-10-02 00:00:00", "android", "", "", "r2")

		// 10: neither the start event nor a row: the stream is left to the
		// caller's fallback (the first step's flow step)
		send(10, EventReengageEmailSent, "2026-10-01 13:00:00", "linux", "", "", "w1")
		event(10, EventConnectDay, "2026-12-01 13:00:00", "", "", "", nil)
		event(10, EventConnectDay, "2026-12-30 13:00:01", "", "", "", nil)

		type want struct {
			n          int
			at         string
			virtual    bool
			experiment string
			variant    string
			stream     string
			flowStep   string
			platform   string
			country    string
			flags      string
			connects   []string
			paid       []string
		}
		flags := func(f reengagement.EpisodeFacts) string {
			out := []string{}
			for _, flag := range []struct {
				on   bool
				name string
			}{
				{f.Sent, "sent"}, {f.Delivered, "delivered"}, {f.LandingClicked, "landing"}, {f.Answered, "answered"},
				{f.Kept, "kept"}, {f.Unsubscribed, "unsubscribed"}, {f.Complained, "complained"}, {f.Bounced, "bounced"},
			} {
				if flag.on {
					out = append(out, flag.name)
				}
			}
			return strings.Join(out, ",")
		}
		check := func(day string, wants []want) {
			episodes := ListReengagementCohortEpisodes(ctx, at(day+" 00:00:00"))
			if len(episodes) != len(wants) {
				for _, e := range episodes {
					t.Logf("episode %s at %s", e.NetworkId, e.Facts.FirstStepAt)
				}
				t.Fatalf("%s: %d episodes, want %d", day, len(episodes), len(wants))
			}
			for i, w := range wants {
				e := episodes[i]
				times := func(values []string) []time.Time {
					out := []time.Time{}
					for _, v := range values {
						out = append(out, at(v))
					}
					return out
				}
				if !e.Facts.FirstStepAt.Equal(at(w.at)) {
					t.Errorf("%s episode %d (network %d): first step at %s, want %s", day, i, w.n, e.Facts.FirstStepAt, w.at)
				}
				got := []any{e.NetworkId, e.Facts.Virtual, e.Experiment, e.Variant, e.Stream, e.FlowStep, e.Platform, e.Country, flags(e.Facts)}
				expected := []any{network(w.n), w.virtual, w.experiment, w.variant, w.stream, w.flowStep, w.platform, w.country, w.flags}
				for j := range got {
					if got[j] != expected[j] {
						t.Errorf("%s episode %d (network %d): field %d = %v, want %v", day, i, w.n, j, got[j], expected[j])
					}
				}
				equalTimes := func(a []time.Time, b []time.Time) bool {
					return slices.EqualFunc(a, b, func(x time.Time, y time.Time) bool { return x.Equal(y) })
				}
				if !equalTimes(e.Facts.ConnectDays, times(w.connects)) {
					t.Errorf("%s network %d: connect days = %v, want %v", day, w.n, e.Facts.ConnectDays, w.connects)
				}
				if !equalTimes(e.Facts.PaidStarts, times(w.paid)) {
					t.Errorf("%s network %d: paid starts = %v, want %v", day, w.n, e.Facts.PaidStarts, w.paid)
				}
			}
		}

		check("2026-10-01", []want{
			{n: 6, at: "2026-10-01 09:00:00", stream: reengagement.StreamFix, flowStep: "f1", platform: "ios", country: "BR",
				flags: "sent,complained", connects: []string{"2026-10-01 09:00:00", "2026-10-08 09:00:00"}},
			{n: 1, at: "2026-10-01 10:00:00.700", experiment: "reengage_lapsed_1", variant: "control", stream: reengagement.StreamLapsed,
				flowStep: "r2", platform: "android", country: "US", flags: "sent,delivered,landing",
				connects: []string{"2026-10-04 09:00:00", "2026-12-10 09:00:00"}, paid: []string{"2026-10-06 00:00:00"}},
			{n: 2, at: "2026-10-01 11:00:00", virtual: true, experiment: "reengage_lapsed_1", variant: "holdout", stream: reengagement.StreamLapsed,
				flowStep: "r2", platform: "ios", country: "IN"},
			{n: 5, at: "2026-10-01 12:00:00", experiment: "nudge_1", variant: "b", stream: reengagement.StreamNeverActivated,
				flowStep: "n1", platform: "android", flags: "sent"},
			{n: 10, at: "2026-10-01 13:00:00", flowStep: "w1", platform: "linux", flags: "sent",
				connects: []string{"2026-12-01 13:00:00"}},
			{n: 4, at: "2026-10-01 15:00:00", stream: reengagement.StreamWinback, flowStep: "w1", platform: "web", country: "DE", flags: "sent,bounced"},
			{n: 7, at: "2026-10-01 16:00:00", virtual: true, experiment: "reengage_lapsed_1", variant: "holdout", stream: reengagement.StreamLapsed,
				flowStep: "r2", platform: "android", country: "US", flags: "kept"},
		})
		// the lapsed episode before the fix follow-up ends at the fix start:
		// its answer is its own, the later complaint is not
		check("2026-09-21", []want{
			{n: 6, at: "2026-09-21 10:00:00", stream: reengagement.StreamLapsed, flowStep: "r1", platform: "ios", country: "BR",
				flags: "sent,answered", connects: []string{"2026-10-01 09:00:00", "2026-10-08 09:00:00"}},
		})
		check("2026-09-24", []want{
			{n: 3, at: "2026-09-24 10:00:00", stream: reengagement.StreamLapsed, flowStep: "r1", platform: "android", country: "US", flags: "sent"},
		})
		check("2026-03-15", []want{
			{n: 4, at: "2026-03-15 10:00:00", stream: reengagement.StreamLapsed, flowStep: "r2", platform: "web", country: "DE", flags: "sent,unsubscribed"},
		})
		check("2026-09-30", []want{
			{n: 8, at: "2026-09-30 23:59:59.999", stream: reengagement.StreamLapsed, flowStep: "r2", platform: "android", flags: "sent"},
		})
		check("2026-09-29", []want{})
	})
}
