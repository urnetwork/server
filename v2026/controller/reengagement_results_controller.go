// SPDX-License-Identifier: MPL-2.0

package controller

// The results side of the re-engagement program (mmm/reengagement/PLAN.md
// "MEASUREMENT"):
//
//   - the nightly ReengagementResultsRollup task (02:45 UTC, after the 02:30
//     scan): reengagement_results_daily recomputed over the last
//     `reengagement.results.rollup_days` cohort days, so the windows (up to 90
//     days after an episode's first step) mature in place. One statement and
//     one transaction per cohort day.
//   - RunReengagementResultsRollup, shared with `bringyourctl reengagement
//     rollup` for a backfill
//   - GET /admin/reengagement/results behind the onboarding admin bearers, with
//     the volume floor, keyset paging and envelope of
//     GET /admin/onboarding/results
//
// The rollup only reads the program's events and writes the aggregate, so it
// runs whether or not reengagement.yml enables the program: with no episodes it
// writes no rows.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/reengagement"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// ----- metrics -----

var reengagementResultsRollupTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "results_rollup_total",
	Help:      "Nightly re-engagement results rollup runs by result: ok, failed.",
}, []string{"result"})

var reengagementResultsRollupDurationSeconds = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "results_rollup_duration_seconds",
	Help:      "How long the last re-engagement results rollup ran, ok or failed.",
})

var reengagementResultsRows = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "results_rows",
	Help:      "Rows of reengagement_results_daily written by the last completed rollup.",
})

var reengagementResultsEpisodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "results_episodes",
	Help:      "Episodes in the cohort window the last completed rollup recomputed, by stream and first send: real, virtual (the holdout).",
}, []string{"stream", "send"})

func init() {
	prometheus.MustRegister(
		reengagementResultsRollupTotal,
		reengagementResultsRollupDurationSeconds,
		reengagementResultsRows,
		reengagementResultsEpisodes,
	)
}

// The first send labels of urnetwork_reengagement_results_episodes.
const (
	reengagementResultsSendReal    = "real"
	reengagementResultsSendVirtual = "virtual"
)

// ----- the nightly rollup task -----

type ReengagementResultsRollupArgs struct {
	// From and To bound the cohort days to recompute (inclusive). Zero means
	// the configured window ending yesterday.
	From time.Time `json:"from,omitempty"`
	To   time.Time `json:"to,omitempty"`
}

type ReengagementResultsRollupResult struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// the cohort days recomputed, and their episodes and rows
	Days     int `json:"days"`
	Episodes int `json:"episodes"`
	Rows     int `json:"rows"`
	// episodes per stream, and how many of them were virtual (the holdout)
	EpisodesByStream        map[string]int `json:"episodes_by_stream"`
	VirtualEpisodesByStream map[string]int `json:"virtual_episodes_by_stream"`
	DurationSeconds         float64        `json:"duration_seconds"`
}

const reengagementResultsRollupTaskKey = "reengagement_results_rollup"

// One statement per cohort day, each bounded by the day's sends: minutes at
// the program's daily caps, seconds at its expected volume.
const reengagementResultsRollupMaxTime = 30 * time.Minute

// ScheduleReengagementResultsRollup schedules the nightly task once.
func ScheduleReengagementResultsRollup(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ReengagementResultsRollup,
		&ReengagementResultsRollupArgs{},
		clientSession,
		task.RunOnce(reengagementResultsRollupTaskKey),
		task.RunAt(at),
		task.MaxTime(reengagementResultsRollupMaxTime),
	)
}

// ReengagementResultsRollup is the nightly task body. Idempotent: each cohort
// day's rows are replaced in one transaction.
func ReengagementResultsRollup(
	args *ReengagementResultsRollupArgs,
	clientSession *session.ClientSession,
) (result *ReengagementResultsRollupResult, returnErr error) {
	if args == nil {
		args = &ReengagementResultsRollupArgs{}
	}
	result, returnErr = RunReengagementResultsRollup(clientSession.Ctx, args.From, args.To)
	if returnErr != nil {
		reengagementResultsRollupTotal.WithLabelValues("failed").Inc()
	} else {
		reengagementResultsRollupTotal.WithLabelValues("ok").Inc()
	}
	return
}

func ReengagementResultsRollupPost(
	args *ReengagementResultsRollupArgs,
	result *ReengagementResultsRollupResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleReengagementResultsRollup(clientSession, tx, reengagement.NextResultsRollupAt(server.NowUtc()))
	return nil
}

// reengagementResultsWindow is the cohort days a rollup recomputes: [from, to]
// as given, where a zero `to` is yesterday (today's sends are still going out)
// and a zero `from` begins the `days` days that end at `to`.
func reengagementResultsWindow(now time.Time, from time.Time, to time.Time, days int) (time.Time, time.Time, error) {
	if to.IsZero() {
		to = onboarding.CohortDay(now).Add(-24 * time.Hour)
	}
	if from.IsZero() {
		from = onboarding.CohortDay(to).Add(-time.Duration(days-1) * 24 * time.Hour)
	}
	from = onboarding.CohortDay(from)
	to = onboarding.CohortDay(to)
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("rollup window is empty: from %s after to %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	return from, to, nil
}

// RunReengagementResultsRollup recomputes the cohort days [from, to] (zero
// bounds mean the configured window ending yesterday), one day at a time:
// load the day's episodes and facts, aggregate, replace the day's rows. A day
// that fails leaves the days before it written; the next run rewrites them
// all. Shared by the task and `bringyourctl reengagement rollup`.
func RunReengagementResultsRollup(ctx context.Context, from time.Time, to time.Time) (result *ReengagementResultsRollupResult, returnErr error) {
	start := server.NowUtc()
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("re-engagement results rollup: %v", r)
			glog.Errorf("[reengagement]results rollup failed: %v\n", r)
		}
		reengagementResultsRollupDurationSeconds.Set(server.NowUtc().Sub(start).Seconds())
	}()
	now := start
	from, to, err := reengagementResultsWindow(now, from, to, model.Reengagement().EffectiveResultsRollupDays())
	if err != nil {
		return nil, err
	}
	result = &ReengagementResultsRollupResult{
		From:                    from,
		To:                      to,
		EpisodesByStream:        map[string]int{},
		VirtualEpisodesByStream: map[string]int{},
	}
	for day := from; !day.After(to); day = day.Add(24 * time.Hour) {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("re-engagement results rollup stopped before %s: %w", day.Format("2006-01-02"), err)
		}
		episodes := model.ListReengagementCohortEpisodes(ctx, day)
		rows := buildReengagementResultsRows(day, episodes, now)
		if err := model.ReplaceReengagementResults(ctx, day, day, rows, now); err != nil {
			return result, err
		}
		result.Days += 1
		result.Episodes += len(episodes)
		result.Rows += len(rows)
		for _, r := range rows {
			result.EpisodesByStream[r.Stream] += r.Exposures
			result.VirtualEpisodesByStream[r.Stream] += r.VirtualExposures
		}
	}
	setReengagementResultsGauges(result)
	result.DurationSeconds = server.NowUtc().Sub(start).Seconds()
	glog.Infof("[reengagement]results rollup %s..%s: %d days, %d episodes, %d rows in %.1fs\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"),
		result.Days, result.Episodes, result.Rows, result.DurationSeconds)
	return result, nil
}

var reengagementResultsGaugeLock sync.Mutex

// setReengagementResultsGauges replaces the window's gauges with a completed
// run's, every stream present (known zeroes) so a quiet stream reads 0, not
// its last value.
func setReengagementResultsGauges(result *ReengagementResultsRollupResult) {
	reengagementResultsGaugeLock.Lock()
	defer reengagementResultsGaugeLock.Unlock()
	reengagementResultsRows.Set(float64(result.Rows))
	reengagementResultsEpisodes.Reset()
	streams := []string{reengagement.StreamLapsed, reengagement.StreamNeverActivated, reengagement.StreamFix, reengagement.StreamWinback}
	for stream := range result.EpisodesByStream {
		if !slices.Contains(streams, stream) {
			streams = append(streams, stream)
		}
	}
	for _, stream := range streams {
		virtual := result.VirtualEpisodesByStream[stream]
		reengagementResultsEpisodes.WithLabelValues(stream, reengagementResultsSendReal).Set(float64(result.EpisodesByStream[stream] - virtual))
		reengagementResultsEpisodes.WithLabelValues(stream, reengagementResultsSendVirtual).Set(float64(virtual))
	}
}

// buildReengagementResultsRows aggregates one cohort day's episodes into
// result rows. The dimensions: the first step's experiment and variant (""
// when no experiment covered the episode); the episode's stream, else the one
// its first step opens; the step's platform; the price tier of the network's
// country, as onboarding's rows. A dimension nothing recorded is `unknown`.
func buildReengagementResultsRows(cohortDay time.Time, episodes []*model.ReengagementCohortEpisode, now time.Time) []*model.ReengagementResultsRow {
	cohortDay = onboarding.CohortDay(cohortDay)
	byKey := map[reengagement.ResultsKey]*model.ReengagementResultsRow{}
	for _, e := range episodes {
		stream := e.Stream
		if stream == "" {
			stream = reengagement.FirstStepStream(e.FlowStep)
		}
		if stream == "" {
			stream = reengagement.ResultsUnknown
		}
		platform := e.Platform
		if platform == "" {
			platform = reengagement.ResultsUnknown
		}
		tier := reengagement.ResultsUnknown
		if e.Country != "" {
			tier = model.Pro().PriceTierForCountry(e.Country).Name
		}
		r := &model.ReengagementResultsRow{
			CohortDay:    cohortDay,
			CohortDayStr: cohortDay.Format("2006-01-02"),
			Experiment:   e.Experiment,
			Variant:      e.Variant,
			Stream:       stream,
			Platform:     platform,
			Tier:         tier,
			MaturedDays:  onboarding.MaturedDays(cohortDay, now),
			ComputedAt:   now,
		}
		key := r.Key()
		if existing, ok := byKey[key]; ok {
			r = existing
		} else {
			byKey[key] = r
		}
		r.Add(reengagement.ComputeEpisodeOutcomes(e.Facts, now))
	}
	rows := make([]*model.ReengagementResultsRow, 0, len(byKey))
	for _, r := range byKey {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Key().Less(rows[j].Key())
	})
	return rows
}

// ----- admin endpoint -----

// AdminReengagementResultsArgs are the query parameters of
// GET /admin/reengagement/results.
type AdminReengagementResultsArgs struct {
	// one registry experiment; empty returns every row, the episodes no
	// experiment covered included (their experiment and variant are "")
	Experiment string
	From       string
	To         string
	Stream     string
	Platform   string
	Tier       string
	Cursor     string
	Limit      int
}

// AdminReengagementResultsResult is ReengagementResultsPage in the spec, the
// envelope of GET /admin/onboarding/results.
type AdminReengagementResultsResult struct {
	Rows       []*model.ReengagementResultsRow `json:"rows"`
	NextCursor *string                         `json:"next_cursor"`
	// MinExposures is the volume floor applied to this page: the least
	// exposures an experiment's variant has in total over the request's
	// cohort days and filters for its rows to be returned.
	MinExposures int `json:"min_exposures"`
}

// AdminReengagementResults pages reengagement_results_daily. The admin
// bearers, the volume floor (onboarding.yml `results.min_exposures`) and the
// paging are GET /admin/onboarding/results's.
func AdminReengagementResults(args *AdminReengagementResultsArgs, clientSession *session.ClientSession) (*AdminReengagementResultsResult, error) {
	const endpoint = "reengagement_results"
	if err := requireOnboardingAdmin(clientSession, endpoint); err != nil {
		return nil, err
	}
	bad := func(err error) (*AdminReengagementResultsResult, error) {
		onboardingAdminRequestsTotal.WithLabelValues(endpoint, "bad_request").Inc()
		return nil, err
	}
	if args == nil {
		return bad(fmt.Errorf("%d from and to are required.", http.StatusBadRequest))
	}
	from, err := parseCohortDay(args.From, "from")
	if err != nil {
		return bad(err)
	}
	to, err := parseCohortDay(args.To, "to")
	if err != nil {
		return bad(err)
	}
	if to.Before(from) {
		return bad(fmt.Errorf("%d to is before from.", http.StatusBadRequest))
	}
	filter := model.ReengagementResultsFilter{
		Experiment:   strings.TrimSpace(args.Experiment),
		From:         from,
		To:           to,
		Stream:       strings.TrimSpace(args.Stream),
		Platform:     strings.TrimSpace(args.Platform),
		Tier:         strings.TrimSpace(args.Tier),
		MinExposures: model.Onboarding().EffectiveMinExposures(),
		Limit:        args.Limit,
	}
	if cursor := strings.TrimSpace(args.Cursor); cursor != "" {
		key, err := reengagement.DecodeResultsCursor(cursor)
		if err != nil {
			return bad(fmt.Errorf("%d cursor is not valid.", http.StatusBadRequest))
		}
		filter.After = &key
	}
	rows, next := model.ListReengagementResults(clientSession.Ctx, filter)
	result := &AdminReengagementResultsResult{
		Rows:         rows,
		MinExposures: filter.MinExposures,
	}
	if next != nil {
		cursor := reengagement.EncodeResultsCursor(*next)
		result.NextCursor = &cursor
	}
	onboardingAdminRequestsTotal.WithLabelValues(endpoint, "ok").Inc()
	return result, nil
}
