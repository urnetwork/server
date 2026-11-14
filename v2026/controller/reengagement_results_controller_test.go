// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

func TestReengagementResultsWindow(t *testing.T) {
	now := time.Date(2026, 11, 2, 2, 45, 0, 0, time.UTC)
	from, to, err := reengagementResultsWindow(now, time.Time{}, time.Time{}, 120)
	if err != nil {
		t.Fatal(err)
	}
	// 120 cohort days ending yesterday: today's sends are still going out
	if got := from.Format("2006-01-02") + ".." + to.Format("2006-01-02"); got != "2026-07-05..2026-11-01" {
		t.Fatalf("default window = %s", got)
	}
	// explicit bounds are whole UTC days
	from, to, err = reengagementResultsWindow(now, time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC), time.Date(2026, 9, 3, 1, 0, 0, 0, time.UTC), 120)
	if err != nil || from.Format(time.RFC3339) != "2026-09-01T00:00:00Z" || to.Format(time.RFC3339) != "2026-09-03T00:00:00Z" {
		t.Fatalf("explicit window = %s..%s, %v", from, to, err)
	}
	// only a `to`: the window ends there
	from, _, err = reengagementResultsWindow(now, time.Time{}, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), 30)
	if err != nil || from.Format("2006-01-02") != "2026-09-01" {
		t.Fatalf("window ending 09-30 = %s, %v", from, err)
	}
	if _, _, err := reengagementResultsWindow(now, time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), 120); err == nil {
		t.Fatalf("an empty window was accepted")
	}
}

// Episodes aggregate by (experiment, variant, stream, platform, tier); a
// dimension nothing recorded is `unknown`, an episode with no recorded stream
// takes the one its first step opens, and a window that has not closed counts
// nothing.
func TestBuildReengagementResultsRows(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := day.Add(10*24*time.Hour + 3*time.Hour)
	step := day.Add(10 * time.Hour)
	episode := func(experiment string, variant string, stream string, flowStep string, platform string, country string, facts reengagement.EpisodeFacts) *model.ReengagementCohortEpisode {
		facts.FirstStepAt = step
		return &model.ReengagementCohortEpisode{
			NetworkId:  server.NewId(),
			Experiment: experiment,
			Variant:    variant,
			Stream:     stream,
			FlowStep:   flowStep,
			Platform:   platform,
			Country:    country,
			Facts:      facts,
		}
	}
	reconnected := []time.Time{step.Add(2 * 24 * time.Hour)}
	rows := buildReengagementResultsRows(day.Add(5*time.Hour), []*model.ReengagementCohortEpisode{
		episode("reengage_lapsed_1", "control", reengagement.StreamLapsed, "r2", "android", "US",
			reengagement.EpisodeFacts{Sent: true, Delivered: true, ConnectDays: reconnected}),
		episode("reengage_lapsed_1", "control", reengagement.StreamLapsed, "r1", "android", "US",
			reengagement.EpisodeFacts{Sent: true, Unsubscribed: true}),
		episode("reengage_lapsed_1", "holdout", reengagement.StreamLapsed, "r2", "android", "US",
			reengagement.EpisodeFacts{Virtual: true, ConnectDays: reconnected}),
		// no stream recorded: the first step's (w1 opens a win-back)
		episode("", "", "", "w1", "", "", reengagement.EpisodeFacts{Sent: true, Bounced: true}),
		// nothing to go on
		episode("", "", "", "", "web", "", reengagement.EpisodeFacts{Sent: true}),
	}, now)

	type summary struct {
		key       reengagement.ResultsKey
		exposures int
		virtual   int
		sent      int
		delivered int
		react7d   int
		react30d  int
		unsub     int
		bounce    int
		matured   int
	}
	got := []summary{}
	for _, r := range rows {
		got = append(got, summary{r.Key(), r.Exposures, r.VirtualExposures, r.Sent, r.Delivered, r.Reactivation7d, r.Reactivation30d, r.Unsubscribe, r.HardBounce, r.MaturedDays})
		if !r.ComputedAt.Equal(now) || !r.CohortDay.Equal(day) {
			t.Errorf("row %+v: computed at %s for %s", r.Key(), r.ComputedAt, r.CohortDay)
		}
	}
	standard := model.Pro().PriceTierForCountry("US").Name
	want := []summary{
		{reengagement.ResultsKey{CohortDay: "2026-10-01", Stream: reengagement.ResultsUnknown, Platform: "web", Tier: reengagement.ResultsUnknown},
			1, 0, 1, 0, 0, 0, 0, 0, 10},
		{reengagement.ResultsKey{CohortDay: "2026-10-01", Stream: reengagement.StreamWinback, Platform: reengagement.ResultsUnknown, Tier: reengagement.ResultsUnknown},
			1, 0, 1, 0, 0, 0, 0, 1, 10},
		// day 10: the 7-day windows have closed, the 30-day ones have not
		{reengagement.ResultsKey{CohortDay: "2026-10-01", Experiment: "reengage_lapsed_1", Variant: "control", Stream: reengagement.StreamLapsed, Platform: "android", Tier: standard},
			2, 0, 2, 1, 1, 0, 1, 0, 10},
		{reengagement.ResultsKey{CohortDay: "2026-10-01", Experiment: "reengage_lapsed_1", Variant: "holdout", Stream: reengagement.StreamLapsed, Platform: "android", Tier: standard},
			1, 1, 0, 0, 1, 0, 0, 0, 10},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(buildReengagementResultsRows(day, nil, now)) != 0 {
		t.Fatalf("an empty cohort day has rows")
	}
}

// The episode gauges carry every stream, a quiet one at zero, split into the
// real and the virtual (holdout) first sends.
func TestSetReengagementResultsGauges(t *testing.T) {
	setReengagementResultsGauges(&ReengagementResultsRollupResult{
		Rows:                    3,
		EpisodesByStream:        map[string]int{reengagement.StreamLapsed: 10, reengagement.ResultsUnknown: 1},
		VirtualEpisodesByStream: map[string]int{reengagement.StreamLapsed: 4},
	})
	for _, c := range []struct {
		stream string
		send   string
		want   float64
	}{
		{reengagement.StreamLapsed, reengagementResultsSendReal, 6},
		{reengagement.StreamLapsed, reengagementResultsSendVirtual, 4},
		{reengagement.StreamWinback, reengagementResultsSendReal, 0},
		{reengagement.StreamFix, reengagementResultsSendVirtual, 0},
		{reengagement.ResultsUnknown, reengagementResultsSendReal, 1},
	} {
		if got := testutil.ToFloat64(reengagementResultsEpisodes.WithLabelValues(c.stream, c.send)); got != c.want {
			t.Errorf("episodes{%s,%s} = %v, want %v", c.stream, c.send, got, c.want)
		}
	}
	if got := testutil.ToFloat64(reengagementResultsRows); got != 3 {
		t.Errorf("rows gauge = %v", got)
	}
}

// reengagementResultsSeed writes one episode's events: its start and its first
// step, plus the given events after it.
type reengagementResultsSeed struct {
	ctx context.Context
}

func (s reengagementResultsSeed) event(networkId server.Id, name string, at time.Time, experiment string, variant string, props map[string]any) {
	server.Raise(model.AddOnboardingEvent(s.ctx, &model.OnboardingEvent{
		NetworkId:  networkId,
		Name:       name,
		At:         at,
		Platform:   "android",
		Experiment: experiment,
		Variant:    variant,
		Props:      props,
	}))
}

func (s reengagementResultsSeed) episode(networkId server.Id, stream string, step time.Time, virtual bool, experiment string, variant string) {
	s.event(networkId, model.EventReengageEpisodeStarted, step.Add(-6*time.Hour), "", "", map[string]any{"stream": stream})
	name := model.EventReengageEmailSent
	if virtual {
		name = model.EventReengageVirtualSend
	}
	s.event(networkId, name, step, experiment, variant, map[string]any{"step": "r2_reason", "flow_step": "r2", "variant": "lapsed"})
	server.Tx(s.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			s.ctx,
			`
				INSERT INTO network_reengagement (network_id, stream, started_at, platform, country, experiment_id, variant)
				VALUES ($1, $2, $3, 'android', 'US', $4, $5)
			`,
			networkId,
			stream,
			step.Add(-6*time.Hour),
			experiment,
			variant,
		))
	})
}

// The rollup recomputes each cohort day of its window from the events: a day
// old enough has every window closed, a recent one only its short windows; a
// rerun replaces the day's rows, dropping a combination that is gone; the task
// recurs at 02:45 UTC.
func TestReengagementResultsRollupRecomputesTheWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		seed := reengagementResultsSeed{ctx: ctx}
		today := server.NowUtc().Truncate(24 * time.Hour)
		old := today.Add(-100 * 24 * time.Hour)
		recent := today.Add(-10 * 24 * time.Hour)
		day := 24 * time.Hour

		// the old day: two mailed episodes and one holdout
		mailed := server.NewId()
		seed.episode(mailed, reengagement.StreamLapsed, old.Add(10*time.Hour), false, "reengage_lapsed_1", "control")
		seed.event(mailed, model.EventReengageEmailDelivered, old.Add(10*time.Hour+time.Minute), "reengage_lapsed_1", "control", nil)
		seed.event(mailed, model.EventConnectDay, old.Add(3*day+time.Hour), "", "", nil)
		seed.event(mailed, model.EventConnectDay, old.Add(75*day), "", "", nil)
		quiet := server.NewId()
		seed.episode(quiet, reengagement.StreamLapsed, old.Add(11*time.Hour), false, "reengage_lapsed_1", "control")
		seed.event(quiet, model.EventReengageEmailUnsubscribed, old.Add(12*time.Hour), "reengage_lapsed_1", "control", nil)
		holdout := server.NewId()
		seed.episode(holdout, reengagement.StreamLapsed, old.Add(12*time.Hour), true, "reengage_lapsed_1", "holdout")
		seed.event(holdout, model.EventConnectDay, old.Add(20*day), "", "", nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO subscription_renewal (network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, market, transaction_id)
					VALUES ($1, 'supporter', $2, $3, 500000000, 'stripe', 'synthetic-reengagement-restart')
				`,
				holdout,
				old.Add(15*day),
				old.Add(45*day),
			))
		})

		// the recent day: the 7-day window has closed, a connection on day 20
		// (still ahead) cannot have counted, and neither has a 30-day window
		recentNetwork := server.NewId()
		seed.episode(recentNetwork, reengagement.StreamWinback, recent.Add(9*time.Hour), false, "", "")
		seed.event(recentNetwork, model.EventConnectDay, recent.Add(2*day), "", "", nil)
		seed.event(recentNetwork, model.EventConnectDay, recent.Add(20*day), "", "", nil)

		result, err := RunReengagementResultsRollup(ctx, old, recent)
		if err != nil {
			t.Fatal(err)
		}
		if result.Days != 91 || result.Episodes != 4 || result.Rows != 3 ||
			result.EpisodesByStream[reengagement.StreamLapsed] != 3 || result.VirtualEpisodesByStream[reengagement.StreamLapsed] != 1 ||
			result.EpisodesByStream[reengagement.StreamWinback] != 1 {
			t.Fatalf("rollup result = %+v", result)
		}

		list := func() []*model.ReengagementResultsRow {
			rows, next := model.ListReengagementResults(ctx, model.ReengagementResultsFilter{From: old, To: today})
			if next != nil {
				t.Fatalf("one page expected")
			}
			return rows
		}
		type counts struct {
			exposures, virtual, sent, delivered, react7d, react30d, retention, restart, unsub, matured int
		}
		summarize := func(r *model.ReengagementResultsRow) counts {
			return counts{r.Exposures, r.VirtualExposures, r.Sent, r.Delivered, r.Reactivation7d, r.Reactivation30d, r.Retention90d, r.ProRestart30d, r.Unsubscribe, r.MaturedDays}
		}
		rows := list()
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3", len(rows))
		}
		standard := model.Pro().PriceTierForCountry("US").Name
		for i, want := range []struct {
			key    reengagement.ResultsKey
			counts counts
		}{
			{reengagement.ResultsKey{CohortDay: old.Format("2006-01-02"), Experiment: "reengage_lapsed_1", Variant: "control", Stream: reengagement.StreamLapsed, Platform: "android", Tier: standard},
				counts{2, 0, 2, 1, 1, 1, 1, 0, 1, 100}},
			{reengagement.ResultsKey{CohortDay: old.Format("2006-01-02"), Experiment: "reengage_lapsed_1", Variant: "holdout", Stream: reengagement.StreamLapsed, Platform: "android", Tier: standard},
				counts{1, 1, 0, 0, 0, 1, 0, 1, 0, 100}},
			{reengagement.ResultsKey{CohortDay: recent.Format("2006-01-02"), Stream: reengagement.StreamWinback, Platform: "android", Tier: standard},
				counts{1, 0, 1, 0, 1, 0, 0, 0, 0, 10}},
		} {
			if rows[i].Key() != want.key || summarize(rows[i]) != want.counts {
				t.Errorf("row %d = %+v %+v, want %+v %+v", i, rows[i].Key(), summarize(rows[i]), want.key, want.counts)
			}
		}

		// a rerun replaces the rows: the holdout's events are gone, so its
		// combination is too
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_onboarding_event WHERE network_id = $1`, holdout))
		})
		if _, err := RunReengagementResultsRollup(ctx, old, old); err != nil {
			t.Fatal(err)
		}
		rows = list()
		if len(rows) != 2 || rows[0].Variant != "control" || rows[0].Exposures != 2 || rows[1].Stream != reengagement.StreamWinback {
			t.Fatalf("rows after the rerun = %+v", rows)
		}

		// the nightly task: the same rollup, and its successor at 02:45 UTC
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		ok := testutil.ToFloat64(reengagementResultsRollupTotal.WithLabelValues("ok"))
		taskResult := runControllerAuditTask(t, owner, ReengagementResultsRollup, ReengagementResultsRollupPost,
			&ReengagementResultsRollupArgs{From: recent, To: recent})
		if taskResult.Days != 1 || taskResult.Episodes != 1 || taskResult.Rows != 1 {
			t.Fatalf("task result = %+v", taskResult)
		}
		if testutil.ToFloat64(reengagementResultsRollupTotal.WithLabelValues("ok")) != ok+1 {
			t.Fatalf("the task's run was not counted")
		}
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT run_at FROM pending_task WHERE function_name = $1`,
				task.NewTaskTarget(ReengagementResultsRollup).TargetFunctionName(),
			).Scan(&runAt))
		})
		if runAt.UTC().Hour() != 2 || runAt.UTC().Minute() != 45 {
			t.Fatalf("the next rollup runs at %s, want 02:45 UTC", runAt)
		}

		failed := testutil.ToFloat64(reengagementResultsRollupTotal.WithLabelValues("failed"))
		if _, err := ReengagementResultsRollup(&ReengagementResultsRollupArgs{From: recent, To: old}, owner); err == nil {
			t.Fatalf("an empty window was acknowledged")
		}
		if testutil.ToFloat64(reengagementResultsRollupTotal.WithLabelValues("failed")) != failed+1 {
			t.Fatalf("the failed run was not counted")
		}
	})
}

// GET /admin/reengagement/results: the onboarding admin bearers, the volume
// floor and keyset paging of GET /admin/onboarding/results; every row when no
// experiment is named.
func TestAdminReengagementResults(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		previousBearers := onboardingAdminBearers
		onboardingAdminBearers = func() []string { return []string{"synthetic-reengagement-admin"} }
		defer func() { onboardingAdminBearers = previousBearers }()

		day1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		day2 := day1.Add(24 * time.Hour)
		computedAt := time.Date(2026, 11, 2, 2, 45, 0, 0, time.UTC)
		row := func(day time.Time, experiment string, variant string, stream string, platform string, exposures int) *model.ReengagementResultsRow {
			return &model.ReengagementResultsRow{
				CohortDay: day, CohortDayStr: day.Format("2006-01-02"),
				Experiment: experiment, Variant: variant, Stream: stream, Platform: platform, Tier: "standard",
				Exposures: exposures, Sent: exposures, MaturedDays: 32, ComputedAt: computedAt,
			}
		}
		for day, rows := range map[time.Time][]*model.ReengagementResultsRow{
			day1: {
				row(day1, "reengage_lapsed_1", "control", reengagement.StreamLapsed, "android", 40),
				row(day1, "reengage_lapsed_1", "holdout", reengagement.StreamLapsed, "android", 38),
				row(day1, "reengage_lapsed_1", "control", reengagement.StreamLapsed, "ios", 3),
				row(day1, "", "", reengagement.StreamWinback, "web", 12),
				// an arm of 4 + 5 = 9 exposures, under the floor in total
				row(day1, "reengage_winback_1", "offer", reengagement.StreamWinback, "android", 4),
			},
			day2: {
				row(day2, "reengage_lapsed_1", "control", reengagement.StreamLapsed, "android", 41),
				row(day2, "reengage_lapsed_1", "holdout", reengagement.StreamLapsed, "android", 39),
				row(day2, "reengage_winback_1", "offer", reengagement.StreamWinback, "android", 5),
			},
		} {
			if err := model.ReplaceReengagementResults(ctx, day, day, rows, computedAt); err != nil {
				t.Fatal(err)
			}
		}

		request := func(authorization string, args *AdminReengagementResultsArgs) (*AdminReengagementResultsResult, error) {
			clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
			defer clientSession.Cancel()
			if authorization != "" {
				clientSession.Header["Authorization"] = []string{authorization}
			}
			return AdminReengagementResults(args, clientSession)
		}
		counted := func(result string) float64 {
			return testutil.ToFloat64(onboardingAdminRequestsTotal.WithLabelValues("reengagement_results", result))
		}
		window := &AdminReengagementResultsArgs{Experiment: "reengage_lapsed_1", From: "2026-10-01", To: "2026-10-02"}
		const bearer = "Bearer synthetic-reengagement-admin"

		// the admin bearer, compared exactly; a network token is told apart
		for _, c := range []struct {
			authorization string
			status        string
			result        string
		}{
			{"", "401", "unauthorized"},
			{"Bearer synthetic-reengagement-admin-not", "401", "unauthorized"},
			{"Bearer eyJhbGciOi.eyJuZXR3b3Jr.c2lnbmF0dXJl", "403", "forbidden"},
		} {
			before := counted(c.result)
			if _, err := request(c.authorization, window); err == nil || !strings.HasPrefix(err.Error(), c.status) {
				t.Fatalf("authorization %q: err = %v, want %s", c.authorization, err, c.status)
			}
			if counted(c.result) != before+1 {
				t.Fatalf("authorization %q was not counted as %s", c.authorization, c.result)
			}
		}

		// bad requests
		for _, args := range []*AdminReengagementResultsArgs{
			nil,
			{Experiment: "reengage_lapsed_1", From: "2026-10-01"},
			{Experiment: "reengage_lapsed_1", From: "10/01/2026", To: "2026-10-02"},
			{Experiment: "reengage_lapsed_1", From: "2026-10-02", To: "2026-10-01"},
			{Experiment: "reengage_lapsed_1", From: "2026-10-01", To: "2026-10-02", Cursor: "not a cursor"},
		} {
			before := counted("bad_request")
			if _, err := request(bearer, args); err == nil || !strings.HasPrefix(err.Error(), "400") {
				t.Fatalf("args %+v: err = %v, want 400", args, err)
			}
			if counted("bad_request") != before+1 {
				t.Fatalf("args %+v was not counted as a bad request", args)
			}
		}

		// one experiment, two rows a page. The volume floor holds on each
		// variant's total over the window, so the 3-exposure ios row of the
		// control arm (84 exposures in all) is returned with the rest
		floor := model.Onboarding().EffectiveMinExposures()
		if 9 >= floor || floor > 12 {
			t.Fatalf("the fixture assumes a volume floor in (9, 12], got %d", floor)
		}
		seen := []string{}
		args := *window
		args.Limit = 2
		for page := 0; ; page += 1 {
			before := counted("ok")
			result, err := request(bearer, &args)
			if err != nil {
				t.Fatal(err)
			}
			if counted("ok") != before+1 || result.MinExposures != floor {
				t.Fatalf("page %d: not counted, or floor %d", page, result.MinExposures)
			}
			for _, r := range result.Rows {
				seen = append(seen, r.CohortDayStr+"/"+r.Variant+"/"+r.Platform)
				if r.Experiment != "reengage_lapsed_1" || !r.ComputedAt.Equal(computedAt) {
					t.Fatalf("page %d: row %+v", page, r)
				}
			}
			if result.NextCursor == nil {
				break
			}
			if len(result.Rows) != 2 || 3 < page {
				t.Fatalf("page %d: %d rows with a next cursor", page, len(result.Rows))
			}
			args.Cursor = *result.NextCursor
		}
		want := "2026-10-01/control/android 2026-10-01/control/ios 2026-10-01/holdout/android 2026-10-02/control/android 2026-10-02/holdout/android"
		if got := strings.Join(seen, " "); got != want {
			t.Fatalf("paged rows = %s, want %s", got, want)
		}

		// every experiment, the rows without one included, but not the
		// winback offer arm (4 exposures on the day); and the filters
		result, err := request(bearer, &AdminReengagementResultsArgs{From: "2026-10-01", To: "2026-10-01"})
		if err != nil || len(result.Rows) != 4 || result.Rows[0].Experiment != "" || result.Rows[0].Stream != reengagement.StreamWinback || result.NextCursor != nil {
			t.Fatalf("every experiment = %+v, %v", result, err)
		}
		for _, r := range result.Rows {
			if r.Experiment == "reengage_winback_1" {
				t.Fatalf("an arm under the floor was returned: %+v", r)
			}
		}
		result, err = request(bearer, &AdminReengagementResultsArgs{Experiment: "reengage_winback_1", From: "2026-10-01", To: "2026-10-02"})
		if err != nil || len(result.Rows) != 0 {
			t.Fatalf("an arm of 9 exposures = %+v, %v, want no rows", result, err)
		}
		result, err = request(bearer, &AdminReengagementResultsArgs{From: "2026-10-01", To: "2026-10-02", Stream: reengagement.StreamWinback, Platform: "web", Tier: "standard"})
		if err != nil || len(result.Rows) != 1 || result.Rows[0].Exposures != 12 {
			t.Fatalf("filtered = %+v, %v", result, err)
		}
		result, err = request(bearer, &AdminReengagementResultsArgs{Experiment: "unknown_experiment", From: "2026-10-01", To: "2026-10-02"})
		if err != nil || result.Rows == nil || len(result.Rows) != 0 || result.NextCursor != nil {
			t.Fatalf("unknown experiment = %+v, %v", result, err)
		}
	})
}
