package model

// The re-engagement results (mmm/reengagement/PLAN.md "MEASUREMENT"): the
// nightly aggregate the experiment readout reads, reengagement_results_daily,
// and the cohort episodes it is computed from, loaded one cohort day at a
// time. An episode belongs to the UTC day of its first step, a real send or a
// virtual (holdout) send; the windows over its facts are
// reengagement/results.go's.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/reengagement"
)

// ----- results rows -----

// ReengagementResultsRow is one row of reengagement_results_daily: the
// dimension tuple and the counts. Counts are episodes, never events: a network
// counts once per episode, in the cohort day of the episode's first step. The
// JSON names are the contract the analysis (mmm/onboarding/analyze.mjs) reads.
type ReengagementResultsRow struct {
	CohortDay    time.Time `json:"-"`
	CohortDayStr string    `json:"cohort_day"`
	Experiment   string    `json:"experiment"`
	Variant      string    `json:"variant"`
	Stream       string    `json:"stream"`
	Platform     string    `json:"platform"`
	Tier         string    `json:"tier"`
	Exposures    int       `json:"exposures"`
	// VirtualExposures are the exposures whose first step was a virtual send:
	// the holdout
	VirtualExposures int `json:"virtual_exposures"`
	Sent             int `json:"sent"`
	Delivered        int `json:"delivered"`
	LandingClicked   int `json:"landing_clicked"`
	ReasonAnswers    int `json:"reason_answers"`
	Keeps            int `json:"keeps"`
	Reactivation7d   int `json:"reactivation_7d"`
	Reactivation30d  int `json:"reactivation_30d"`
	Retention90d     int `json:"retention_90d"`
	ProRestart30d    int `json:"pro_restart_30d"`
	Unsubscribe      int `json:"unsubscribe"`
	Complaint        int `json:"complaint"`
	HardBounce       int `json:"hard_bounce"`
	// MaturedDays is the cohort day's age in whole days at rollup time: a
	// window of W days has closed for every episode of the day once this is
	// more than W (the first steps fall anywhere in the day)
	MaturedDays int       `json:"matured_days"`
	ComputedAt  time.Time `json:"computed_at"`
}

// Key is the ordering tuple, the admin endpoint's keyset cursor.
func (r *ReengagementResultsRow) Key() reengagement.ResultsKey {
	return reengagement.ResultsKey{
		CohortDay:  r.CohortDayStr,
		Experiment: r.Experiment,
		Variant:    r.Variant,
		Stream:     r.Stream,
		Platform:   r.Platform,
		Tier:       r.Tier,
	}
}

// Add adds one episode's outcomes to the row.
func (r *ReengagementResultsRow) Add(o reengagement.EpisodeOutcomes) {
	r.Exposures += 1
	inc := func(v bool, n *int) {
		if v {
			*n += 1
		}
	}
	inc(o.Virtual, &r.VirtualExposures)
	inc(o.Sent, &r.Sent)
	inc(o.Delivered, &r.Delivered)
	inc(o.LandingClicked, &r.LandingClicked)
	inc(o.ReasonAnswered, &r.ReasonAnswers)
	inc(o.Kept, &r.Keeps)
	inc(o.Reactivation7d, &r.Reactivation7d)
	inc(o.Reactivation30d, &r.Reactivation30d)
	inc(o.Retention90d, &r.Retention90d)
	inc(o.ProRestart30d, &r.ProRestart30d)
	inc(o.Unsubscribe, &r.Unsubscribe)
	inc(o.Complaint, &r.Complaint)
	inc(o.HardBounce, &r.HardBounce)
}

// ReplaceReengagementResults replaces every row whose cohort day is in
// [from, to] with `rows`, in one transaction, so a recompute never leaves a
// dimension combination behind that no longer exists.
func ReplaceReengagementResults(ctx context.Context, from time.Time, to time.Time, rows []*ReengagementResultsRow, computedAt time.Time) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", r)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM reengagement_results_daily WHERE cohort_day >= $1 AND cohort_day <= $2`,
			onboarding.CohortDay(from),
			onboarding.CohortDay(to),
		))
		for _, r := range rows {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO reengagement_results_daily (
						cohort_day, experiment, variant, stream, platform, tier,
						exposures, virtual_exposures, sent, delivered, landing_clicked,
						reason_answers, keeps, reactivation_7d, reactivation_30d,
						retention_90d, pro_restart_30d, unsubscribe, complaint, hard_bounce,
						matured_days, computed_at
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)
					ON CONFLICT (cohort_day, experiment, variant, stream, platform, tier) DO UPDATE
					SET
						exposures = $7, virtual_exposures = $8, sent = $9, delivered = $10, landing_clicked = $11,
						reason_answers = $12, keeps = $13, reactivation_7d = $14, reactivation_30d = $15,
						retention_90d = $16, pro_restart_30d = $17, unsubscribe = $18, complaint = $19, hard_bounce = $20,
						matured_days = $21, computed_at = $22
				`,
				r.CohortDay, r.Experiment, r.Variant, r.Stream, r.Platform, r.Tier,
				r.Exposures, r.VirtualExposures, r.Sent, r.Delivered, r.LandingClicked,
				r.ReasonAnswers, r.Keeps, r.Reactivation7d, r.Reactivation30d,
				r.Retention90d, r.ProRestart30d, r.Unsubscribe, r.Complaint, r.HardBounce,
				r.MaturedDays, computedAt,
			))
		}
	})
	return
}

// ReengagementResultsFilter selects rows for the admin endpoint.
type ReengagementResultsFilter struct {
	// Experiment is one registry experiment, or "" for every row, the
	// episodes no experiment covered (experiment "") included
	Experiment string
	From       time.Time
	To         time.Time
	Stream     string
	Platform   string
	Tier       string
	// After is the keyset cursor: rows strictly after this key in
	// (cohort_day, experiment, variant, stream, platform, tier) order.
	After *reengagement.ResultsKey
	// MinExposures is the volume floor, held on the totals the readout sums:
	// an experiment's variant with fewer exposures than this over every row
	// the filter selects (cohort days and dimensions; the cursor aside) has
	// none of its rows returned, and one at or over it has all of them.
	MinExposures int
	Limit        int
}

const reengagementResultsColumns = `
	cohort_day, experiment, variant, stream, platform, tier,
	exposures, virtual_exposures, sent, delivered, landing_clicked,
	reason_answers, keeps, reactivation_7d, reactivation_30d,
	retention_90d, pro_restart_30d, unsubscribe, complaint, hard_bounce,
	matured_days, computed_at
`

func scanReengagementResultsRow(result pgx.Rows) *ReengagementResultsRow {
	r := &ReengagementResultsRow{}
	server.Raise(result.Scan(
		&r.CohortDay, &r.Experiment, &r.Variant, &r.Stream, &r.Platform, &r.Tier,
		&r.Exposures, &r.VirtualExposures, &r.Sent, &r.Delivered, &r.LandingClicked,
		&r.ReasonAnswers, &r.Keeps, &r.Reactivation7d, &r.Reactivation30d,
		&r.Retention90d, &r.ProRestart30d, &r.Unsubscribe, &r.Complaint, &r.HardBounce,
		&r.MaturedDays, &r.ComputedAt,
	))
	r.CohortDayStr = r.CohortDay.UTC().Format(cohortDayLayout)
	return r
}

// ListReengagementResults pages the aggregate. The volume floor
// (filter.MinExposures) holds on each experiment's variant totals over the
// selected rows, the sums the readout compares, never on a single row: the
// arms are made of a day's platform and tier cells of a few exposures each,
// and a per-row floor dropped most of them (the 2026-11-13 review: 159 of 272
// control and 162 of 250 holdout exposures of reengage_lapsed_1, every
// matured reactivation among them). The totals are taken before the cursor,
// so every page applies the same floor. Returns the rows and, when more
// remain, the key of the last row returned (the caller encodes it).
func ListReengagementResults(ctx context.Context, filter ReengagementResultsFilter) (rows []*ReengagementResultsRow, next *reengagement.ResultsKey) {
	limit := filter.Limit
	if limit <= 0 || 1000 < limit {
		limit = 500
	}
	rows = []*ReengagementResultsRow{}
	server.Db(ctx, func(conn server.PgConn) {
		args := []any{onboarding.CohortDay(filter.From), onboarding.CohortDay(filter.To)}
		where := []string{"cohort_day >= $1", "cohort_day <= $2"}
		add := func(column string, value string) {
			if value == "" {
				return
			}
			args = append(args, value)
			where = append(where, column+" = $"+itoa(len(args)))
		}
		add("experiment", filter.Experiment)
		add("stream", filter.Stream)
		add("platform", filter.Platform)
		add("tier", filter.Tier)
		args = append(args, filter.MinExposures)
		outer := []string{"$" + itoa(len(args)) + " <= selected.arm_exposures"}
		if filter.After != nil {
			cohortDay, err := time.Parse(cohortDayLayout, filter.After.CohortDay)
			server.Raise(err)
			args = append(args, cohortDay, filter.After.Experiment, filter.After.Variant, filter.After.Stream, filter.After.Platform, filter.After.Tier)
			n := len(args)
			outer = append(outer, "(cohort_day, experiment, variant, stream, platform, tier) > ($"+itoa(n-5)+", $"+itoa(n-4)+", $"+itoa(n-3)+", $"+itoa(n-2)+", $"+itoa(n-1)+", $"+itoa(n)+")")
		}
		args = append(args, limit+1)
		query := "SELECT " + reengagementResultsColumns +
			" FROM (" +
			"SELECT *, SUM(exposures) OVER (PARTITION BY experiment, variant) AS arm_exposures" +
			" FROM reengagement_results_daily" +
			" WHERE " + strings.Join(where, " AND ") +
			") selected" +
			" WHERE " + strings.Join(outer, " AND ") +
			" ORDER BY cohort_day, experiment, variant, stream, platform, tier" +
			" LIMIT $" + itoa(len(args))
		result, err := conn.Query(ctx, query, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanReengagementResultsRow(result))
			}
		})
	})
	if limit < len(rows) {
		rows = rows[:limit]
		key := rows[len(rows)-1].Key()
		next = &key
	}
	return
}

// ----- cohort episodes -----

// ReengagementCohortEpisode is one episode of a cohort day: its dimensions and
// its facts.
type ReengagementCohortEpisode struct {
	NetworkId server.Id
	// the first step's experiment and variant, which the step copies from
	// network_reengagement's experiment_id and variant ("" when no experiment
	// covered the episode). The step's copy, not the row: a later episode
	// reuses the row.
	Experiment string
	Variant    string
	// the stream of the episode's start ("" when it was not recorded)
	Stream string
	// the first step's flow step (r1, r2, n1, f1, w1)
	FlowStep string
	// the platform the step was sent with, and the network's country from its
	// network_reengagement row ("" when unknown)
	Platform string
	Country  string
	Facts    reengagement.EpisodeFacts
}

// reengagementCohortSql loads the episodes whose first step fell in [$1, $2)
// with their facts, in one statement bounded by the cohort day: the day's sends
// come from the (name, at) index, and every other read is one network's range
// of the (network_id, at) index (the row comparisons keep the planner on it, as
// in insertConnectDayInTx), its network_reengagement row, or its
// subscription_renewal rows by start.
//
// An episode begins at its start: the network's latest reengage.episode.started
// at or before the send, or its network_reengagement row's started_at when that
// is later (the current episode, should its event be missing). A send is the
// episode's first step when no other send of the network falls between that
// start and it. The episode's own events run from the first step, less the
// webhook's one-second rounding, to the next episode's start.
const reengagementCohortSql = `
	WITH step AS MATERIALIZED (
		SELECT
			s.network_id,
			s.at,
			s.name = $4 AS is_virtual,
			s.experiment,
			s.variant,
			s.platform,
			COALESCE(s.props->>'flow_step', '') AS flow_step
		FROM network_onboarding_event s
		WHERE s.name IN ($3, $4) AND s.at >= $1 AND s.at < $2
	),
	episode AS MATERIALIZED (
		SELECT
			step.*,
			COALESCE(started.stream, '') AS stream
		FROM step
		LEFT JOIN LATERAL (
			SELECT starts.at, starts.stream
			FROM (
				(
					SELECT e.at, e.props->>'stream' AS stream
					FROM network_onboarding_event e
					WHERE
						e.network_id = step.network_id AND
						(e.network_id, e.at) <= (step.network_id, step.at) AND
						e.name = $5
					ORDER BY e.network_id DESC, e.at DESC
					LIMIT 1
				)
				UNION ALL
				(
					SELECT current_episode.started_at, current_episode.stream
					FROM network_reengagement current_episode
					WHERE current_episode.network_id = step.network_id AND current_episode.started_at <= step.at
				)
			) starts
			ORDER BY starts.at DESC
			LIMIT 1
		) started ON true
		WHERE NOT EXISTS (
			SELECT 1
			FROM network_onboarding_event earlier
			WHERE
				earlier.network_id = step.network_id AND
				(earlier.network_id, earlier.at) >= (step.network_id, COALESCE(started.at, '-infinity'::timestamp)) AND
				(earlier.network_id, earlier.at) < (step.network_id, step.at) AND
				earlier.name IN ($3, $4)
		)
	)
	SELECT
		episode.network_id,
		episode.at,
		episode.is_virtual,
		episode.experiment,
		episode.variant,
		episode.stream,
		episode.flow_step,
		episode.platform,
		COALESCE(state.country, '') AS country,
		facts.sent,
		facts.delivered,
		facts.landing_clicked,
		facts.answered,
		facts.kept,
		facts.unsubscribed,
		facts.complained,
		facts.bounced,
		COALESCE(connects.days, '{}') AS connect_days,
		COALESCE(paid.starts, '{}') AS paid_starts
	FROM episode
	LEFT JOIN network_reengagement state ON state.network_id = episode.network_id
	LEFT JOIN LATERAL (
		SELECT MIN(later.at) AS at
		FROM (
			(
				SELECT e.at
				FROM network_onboarding_event e
				WHERE
					e.network_id = episode.network_id AND
					(e.network_id, e.at) > (episode.network_id, episode.at) AND
					e.name = $5
				ORDER BY e.network_id, e.at
				LIMIT 1
			)
			UNION ALL
			(
				SELECT current_episode.started_at
				FROM network_reengagement current_episode
				WHERE current_episode.network_id = episode.network_id AND episode.at < current_episode.started_at
			)
		) later
	) ended ON true
	CROSS JOIN LATERAL (
		SELECT
			COALESCE(BOOL_OR(e.name = $3), false) AS sent,
			COALESCE(BOOL_OR(e.name = $6), false) AS delivered,
			COALESCE(BOOL_OR(e.name = $7), false) AS landing_clicked,
			COALESCE(BOOL_OR(e.name = $8), false) AS answered,
			COALESCE(BOOL_OR(e.name = $9), false) AS kept,
			COALESCE(BOOL_OR(e.name = $10), false) AS unsubscribed,
			COALESCE(BOOL_OR(e.name = $11), false) AS complained,
			COALESCE(BOOL_OR(e.name = $12), false) AS bounced
		FROM network_onboarding_event e
		WHERE
			e.network_id = episode.network_id AND
			(e.network_id, e.at) >= (episode.network_id, episode.at - make_interval(secs => $14)) AND
			(e.network_id, e.at) < (episode.network_id, COALESCE(ended.at, 'infinity'::timestamp)) AND
			e.name IN ($3, $6, $7, $8, $9, $10, $11, $12)
	) facts
	CROSS JOIN LATERAL (
		SELECT array_agg(e.at ORDER BY e.at) AS days
		FROM network_onboarding_event e
		WHERE
			e.network_id = episode.network_id AND
			(e.network_id, e.at) >= (episode.network_id, episode.at) AND
			(e.network_id, e.at) <= (episode.network_id, episode.at + make_interval(secs => $15)) AND
			e.name = $13
	) connects
	CROSS JOIN LATERAL (
		SELECT array_agg(renewal.start_time ORDER BY renewal.start_time) AS starts
		FROM subscription_renewal renewal
		WHERE
			renewal.network_id = episode.network_id AND
			renewal.subscription_type = $17 AND
			renewal.start_time >= episode.at AND
			renewal.start_time <= episode.at + make_interval(secs => $16) AND
			0 < renewal.net_revenue_nano_cents
	) paid
	ORDER BY episode.at, episode.network_id
`

// ListReengagementCohortEpisodes is every episode whose first step fell on the
// UTC day `cohortDay`, with its facts, oldest first.
func ListReengagementCohortEpisodes(ctx context.Context, cohortDay time.Time) (episodes []*ReengagementCohortEpisode) {
	day := onboarding.CohortDay(cohortDay)
	episodes = []*ReengagementCohortEpisode{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			reengagementCohortSql,
			day,
			day.Add(24*time.Hour),
			EventReengageEmailSent,
			EventReengageVirtualSend,
			EventReengageEpisodeStarted,
			EventReengageEmailDelivered,
			EventReengageLandingClicked,
			EventReengageReason,
			EventReengageKeep,
			EventReengageEmailUnsubscribed,
			EventReengageEmailComplained,
			EventReengageEmailBounced,
			EventConnectDay,
			reengagement.ResultsEventSlack.Seconds(),
			reengagement.ResultsConnectHorizon.Seconds(),
			reengagement.ResultsPaidHorizon.Seconds(),
			SubscriptionTypeSupporter,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				e := &ReengagementCohortEpisode{}
				f := &e.Facts
				server.Raise(result.Scan(
					&e.NetworkId,
					&f.FirstStepAt,
					&f.Virtual,
					&e.Experiment,
					&e.Variant,
					&e.Stream,
					&e.FlowStep,
					&e.Platform,
					&e.Country,
					&f.Sent,
					&f.Delivered,
					&f.LandingClicked,
					&f.Answered,
					&f.Kept,
					&f.Unsubscribed,
					&f.Complained,
					&f.Bounced,
					&f.ConnectDays,
					&f.PaidStarts,
				))
				episodes = append(episodes, e)
			}
		})
	})
	return
}
