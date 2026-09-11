package model

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// OnboardingEmailTrackerRow is one privacy-safe send cohort. Every count is a
// distinct network with the outcome, not a count of webhook retries or client
// events. The raw network and message identifiers never leave the source
// tables.
type OnboardingEmailTrackerRow struct {
	SendDay              time.Time `json:"-"`
	SendDayStr           string    `json:"send_day"`
	Step                 string    `json:"step"`
	Template             string    `json:"template"`
	Variant              string    `json:"variant"`
	Experiment           string    `json:"experiment"`
	ExperimentVariant    string    `json:"experiment_variant"`
	Platform             string    `json:"platform"`
	Path                 string    `json:"path"`
	Sent                 int64     `json:"sent"`
	Delivered            int64     `json:"delivered"`
	Opened               int64     `json:"opened"`
	Clicked              int64     `json:"clicked"`
	LandingClicked       int64     `json:"landing_clicked"`
	AppOpened            int64     `json:"app_opened"`
	Connected            int64     `json:"connected"`
	WidgetAdded          int64     `json:"widget_added"`
	FeedbackSubmitted    int64     `json:"feedback_submitted"`
	ProStarted           int64     `json:"pro_started"`
	Engaged              int64     `json:"engaged"`
	Bounced              int64     `json:"bounced"`
	Unsubscribed         int64     `json:"unsubscribed"`
	Complained           int64     `json:"complained"`
	AttributionAmbiguous int64     `json:"attribution_ambiguous"`
	ComputedAt           time.Time `json:"computed_at"`
}

func (r *OnboardingEmailTrackerRow) Key() onboarding.EmailTrackerKey {
	return onboarding.EmailTrackerKey{
		SendDay: r.SendDayStr, Step: r.Step, Template: r.Template,
		Variant: r.Variant, Experiment: r.Experiment,
		ExperimentVariant: r.ExperimentVariant, Platform: r.Platform, Path: r.Path,
	}
}

// OnboardingEmailTrackerTotals is the bounded Prometheus projection of the
// detailed daily rows.
type OnboardingEmailTrackerTotals struct {
	Step                 string
	Sent                 int64
	Delivered            int64
	Opened               int64
	Clicked              int64
	LandingClicked       int64
	AppOpened            int64
	Connected            int64
	WidgetAdded          int64
	FeedbackSubmitted    int64
	ProStarted           int64
	Engaged              int64
	Bounced              int64
	Unsubscribed         int64
	Complained           int64
	AttributionAmbiguous int64
}

func (t *OnboardingEmailTrackerTotals) Outcomes() map[string]int64 {
	return map[string]int64{
		onboarding.EmailOutcomeSent:                 t.Sent,
		onboarding.EmailOutcomeDelivered:            t.Delivered,
		onboarding.EmailOutcomeOpened:               t.Opened,
		onboarding.EmailOutcomeClicked:              t.Clicked,
		onboarding.EmailOutcomeLandingClicked:       t.LandingClicked,
		onboarding.EmailOutcomeAppOpened:            t.AppOpened,
		onboarding.EmailOutcomeConnected:            t.Connected,
		onboarding.EmailOutcomeWidgetAdded:          t.WidgetAdded,
		onboarding.EmailOutcomeFeedbackSubmitted:    t.FeedbackSubmitted,
		onboarding.EmailOutcomeProStarted:           t.ProStarted,
		onboarding.EmailOutcomeEngaged:              t.Engaged,
		onboarding.EmailOutcomeBounced:              t.Bounced,
		onboarding.EmailOutcomeUnsubscribed:         t.Unsubscribed,
		onboarding.EmailOutcomeComplained:           t.Complained,
		onboarding.EmailOutcomeAttributionAmbiguous: t.AttributionAmbiguous,
	}
}

// onboardingEmailTrackerRebuildSql attributes each event to a send before it
// aggregates. New events carry the exact flow_step. For old events, a template
// is accepted only when that network used it at one flow step; the shared
// E3/E5 template is counted as attribution_ambiguous rather than guessed.
// Product outcomes belong to the most recent email until the next send, with a
// 14-day ceiling. Delivery events retain a 30-day correction window.
const onboardingEmailTrackerRebuildSql = `
	WITH candidate_sends AS MATERIALIZED (
		SELECT
			email.message_id, email.network_id, email.step, email.template,
			email.variant, email.experiment, email.experiment_variant,
			email.sent_at
		FROM network_onboarding_email AS email
		WHERE email.sent_at >= $1 AND email.sent_at < $2
			AND email.step IN ('e1', 'e2', 'e3', 'e4', 'e5')
			AND NOT EXISTS (
				SELECT 1
				FROM network_onboarding_email AS earlier
				WHERE earlier.network_id = email.network_id
					AND earlier.step = email.step
					AND (earlier.sent_at, earlier.message_id) < (email.sent_at, email.message_id)
			)
	),
	sequenced AS MATERIALIZED (
		SELECT
			email.*,
			(
				SELECT later.sent_at
				FROM network_onboarding_email AS later
				WHERE later.network_id = email.network_id
					AND later.step <> email.step
					AND (later.sent_at, later.step) > (email.sent_at, email.step)
				ORDER BY later.sent_at, later.step
				LIMIT 1
			) AS next_sent_at,
			EXISTS (
				SELECT 1
				FROM network_onboarding_email AS shared
				WHERE shared.network_id = email.network_id
					AND shared.template = email.template
					AND shared.step <> email.step
			) AS template_ambiguous
		FROM candidate_sends AS email
	),
	sends AS MATERIALIZED (
		SELECT
			email.*,
			COALESCE(campaign.platform, '') AS platform,
			COALESCE(campaign.path, '') AS path,
			LEAST(
				COALESCE(email.next_sent_at, email.sent_at + INTERVAL '14 days'),
				email.sent_at + INTERVAL '14 days'
			) AS engagement_ends_at,
			LEAST(
				COALESCE(email.next_sent_at, email.sent_at + INTERVAL '30 days'),
				email.sent_at + INTERVAL '30 days'
			) AS legacy_attribution_ends_at
		FROM sequenced AS email
		LEFT JOIN network_onboarding AS campaign USING (network_id)
	),
	classified AS (
		SELECT
			sends.*,
			events.delivered,
			events.opened,
			events.clicked,
			events.landing_clicked,
			events.app_opened,
			events.connected,
			events.widget_added,
			(events.feedback_submitted OR EXISTS (
				SELECT 1 FROM account_feedback AS feedback
				WHERE feedback.network_id = sends.network_id
					AND feedback.feedback_time >= sends.sent_at
					AND feedback.feedback_time < sends.engagement_ends_at
			)) AS feedback_submitted,
			(events.pro_started OR EXISTS (
				SELECT 1 FROM subscription_renewal AS renewal
				WHERE renewal.network_id = sends.network_id
					AND renewal.start_time >= sends.sent_at
					AND renewal.start_time < sends.engagement_ends_at
			)) AS pro_started,
			events.bounced,
			events.unsubscribed,
			events.complained,
			events.attribution_ambiguous
		FROM sends
		CROSS JOIN LATERAL (
			SELECT
				COALESCE(BOOL_OR(event.name = 'email.delivered' AND event.step_attributed), false) AS delivered,
				COALESCE(BOOL_OR(event.name = 'email.opened' AND event.step_attributed), false) AS opened,
				COALESCE(BOOL_OR(event.name = 'email.clicked' AND event.step_attributed), false) AS clicked,
				COALESCE(BOOL_OR(event.name = 'landing.clicked' AND event.step_attributed), false) AS landing_clicked,
				COALESCE(BOOL_OR(event.name = 'app.opened' AND event.step_attributed), false) AS app_opened,
				COALESCE(BOOL_OR(event.name IN ('connect.first', 'connect.day') AND event.at < sends.engagement_ends_at), false) AS connected,
				COALESCE(BOOL_OR(event.name = 'widget.added' AND event.at < sends.engagement_ends_at), false) AS widget_added,
				COALESCE(BOOL_OR(event.name = 'feedback.submitted' AND event.at < sends.engagement_ends_at), false) AS feedback_submitted,
				COALESCE(BOOL_OR(event.name = 'purchase.completed' AND event.at < sends.engagement_ends_at), false) AS pro_started,
				COALESCE(BOOL_OR(event.name = 'email.bounced' AND event.step_attributed), false) AS bounced,
				COALESCE(BOOL_OR(event.name = 'email.unsubscribed' AND event.step_attributed), false) AS unsubscribed,
				COALESCE(BOOL_OR(event.name = 'email.complained' AND event.step_attributed), false) AS complained,
				COALESCE(BOOL_OR(event.legacy_ambiguous), false) AS attribution_ambiguous
			FROM (
				SELECT
					raw.name,
					raw.at,
					(
						raw.props->>'flow_step' = sends.step OR (
							COALESCE(raw.props->>'flow_step', '') = '' AND
							NOT sends.template_ambiguous AND
							raw.props->>'step' = sends.template
						)
					) AS step_attributed,
					(
						raw.name IN (
							'email.delivered', 'email.opened', 'email.clicked',
							'email.bounced', 'email.unsubscribed', 'email.complained',
							'landing.clicked', 'app.opened'
						) AND
						COALESCE(raw.props->>'flow_step', '') = '' AND
						sends.template_ambiguous AND
						raw.props->>'step' = sends.template AND
						raw.at < sends.legacy_attribution_ends_at
					) AS legacy_ambiguous
				FROM network_onboarding_event AS raw
				WHERE raw.network_id = sends.network_id
					AND raw.at >= sends.sent_at
					AND raw.at < sends.sent_at + INTERVAL '30 days'
					AND raw.name IN (
						'email.delivered', 'email.opened', 'email.clicked',
						'email.bounced', 'email.unsubscribed', 'email.complained',
						'landing.clicked', 'app.opened', 'connect.first',
						'connect.day', 'widget.added', 'feedback.submitted',
						'purchase.completed'
					)
			) AS event
		) AS events
	),
	aggregated AS (
		SELECT
			date_trunc('day', sent_at) AS send_day,
			step, template, variant, experiment, experiment_variant, platform, path,
			COUNT(*) AS sent,
			COUNT(*) FILTER (WHERE delivered) AS delivered,
			COUNT(*) FILTER (WHERE opened) AS opened,
			COUNT(*) FILTER (WHERE clicked) AS clicked,
			COUNT(*) FILTER (WHERE landing_clicked) AS landing_clicked,
			COUNT(*) FILTER (WHERE app_opened) AS app_opened,
			COUNT(*) FILTER (WHERE connected) AS connected,
			COUNT(*) FILTER (WHERE widget_added) AS widget_added,
			COUNT(*) FILTER (WHERE feedback_submitted) AS feedback_submitted,
			COUNT(*) FILTER (WHERE pro_started) AS pro_started,
			COUNT(*) FILTER (WHERE
				clicked OR landing_clicked OR app_opened OR connected OR
				widget_added OR feedback_submitted OR pro_started
			) AS engaged,
			COUNT(*) FILTER (WHERE bounced) AS bounced,
			COUNT(*) FILTER (WHERE unsubscribed) AS unsubscribed,
			COUNT(*) FILTER (WHERE complained) AS complained,
			COUNT(*) FILTER (WHERE attribution_ambiguous) AS attribution_ambiguous
		FROM classified
		GROUP BY send_day, step, template, variant, experiment,
			experiment_variant, platform, path
	)
	INSERT INTO onboarding_email_tracker_daily (
		send_day, step, template, variant, experiment, experiment_variant,
		platform, path, sent, delivered, opened, clicked, landing_clicked,
		app_opened, connected, widget_added, feedback_submitted, pro_started,
		engaged, bounced, unsubscribed, complained, attribution_ambiguous,
		computed_at
	)
	SELECT
		send_day, step, template, variant, experiment, experiment_variant,
		platform, path, sent, delivered, opened, clicked, landing_clicked,
		app_opened, connected, widget_added, feedback_submitted, pro_started,
		engaged, bounced, unsubscribed, complained, attribution_ambiguous, $3
	FROM aggregated
`

// RebuildOnboardingEmailTracker atomically replaces send days [from, to].
func RebuildOnboardingEmailTracker(ctx context.Context, from time.Time, to time.Time, computedAt time.Time) (rows int64, returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("onboarding email tracker rebuild: %v", r)
		}
	}()
	from = onboarding.CohortDay(from)
	to = onboarding.CohortDay(to)
	if to.Before(from) {
		return 0, fmt.Errorf("onboarding email tracker window is empty")
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`DELETE FROM onboarding_email_tracker_daily WHERE send_day >= $1 AND send_day <= $2`,
			from, to,
		))
		tag := server.RaisePgResult(tx.Exec(ctx, onboardingEmailTrackerRebuildSql, from, to.Add(24*time.Hour), computedAt))
		rows = tag.RowsAffected()
	}, server.TxReadCommitted)
	return
}

const onboardingEmailTrackerSelect = `
	SELECT
		send_day, step, template, variant, experiment, experiment_variant,
		platform, path, sent, delivered, opened, clicked, landing_clicked,
		app_opened, connected, widget_added, feedback_submitted, pro_started,
		engaged, bounced, unsubscribed, complained, attribution_ambiguous,
		computed_at
	FROM onboarding_email_tracker_daily
`

func scanOnboardingEmailTrackerRow(result pgx.Rows) *OnboardingEmailTrackerRow {
	r := &OnboardingEmailTrackerRow{}
	server.Raise(result.Scan(
		&r.SendDay, &r.Step, &r.Template, &r.Variant, &r.Experiment,
		&r.ExperimentVariant, &r.Platform, &r.Path, &r.Sent, &r.Delivered,
		&r.Opened, &r.Clicked, &r.LandingClicked, &r.AppOpened, &r.Connected,
		&r.WidgetAdded, &r.FeedbackSubmitted, &r.ProStarted, &r.Engaged,
		&r.Bounced, &r.Unsubscribed, &r.Complained, &r.AttributionAmbiguous,
		&r.ComputedAt,
	))
	r.SendDayStr = r.SendDay.UTC().Format(cohortDayLayout)
	return r
}

type OnboardingEmailTrackerFilter struct {
	From       time.Time
	To         time.Time
	Step       string
	Experiment string
	Platform   string
	Path       string
	MinSent    int
	After      *onboarding.EmailTrackerKey
	Limit      int
}

// ListOnboardingEmailTracker pages aggregate rows in stable key order.
func ListOnboardingEmailTracker(ctx context.Context, filter OnboardingEmailTrackerFilter) (rows []*OnboardingEmailTrackerRow, next *onboarding.EmailTrackerKey) {
	limit := filter.Limit
	if limit <= 0 || 2000 < limit {
		limit = 500
	}
	rows = []*OnboardingEmailTrackerRow{}
	server.Db(ctx, func(conn server.PgConn) {
		args := []any{onboarding.CohortDay(filter.From), onboarding.CohortDay(filter.To), filter.MinSent}
		where := []string{"send_day >= $1", "send_day <= $2", "sent >= $3"}
		add := func(column string, value string) {
			if value == "" {
				return
			}
			args = append(args, value)
			where = append(where, column+" = $"+itoa(len(args)))
		}
		add("step", filter.Step)
		add("experiment", filter.Experiment)
		add("platform", filter.Platform)
		add("path", filter.Path)
		if filter.After != nil {
			day, err := time.Parse(cohortDayLayout, filter.After.SendDay)
			server.Raise(err)
			args = append(args, day, filter.After.Step, filter.After.Template,
				filter.After.Variant, filter.After.Experiment,
				filter.After.ExperimentVariant, filter.After.Platform, filter.After.Path)
			n := len(args)
			where = append(where,
				"(send_day, step, template, variant, experiment, experiment_variant, platform, path) > ("+
					"$"+itoa(n-7)+", $"+itoa(n-6)+", $"+itoa(n-5)+", $"+itoa(n-4)+", "+
					"$"+itoa(n-3)+", $"+itoa(n-2)+", $"+itoa(n-1)+", $"+itoa(n)+")")
		}
		args = append(args, limit+1)
		query := onboardingEmailTrackerSelect + " WHERE " + strings.Join(where, " AND ") +
			" ORDER BY send_day, step, template, variant, experiment, experiment_variant, platform, path" +
			" LIMIT $" + itoa(len(args))
		result, err := conn.Query(ctx, query, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rows = append(rows, scanOnboardingEmailTrackerRow(result))
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

// SumOnboardingEmailTracker returns all five flow steps, including known
// zeroes, for a Prometheus snapshot over [from, to].
func SumOnboardingEmailTracker(ctx context.Context, from time.Time, to time.Time) map[string]*OnboardingEmailTrackerTotals {
	totals := map[string]*OnboardingEmailTrackerTotals{}
	for _, step := range onboarding.FlowSteps() {
		totals[step] = &OnboardingEmailTrackerTotals{Step: step}
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT
				step, SUM(sent), SUM(delivered), SUM(opened), SUM(clicked),
				SUM(landing_clicked), SUM(app_opened), SUM(connected),
				SUM(widget_added), SUM(feedback_submitted), SUM(pro_started),
				SUM(engaged), SUM(bounced), SUM(unsubscribed), SUM(complained),
				SUM(attribution_ambiguous)
			FROM onboarding_email_tracker_daily
			WHERE send_day >= $1 AND send_day <= $2
			GROUP BY step
		`, onboarding.CohortDay(from), onboarding.CohortDay(to))
		server.WithPgResult(result, err, func() {
			for result.Next() {
				t := &OnboardingEmailTrackerTotals{}
				server.Raise(result.Scan(
					&t.Step, &t.Sent, &t.Delivered, &t.Opened, &t.Clicked,
					&t.LandingClicked, &t.AppOpened, &t.Connected, &t.WidgetAdded,
					&t.FeedbackSubmitted, &t.ProStarted, &t.Engaged, &t.Bounced,
					&t.Unsubscribed, &t.Complained, &t.AttributionAmbiguous,
				))
				totals[t.Step] = t
			}
		})
	})
	return totals
}
